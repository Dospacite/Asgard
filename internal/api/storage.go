package api

import (
	"database/sql"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/rousoftware/asgard/internal/httpx"
	"github.com/rousoftware/asgard/internal/store"
)

func (s *Server) volumes(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.DB.QueryContext(r.Context(), `SELECT v.id,v.project_id,v.service_id,v.name,v.mount_path,v.created_at,p.name,COALESCE(s.name,'') FROM volumes v JOIN projects p ON p.id=v.project_id LEFT JOIN services s ON s.id=v.service_id ORDER BY p.name,v.name`)
	if err != nil {
		writeError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, projectID, name, mountPath, created, projectName, serviceName string
		var serviceID sql.NullString
		if err := rows.Scan(&id, &projectID, &serviceID, &name, &mountPath, &created, &projectName, &serviceName); err != nil {
			writeError(w, err)
			return
		}
		items = append(items, map[string]any{"id": id, "projectId": projectID, "serviceId": serviceID.String, "name": name, "mountPath": mountPath, "projectName": projectName, "serviceName": serviceName, "createdAt": created})
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
}
func (s *Server) backups(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.DB.QueryContext(r.Context(), `SELECT id,project_id,volume_id,operation_id,kind,status,path,size_bytes,sha256,error,created_at,completed_at FROM backups ORDER BY created_at DESC LIMIT ?`, parseLimit(r, 100, 500))
	if err != nil {
		writeError(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, operationID, kind, status, path, hash, errorMessage, created string
		var projectID, volumeID, completed sql.NullString
		var size int64
		if err := rows.Scan(&id, &projectID, &volumeID, &operationID, &kind, &status, &path, &size, &hash, &errorMessage, &created, &completed); err != nil {
			writeError(w, err)
			return
		}
		items = append(items, map[string]any{"id": id, "projectId": projectID.String, "volumeId": volumeID.String, "operationId": operationID, "kind": kind, "status": status, "path": path, "sizeBytes": size, "sha256": hash, "error": errorMessage, "createdAt": created, "completedAt": completed.String})
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
}
func (s *Server) createBackup(w http.ResponseWriter, r *http.Request) {
	volumeID := chi.URLParam(r, "volumeID")
	var projectID, name string
	if err := s.Store.DB.QueryRowContext(r.Context(), `SELECT project_id,name FROM volumes WHERE id=?`, volumeID).Scan(&projectID, &name); err != nil {
		writeError(w, err)
		return
	}
	actor := identity(r)
	backupID := uuid.NewString()
	op := store.Operation{ID: uuid.NewString(), Kind: "backup.create", TargetType: "volume", TargetID: volumeID, Summary: "Back up " + name, RequestedBy: actor.UserID, Payload: payload(map[string]any{"backupId": backupID, "volumeId": volumeID})}
	op, err := s.Store.CreateOperation(r.Context(), op, idempotency(r))
	if err != nil {
		writeError(w, err)
		return
	}
	if _, err = s.Store.DB.ExecContext(r.Context(), `INSERT INTO backups(id,project_id,volume_id,operation_id,kind,status,created_at) VALUES(?,?,?,?, 'manual','queued',?)`, backupID, projectID, volumeID, op.ID, store.Now()); err != nil {
		writeError(w, err)
		return
	}
	s.Operations.Enqueue(op.ID)
	s.auditRequest(r, "backup.create", "volume", volumeID, "Queued manual volume backup")
	httpx.JSON(w, http.StatusAccepted, map[string]any{"operation": op, "backupId": backupID})
}
func (s *Server) restoreBackup(w http.ResponseWriter, r *http.Request) {
	backupID := chi.URLParam(r, "backupID")
	var volumeID, name, status string
	if err := s.Store.DB.QueryRowContext(r.Context(), `SELECT b.volume_id,v.name,b.status FROM backups b JOIN volumes v ON v.id=b.volume_id WHERE b.id=?`, backupID).Scan(&volumeID, &name, &status); err != nil {
		writeError(w, err)
		return
	}
	if status != "succeeded" {
		httpx.Error(w, http.StatusConflict, "backup_not_ready", "Only a successful backup can be restored.")
		return
	}
	var body struct {
		Confirm string `json:"confirm"`
	}
	if !httpx.Decode(w, r, &body) {
		return
	}
	if body.Confirm != name {
		httpx.Error(w, http.StatusBadRequest, "confirmation_mismatch", "Type the exact volume name to restore it.")
		return
	}
	actor := identity(r)
	op := store.Operation{ID: uuid.NewString(), Kind: "backup.restore", TargetType: "volume", TargetID: volumeID, Summary: "Restore " + name, RequestedBy: actor.UserID, Payload: payload(map[string]any{"backupId": backupID, "volumeId": volumeID})}
	op, err := s.Store.CreateOperation(r.Context(), op, idempotency(r))
	if err != nil {
		writeError(w, err)
		return
	}
	s.Operations.Enqueue(op.ID)
	s.auditRequest(r, "backup.restore", "backup", backupID, "Queued volume restore with pre-restore snapshot")
	httpx.JSON(w, http.StatusAccepted, op)
}
