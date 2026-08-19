package api

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rousoftware/asgard/internal/composecfg"
	"github.com/rousoftware/asgard/internal/httpx"
	"github.com/rousoftware/asgard/internal/store"
)

func (s *Server) service(w http.ResponseWriter, r *http.Request) {
	item, err := s.Store.GetService(r.Context(), chi.URLParam(r, "serviceID"))
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("ETag", "\""+strconv.Itoa(item.ConfigRevision)+"\"")
	httpx.JSON(w, http.StatusOK, item)
}

func (s *Server) updateService(w http.ResponseWriter, r *http.Request) {
	current, err := s.Store.GetService(r.Context(), chi.URLParam(r, "serviceID"))
	if err != nil {
		writeError(w, err)
		return
	}
	var body struct {
		Role           string            `json:"role"`
		Environment    map[string]string `json:"environment"`
		Public         bool              `json:"public"`
		Port           int               `json:"port"`
		Hostname       string            `json:"hostname"`
		HealthPath     string            `json:"healthPath"`
		CPULimit       float64           `json:"cpuLimit"`
		MemoryLimit    int64             `json:"memoryLimit"`
		PIDsLimit      int64             `json:"pidsLimit"`
		RestartPolicy  string            `json:"restartPolicy"`
		ConfigRevision int               `json:"configRevision"`
	}
	if !httpx.Decode(w, r, &body) {
		return
	}
	revision, err := parseRevision(r, body.ConfigRevision)
	if err != nil {
		httpx.Error(w, http.StatusPreconditionRequired, "revision_required", err.Error())
		return
	}
	if body.Role != "web" && body.Role != "worker" && body.Role != "stateful" {
		httpx.Error(w, http.StatusBadRequest, "invalid_role", "Role must be web, worker, or stateful.")
		return
	}
	if body.CPULimit < 0.05 || body.CPULimit > 64 || body.MemoryLimit < 32<<20 || body.MemoryLimit > 256<<30 || body.PIDsLimit < 16 || body.PIDsLimit > 32768 {
		httpx.Error(w, http.StatusBadRequest, "invalid_resources", "Resource limits are outside the supported range.")
		return
	}
	if body.RestartPolicy != "no" && body.RestartPolicy != "always" && body.RestartPolicy != "on-failure" && body.RestartPolicy != "unless-stopped" {
		httpx.Error(w, http.StatusBadRequest, "invalid_restart_policy", "Unsupported restart policy.")
		return
	}
	if err := composecfg.ValidateEnvironment(body.Environment); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid_environment", err.Error())
		return
	}
	body.Hostname = strings.ToLower(strings.TrimSpace(body.Hostname))
	if body.Public {
		if body.Port < 1 || body.Port > 65535 {
			httpx.Error(w, http.StatusBadRequest, "invalid_port", "A public service needs a valid internal port.")
			return
		}
		switch err := composecfg.ValidatePublicHostname(body.Hostname, s.Config.Domain); {
		case errors.Is(err, composecfg.ErrHostnameReserved):
			httpx.Error(w, http.StatusConflict, "reserved_hostname", "The control-plane hostname is reserved.")
			return
		case err != nil:
			httpx.Error(w, http.StatusBadRequest, "invalid_hostname", "Hostname must be a fully qualified DNS name, such as app.example.com, whose DNS points at this host.")
			return
		}
	}
	if body.HealthPath == "" {
		body.HealthPath = "/"
	}
	if !strings.HasPrefix(body.HealthPath, "/") {
		httpx.Error(w, http.StatusBadRequest, "invalid_health_path", "Health path must begin with /.")
		return
	}
	current.Role = body.Role
	current.Environment = body.Environment
	current.Public = body.Public
	current.Port = body.Port
	current.Hostname = body.Hostname
	current.HealthPath = body.HealthPath
	current.CPULimit = body.CPULimit
	current.MemoryLimit = body.MemoryLimit
	current.PIDsLimit = body.PIDsLimit
	current.RestartPolicy = body.RestartPolicy
	if err := s.Store.UpdateService(r.Context(), current, revision); err != nil {
		httpx.Error(w, http.StatusConflict, "revision_conflict", err.Error())
		return
	}
	s.auditRequest(r, "service.update", "service", current.ID, "Updated service configuration")
	item, _ := s.Store.GetService(r.Context(), current.ID)
	w.Header().Set("ETag", "\""+strconv.Itoa(item.ConfigRevision)+"\"")
	httpx.JSON(w, http.StatusOK, item)
}

