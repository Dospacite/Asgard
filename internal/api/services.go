package api

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
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
		HSTSMode       string            `json:"hstsMode"`
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
	settings := composecfg.ServiceSettings{Role: body.Role, Environment: body.Environment, Public: body.Public, Port: body.Port, Hostname: body.Hostname, HealthPath: body.HealthPath, HSTSMode: body.HSTSMode, CPULimit: body.CPULimit, MemoryLimit: body.MemoryLimit, PIDsLimit: body.PIDsLimit, RestartPolicy: body.RestartPolicy}
	if err := settings.Normalize(s.Config.Domain); err != nil {
		writeSettingsError(w, err)
		return
	}
	current.Role = settings.Role
	current.Environment = settings.Environment
	current.Public = settings.Public
	current.Port = settings.Port
	current.Hostname = settings.Hostname
	current.HealthPath = settings.HealthPath
	current.HSTSMode = settings.HSTSMode
	current.CPULimit = settings.CPULimit
	current.MemoryLimit = settings.MemoryLimit
	current.PIDsLimit = settings.PIDsLimit
	current.RestartPolicy = settings.RestartPolicy
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
			// A live read has no previous sample to difference against, so its
			// throttling share is the container's lifetime figure. The stored
			// history below carries the per-interval share.
			service.Metrics = &store.Metrics{CPUPercent: live.CPUPercent, MemoryBytes: live.MemoryBytes, MemoryLimit: live.MemoryLimit, NetworkRX: live.NetworkRX, NetworkTX: live.NetworkTX, BlockRead: live.BlockRead, BlockWrite: live.BlockWrite, PIDs: live.PIDs, CPUThrottledPercent: live.ThrottledPercent, CPUPeriods: live.CPUPeriods, CPUThrottledPeriods: live.CPUThrottledPeriods, CPUThrottledNanos: live.CPUThrottledNanos, CollectedAt: live.CollectedAt}
		}
	}
	since := time.Now().UTC().Add(-2 * time.Hour)
	if raw := r.URL.Query().Get("since"); raw != "" {
		if parsed, parseErr := time.Parse(time.RFC3339, raw); parseErr == nil {
			since = parsed
		}
	}
	history, err := s.Store.RecentMetrics(r.Context(), service.ID, since, 1000)
	if err != nil {
		writeError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"current": service.Metrics, "history": history, "limits": map[string]any{"cpu": service.CPULimit, "memoryBytes": service.MemoryLimit, "pids": service.PIDsLimit}, "throttling": store.ThrottlingSummary(history, service.CPULimit)})
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

// writeSettingsError maps a shared validation failure onto its HTTP status.
// The mapping lives with the transport; the rules themselves live in
// composecfg so the MCP server enforces exactly the same ones.
func writeSettingsError(w http.ResponseWriter, err error) {
	var settings *composecfg.SettingsError
	if !errors.As(err, &settings) {
		writeError(w, err)
		return
	}
	status := http.StatusBadRequest
	if settings.Code == "reserved_hostname" {
		status = http.StatusConflict
	}
	httpx.Error(w, status, settings.Code, settings.Message)
}
