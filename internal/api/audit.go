package api

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/rousoftware/asgard/internal/auth"
	"github.com/rousoftware/asgard/internal/httpx"
	"github.com/rousoftware/asgard/internal/store"
)

func (s *Server) audit(w http.ResponseWriter, r *http.Request) {
	items, err := s.Store.ListAudit(r.Context(), parseLimit(r, 100, 500))
	if err != nil {
		writeError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) deletionPreview(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TargetType string `json:"targetType"`
		TargetID   string `json:"targetId"`
	}
	if !httpx.Decode(w, r, &body) {
		return
	}
	preview := map[string]any{"targetType": body.TargetType, "targetId": body.TargetID}
	switch body.TargetType {
	case "project":
		project, err := s.Store.GetProject(r.Context(), body.TargetID)
		if err != nil {
			writeError(w, err)
			return
		}
		preview["name"] = project.Name
		preview["slug"] = project.Slug
		preview["services"] = len(project.Services)
		var volumes int
		_ = s.Store.DB.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM volumes WHERE project_id=?`, project.ID).Scan(&volumes)
		preview["volumes"] = volumes
		preview["willDeleteSource"] = true
		preview["willDeleteVolumes"] = false
	case "container":
		item, err := s.Docker.Container(r.Context(), body.TargetID)
		if err != nil {
			writeError(w, err)
			return
		}
		preview["name"] = item.Name
		preview["managed"] = item.Managed
		preview["willDeleteVolumes"] = false
	case "network":
		item, err := s.Networks.Get(r.Context(), body.TargetID)
		if err != nil {
			writeError(w, err)
			return
		}
		if len(item.Members) > 0 {
			httpx.Error(w, http.StatusConflict, "network_not_empty", "Disconnect every service before deleting this network.")
			return
		}
		preview["name"] = item.Name
		preview["slug"] = item.Slug
		preview["members"] = 0
		preview["internal"] = item.Internal
		preview["willDeleteDockerNetwork"] = true
	default:
		httpx.Error(w, http.StatusBadRequest, "unsupported_target", "Only project, container, and network deletion are supported.")
		return
	}
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	token := base64.RawURLEncoding.EncodeToString(raw)
	actor := identity(r)
	id := base64.RawURLEncoding.EncodeToString(raw[:12])
	previewJSON, _ := json.Marshal(preview)
	expires := time.Now().UTC().Add(5 * time.Minute)
	_, err := s.Store.DB.ExecContext(r.Context(), `INSERT INTO deletion_intents(id,token_hash,target_type,target_id,actor_id,preview_json,expires_at,created_at) VALUES(?,?,?,?,?,?,?,?)`, id, auth.HashToken(token), body.TargetType, body.TargetID, actor.UserID, string(previewJSON), expires.Format(time.RFC3339Nano), store.Now())
	if err != nil {
		writeError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"token": token, "expiresAt": expires, "preview": preview})
}

func (s *Server) deletionConfirm(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if !httpx.Decode(w, r, &body) {
		return
	}
	actor := identity(r)
	var id, targetType, targetID, expires string
	var used *string
	err := s.Store.DB.QueryRowContext(r.Context(), `SELECT id,target_type,target_id,expires_at,used_at FROM deletion_intents WHERE token_hash=? AND actor_id=?`, auth.HashToken(body.Token), actor.UserID).Scan(&id, &targetType, &targetID, &expires, &used)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid_deletion_token", "Deletion token is invalid.")
		return
	}
	expiry, _ := time.Parse(time.RFC3339Nano, expires)
	if used != nil || time.Now().After(expiry) {
		httpx.Error(w, http.StatusBadRequest, "expired_deletion_token", "Deletion token expired or was already used.")
		return
	}
	result, err := s.Store.DB.ExecContext(r.Context(), `UPDATE deletion_intents SET used_at=? WHERE id=? AND used_at IS NULL`, store.Now(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		httpx.Error(w, http.StatusConflict, "deletion_token_used", "Deletion token was already used.")
		return
	}
	switch targetType {
	case "project":
		project, err := s.Store.GetProject(r.Context(), targetID)
		if err != nil {
			writeError(w, err)
			return
		}
		runtimes, _ := s.Store.ActiveRuntimes(r.Context(), project.ID)
		for _, runtime := range runtimes {
			if err := s.Docker.Remove(r.Context(), runtime.DockerID, false); err != nil {
				httpx.Error(w, http.StatusBadGateway, "docker_error", "Could not remove "+runtime.DockerName+": "+err.Error())
				return
			}
		}
		if err := s.Docker.RemoveProjectNetwork(r.Context(), "asgard-project-"+project.Slug, project.ID); err != nil {
			httpx.Error(w, http.StatusBadGateway, "docker_error", "Could not remove the project network: "+err.Error())
			return
		}
		if _, err := s.Store.DB.ExecContext(r.Context(), `DELETE FROM projects WHERE id=?`, project.ID); err != nil {
			writeError(w, err)
			return
		}
		if err := os.RemoveAll(project.SourcePath); err != nil {
			writeError(w, err)
			return
		}
		_ = s.Proxy.Write(r.Context())
		// Every image this project ever built is now unreachable: the retention
		// policy is keyed on a project row that no longer exists, so nothing
		// else would ever collect them. Deleting a project used to leave
		// gigabytes behind for exactly this reason.
		summary := "Deleted project; named volumes retained"
		if s.Reclaimer != nil {
			if freed, reclaimErr := s.Reclaimer.ForgetProject(r.Context(), project.Slug); reclaimErr == nil && freed.ImagesRemoved > 0 {
				summary += "; " + freed.Summary()
			}
		}
		s.auditRequest(r, "project.delete", "project", project.ID, summary)
	case "container":
		if err := s.Docker.Remove(r.Context(), targetID, false); err != nil {
			httpx.Error(w, http.StatusBadGateway, "docker_error", err.Error())
			return
		}
		s.auditRequest(r, "container.delete", "container", targetID, "Deleted container; volumes retained")
	case "network":
		if err := s.Networks.Delete(r.Context(), targetID); err != nil {
			if strings.Contains(err.Error(), "disconnect all") {
				httpx.Error(w, http.StatusConflict, "network_not_empty", err.Error())
				return
			}
			httpx.Error(w, http.StatusBadGateway, "network_delete_failed", err.Error())
			return
		}
		s.auditRequest(r, "network.delete", "network", targetID, "Deleted empty shared network")
	}
	w.WriteHeader(http.StatusNoContent)
}