func (s *Server) serviceStats(w http.ResponseWriter, r *http.Request) {
	service, err := s.Store.GetService(r.Context(), chi.URLParam(r, "serviceID"))
	if err != nil {
		writeError(w, err)
		return
	}
	if service.Runtime != nil && service.Runtime.State == "running" {
		if live, liveErr := s.Docker.Stats(r.Context(), service.Runtime.DockerID); liveErr == nil {
			service.Metrics = &store.Metrics{CPUPercent: live.CPUPercent, MemoryBytes: live.MemoryBytes, MemoryLimit: live.MemoryLimit, NetworkRX: live.NetworkRX, NetworkTX: live.NetworkTX, BlockRead: live.BlockRead, BlockWrite: live.BlockWrite, PIDs: live.PIDs, CollectedAt: live.CollectedAt}
		}
	}
	since := time.Now().UTC().Add(-2 * time.Hour)
	if raw := r.URL.Query().Get("since"); raw != "" {
		if parsed, parseErr := time.Parse(time.RFC3339, raw); parseErr == nil {
			since = parsed
		}
	}
	rows, err := s.Store.DB.QueryContext(r.Context(), `SELECT cpu_percent,memory_bytes,memory_limit,network_rx,network_tx,block_read,block_write,pids,collected_at FROM metrics WHERE service_id=? AND collected_at>=? ORDER BY collected_at LIMIT 1000`, service.ID, since.Format(time.RFC3339Nano))
	if err != nil {
		writeError(w, err)
		return
	}
	defer rows.Close()
	history := []store.Metrics{}
	for rows.Next() {
		var m store.Metrics
		var collected string
		if err := rows.Scan(&m.CPUPercent, &m.MemoryBytes, &m.MemoryLimit, &m.NetworkRX, &m.NetworkTX, &m.BlockRead, &m.BlockWrite, &m.PIDs, &collected); err != nil {
			writeError(w, err)
			return
		}
		m.CollectedAt, _ = time.Parse(time.RFC3339Nano, collected)
		history = append(history, m)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"current": service.Metrics, "history": history, "limits": map[string]any{"cpu": service.CPULimit, "memoryBytes": service.MemoryLimit, "pids": service.PIDsLimit}})
}

func (s *Server) serviceLogs(w http.ResponseWriter, r *http.Request) {
	service, err := s.Store.GetService(r.Context(), chi.URLParam(r, "serviceID"))
	if err != nil {
		writeError(w, err)
		return
	}
	if service.Runtime == nil {
		httpx.JSON(w, http.StatusOK, map[string]any{"content": "", "available": false})
		return
	}
	tail := parseLimit(r, 500, 5000)
	content, err := s.Docker.Logs(r.Context(), service.Runtime.DockerID, tail, r.URL.Query().Get("since"))
	if err != nil {
		writeError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"content": content, "available": true, "containerId": service.Runtime.DockerID})
}

func (s *Server) serviceAction(w http.ResponseWriter, r *http.Request) {
	service, err := s.Store.GetService(r.Context(), chi.URLParam(r, "serviceID"))
	if err != nil {
		writeError(w, err)
		return
	}
	if service.Runtime == nil {
		httpx.Error(w, http.StatusConflict, "not_deployed", "Service has no active container.")
		return
	}
	action := chi.URLParam(r, "action")
	if err := s.Docker.Action(r.Context(), service.Runtime.DockerID, action); err != nil {
		httpx.Error(w, http.StatusBadGateway, "docker_error", err.Error())
		return
	}
	_ = s.Store.UpdateRuntimeState(r.Context(), service.Runtime.DockerID, map[string]string{"start": "running", "stop": "exited", "restart": "running"}[action])
	s.auditRequest(r, "service."+action, "service", service.ID, "Container action: "+action)
	w.WriteHeader(http.StatusNoContent)
}

func latestRuntimeID(db *sql.DB, serviceID string) (string, error) {
	var id string
	err := db.QueryRow(`SELECT docker_id FROM runtime_containers WHERE service_id=? AND active=1`, serviceID).Scan(&id)
	return id, err
}
