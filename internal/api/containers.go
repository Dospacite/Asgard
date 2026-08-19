package api

import (
	"context"
	"net/http"
	"regexp"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/rousoftware/asgard/internal/composecfg"
	"github.com/rousoftware/asgard/internal/dockerx"
	"github.com/rousoftware/asgard/internal/httpx"
	"github.com/rousoftware/asgard/internal/store"
)

func (s *Server) containers(w http.ResponseWriter, r *http.Request) {
	items, err := s.Docker.Containers(r.Context(), true)
	if err != nil {
		httpx.Error(w, http.StatusServiceUnavailable, "docker_unavailable", err.Error())
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
}
func (s *Server) unmanagedContainers(w http.ResponseWriter, r *http.Request) {
	items, err := s.Docker.Containers(r.Context(), true)
	if err != nil {
		httpx.Error(w, http.StatusServiceUnavailable, "docker_unavailable", err.Error())
		return
	}
	unmanaged := items[:0]
	for _, item := range items {
		if !item.Managed {
			unmanaged = append(unmanaged, item)
		}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": unmanaged})
}
func (s *Server) adoptionPreview(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "containerID")
	item, err := s.Docker.InspectAdoption(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	if item.Container.Managed {
		httpx.Error(w, http.StatusConflict, "already_managed", "Container is already managed by Asgard.")
		return
	}
	httpx.JSON(w, http.StatusOK, item)
}

var serviceNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)

func (s *Server) adoptContainer(w http.ResponseWriter, r *http.Request) {
	containerID := chi.URLParam(r, "containerID")
	var body struct {
		ProjectID   string `json:"projectId"`
		ServiceName string `json:"serviceName"`
		Role        string `json:"role"`
		Public      bool   `json:"public"`
		Port        int    `json:"port"`
		Hostname    string `json:"hostname"`
		HealthPath  string `json:"healthPath"`
	}
	if !httpx.Decode(w, r, &body) {
		return
	}
	project, err := s.Store.GetProject(r.Context(), body.ProjectID)
	if err != nil {
		writeError(w, err)
		return
	}
	if !serviceNamePattern.MatchString(body.ServiceName) {
		httpx.Error(w, http.StatusBadRequest, "invalid_service_name", "Service name is invalid.")
		return
	}
	if body.Role == "" {
		body.Role = "web"
	}
	if err := composecfg.ValidateRole(body.Role); err != nil {
		writeSettingsError(w, err)
		return
	}
	body.Hostname, err = composecfg.NormalizeRoute(body.Public, body.Port, body.Hostname, s.Config.Domain)
	if err != nil {
		writeSettingsError(w, err)
		return
	}
	body.HealthPath, err = composecfg.NormalizeHealthPath(body.HealthPath)
	if err != nil {
		writeSettingsError(w, err)
		return
	}
	serviceID := uuid.NewString()
	result, err := s.Docker.Adopt(r.Context(), containerID, dockerx.AdoptionRequest{ProjectID: project.ID, ProjectSlug: project.Slug, ServiceID: serviceID, ServiceName: body.ServiceName})
	if err != nil {
		httpx.Error(w, http.StatusUnprocessableEntity, "adoption_failed", err.Error())
		return
	}
	svc := store.Service{ID: serviceID, ProjectID: project.ID, Name: body.ServiceName, Role: body.Role, Image: result.Image, Command: result.Command, Environment: result.Environment, Public: body.Public, Port: body.Port, Hostname: body.Hostname, HealthPath: body.HealthPath, CPULimit: float64(result.NanoCPUs) / 1e9, MemoryLimit: result.Memory, PIDsLimit: result.PIDs, RestartPolicy: "unless-stopped", Volumes: result.Volumes}
	if svc.CPULimit <= 0 {
		svc.CPULimit = .5
	}
	if svc.MemoryLimit <= 0 {
		svc.MemoryLimit = 512 << 20
	}
	rollback := func() {
		_ = s.Docker.RollbackAdoption(context.WithoutCancel(r.Context()), result)
		_, _ = s.Store.DB.ExecContext(context.WithoutCancel(r.Context()), `DELETE FROM services WHERE id=?`, serviceID)
	}
	if err = s.Store.AddService(r.Context(), svc); err != nil {
		_ = s.Docker.RollbackAdoption(r.Context(), result)
		writeError(w, err)
		return
	}
	if body.Public {
		_ = s.Docker.EnsureNetwork(r.Context(), s.Config.EdgeNetwork, false, map[string]string{dockerx.LabelManaged: "true"})
		if err = s.Docker.ConnectNetwork(r.Context(), s.Config.EdgeNetwork, result.Replacement.ID, result.Replacement.Name); err != nil {
			rollback()
			writeError(w, err)
			return
		}
	}
	now := store.Now()
	_, err = s.Store.DB.ExecContext(r.Context(), `INSERT INTO runtime_containers(id,project_id,service_id,docker_id,docker_name,image_id,state,active,created_at,updated_at) VALUES(?,?,?,?,?,?,?,1,?,?)`, uuid.NewString(), project.ID, serviceID, result.Replacement.ID, result.Replacement.Name, result.Replacement.ImageID, result.Replacement.State, now, now)
	if err == nil && body.Public {
		_, err = s.Store.DB.ExecContext(r.Context(), `INSERT INTO routes(id,project_id,service_id,hostname,target_port,tls,created_at,updated_at) VALUES(?,?,?,?,?,1,?,?)`, uuid.NewString(), project.ID, serviceID, body.Hostname, body.Port, now, now)
	}
	if err != nil {
		rollback()
		writeError(w, err)
		return
	}
	_, _ = s.Store.DB.ExecContext(r.Context(), `INSERT INTO adoption_snapshots(id,project_id,service_id,original_container_id,original_container_name,replacement_container_id,expires_at,created_at) VALUES(?,?,?,?,?,?,?,?)`, uuid.NewString(), project.ID, serviceID, result.OriginalID, result.OriginalRetainedName, result.Replacement.ID, result.RollbackUntil.Format(time.RFC3339Nano), now)
	_ = s.Proxy.Write(r.Context())
	s.auditRequest(r, "container.adopt", "container", containerID, "Adopted container into "+project.Name+" as "+body.ServiceName)
	created, _ := s.Store.GetService(r.Context(), serviceID)
	httpx.JSON(w, http.StatusCreated, map[string]any{"service": created, "originalRetainedUntil": result.RollbackUntil})
}
