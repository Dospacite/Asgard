package api

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/rousoftware/asgard/internal/composecfg"
	"github.com/rousoftware/asgard/internal/httpx"
	"github.com/rousoftware/asgard/internal/importer"
	"github.com/rousoftware/asgard/internal/projectsource"
	"github.com/rousoftware/asgard/internal/store"
)

func (s *Server) projects(w http.ResponseWriter, r *http.Request) {
	items, err := s.Store.ListProjects(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
}
func (s *Server) project(w http.ResponseWriter, r *http.Request) {
	item, err := s.Store.GetProject(r.Context(), chi.URLParam(r, "projectID"))
	if err != nil {
		writeError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, item)
}

func (s *Server) updateProject(w http.ResponseWriter, r *http.Request) {
	project, err := s.Store.GetProject(r.Context(), chi.URLParam(r, "projectID"))
	if err != nil {
		writeError(w, err)
		return
	}
	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if !httpx.Decode(w, r, &body) {
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" || len(body.Name) > 100 || len(body.Description) > 1000 {
		httpx.Error(w, http.StatusBadRequest, "invalid_project", "Name is required and descriptions are limited to 1,000 characters.")
		return
	}
	_, err = s.Store.DB.ExecContext(r.Context(), `UPDATE projects SET name=?,description=?,updated_at=? WHERE id=?`, body.Name, strings.TrimSpace(body.Description), store.Now(), project.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	s.auditRequest(r, "project.update", "project", project.ID, "Updated project settings")
	item, _ := s.Store.GetProject(r.Context(), project.ID)
	httpx.JSON(w, http.StatusOK, item)
}

func (s *Server) sourceFiles(w http.ResponseWriter, r *http.Request) {
	project, err := s.Store.GetProject(r.Context(), chi.URLParam(r, "projectID"))
	if err != nil {
		writeError(w, err)
		return
	}
	workspace, err := projectsource.Load(project)
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, workspace)
}

func (s *Server) updateSourceFile(w http.ResponseWriter, r *http.Request) {
	project, err := s.Store.GetProject(r.Context(), chi.URLParam(r, "projectID"))
	if err != nil {
		writeError(w, err)
		return
	}
	var body struct {
		Path     string `json:"path"`
		Content  string `json:"content"`
		Revision string `json:"revision"`
	}
	if !httpx.Decode(w, r, &body) {
		return
	}
	workspace, err := projectsource.Save(r.Context(), s.Store, project, s.Config.Domain, body.Path, body.Content, body.Revision)
	if err != nil {
		var problem *projectsource.Problem
		if errors.As(err, &problem) {
			status := http.StatusUnprocessableEntity
			if strings.Contains(problem.Code, "revision_conflict") {
				status = http.StatusConflict
			}
			details := map[string]any{}
			if problem.Validation != nil {
				details["validation"] = problem.Validation
			}
			httpx.JSON(w, status, httpx.ErrorBody{Error: httpx.APIError{Code: problem.Code, Message: problem.Message, Details: details}})
			return
		}
		writeError(w, err)
		return
	}
	s.auditRequest(r, "project.source.update", "project", project.ID, "Updated source file "+filepath.ToSlash(filepath.Clean(body.Path)))
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, workspace)
}

// resyncSource re-fetches a Git project's repository so the next deployment
// builds its current head instead of the tree captured at import.
func (s *Server) resyncSource(w http.ResponseWriter, r *http.Request) {
	project, err := s.Store.GetProject(r.Context(), chi.URLParam(r, "projectID"))
	if err != nil {
		writeError(w, err)
		return
	}
	result, err := s.Importer.Resync(r.Context(), project.ID)
	if err != nil {
		if errors.Is(err, importer.ErrNotGitSource) {
			httpx.Error(w, http.StatusConflict, "source_not_git", "Only projects imported from Git can be re-synced. Edit the Compose file directly, or import the project again.")
			return
		}
		var problem *projectsource.Problem
		if errors.As(err, &problem) {
			details := map[string]any{}
			if problem.Validation != nil {
				details["validation"] = problem.Validation
			}
			httpx.JSON(w, http.StatusUnprocessableEntity, httpx.ErrorBody{Error: httpx.APIError{Code: problem.Code, Message: problem.Message, Details: details}})
			return
		}
		httpx.JSON(w, http.StatusUnprocessableEntity, map[string]any{"error": map[string]any{"code": "resync_failed", "message": err.Error()}})
		return
	}
	s.auditRequest(r, "project.source.resync", "project", project.ID, "Re-synced source to "+shortCommit(result.Commit))
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, result)
}

func shortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	if commit == "" {
		return "an unknown revision"
	}
	return commit
}

func (s *Server) importGit(w http.ResponseWriter, r *http.Request) {
	var req importer.Request
	if !httpx.Decode(w, r, &req) {
		return
	}
	project, result, err := s.Importer.FromGit(r.Context(), req)
	if err != nil {
		httpx.JSON(w, http.StatusUnprocessableEntity, map[string]any{"error": map[string]any{"code": "import_failed", "message": err.Error(), "details": result}})
		return
	}
	s.auditRequest(r, "project.import.git", "project", project.ID, "Imported public Git project")
	httpx.JSON(w, http.StatusCreated, map[string]any{"project": project, "validation": result})
}
func (s *Server) importImage(w http.ResponseWriter, r *http.Request) {
	var req importer.Request
	if !httpx.Decode(w, r, &req) {
		return
	}
	project, result, err := s.Importer.FromImage(r.Context(), req)
	if err != nil {
		httpx.JSON(w, http.StatusUnprocessableEntity, map[string]any{"error": map[string]any{"code": "import_failed", "message": err.Error(), "details": result}})
		return
	}
	s.auditRequest(r, "project.import.image", "project", project.ID, "Imported public OCI image")
	httpx.JSON(w, http.StatusCreated, map[string]any{"project": project, "validation": result})
}

func (s *Server) importArchive(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, importer.MaxArchiveCompressed+2<<20)
	if err := r.ParseMultipartForm(importer.MaxArchiveCompressed + 1<<20); err != nil {
		httpx.Error(w, http.StatusRequestEntityTooLarge, "upload_too_large", "Archive uploads are limited to 100 MiB.")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "file_required", "An archive file is required.")
		return
	}
	defer file.Close()
	if !importer.HasArchiveExtension(header.Filename) {
		httpx.Error(w, http.StatusBadRequest, "invalid_file", "Upload one of: "+strings.Join(importer.ArchiveExtensions, ", ")+".")
		return
	}
	tmp, err := os.CreateTemp(filepath.Join(s.Config.DataDir, "uploads"), "upload-*.archive")
	if err != nil {
		writeError(w, err)
		return
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)
	if err := saveExisting(tmpPath, file, importer.MaxArchiveCompressed); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid_upload", err.Error())
		return
	}
	req := importer.Request{Name: r.FormValue("name"), Slug: r.FormValue("slug"), Description: r.FormValue("description"), ComposePath: r.FormValue("composePath")}
	project, result, err := s.Importer.FromArchive(r.Context(), req, tmpPath, header.Filename)
	if err != nil {
		httpx.JSON(w, http.StatusUnprocessableEntity, map[string]any{"error": map[string]any{"code": "import_failed", "message": err.Error(), "details": result}})
		return
	}
	s.auditRequest(r, "project.import.archive", "project", project.ID, "Imported "+project.SourceType+" archive project")
	httpx.JSON(w, http.StatusCreated, map[string]any{"project": project, "validation": result})
}

func saveExisting(path string, reader io.Reader, max int64) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	written, err := io.CopyN(file, reader, max+1)
	if err != nil && err != io.EOF {
		return err
	}
	if written > max {
		return fmt.Errorf("upload exceeds %d bytes", max)
	}
	return nil
}

func (s *Server) composeContract(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, http.StatusOK, composecfg.Contract)
}
func (s *Server) validateCompose(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Content string `json:"content"`
		Slug    string `json:"slug"`
	}
	if !httpx.Decode(w, r, &body) {
		return
	}
	slug := composecfg.Slug(body.Slug)
	if slug == "" {
		slug = "preview"
	}
	_, result := composecfg.Parse([]byte(body.Content), uuid.NewString(), slug, "")
	status := http.StatusOK
	if !result.Valid {
		status = http.StatusUnprocessableEntity
	}
	httpx.JSON(w, status, result)
}

func (s *Server) deployments(w http.ResponseWriter, r *http.Request) {
	items, err := s.Store.ListDeployments(r.Context(), chi.URLParam(r, "projectID"), parseLimit(r, 50, 200))
	if err != nil {
		writeError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) createDeployment(w http.ResponseWriter, r *http.Request) {
	project, err := s.Store.GetProject(r.Context(), chi.URLParam(r, "projectID"))
	if err != nil {
		writeError(w, err)
		return
	}
	actor := identity(r)
	op := store.Operation{ID: uuid.NewString(), Kind: "deployment.create", TargetType: "project", TargetID: project.ID, Summary: "Deploy " + project.Name, RequestedBy: actor.UserID, Payload: payload(map[string]any{"trigger": "manual"})}
	op, err = s.Store.CreateOperation(r.Context(), op, idempotency(r))
	if err != nil {
		writeError(w, err)
		return
	}
	s.Operations.Enqueue(op.ID)
	s.auditRequest(r, "deployment.create", "project", project.ID, "Queued deployment")
	httpx.JSON(w, http.StatusAccepted, op)
}

func (s *Server) rollback(w http.ResponseWriter, r *http.Request) {
	project, err := s.Store.GetProject(r.Context(), chi.URLParam(r, "projectID"))
	if err != nil {
		writeError(w, err)
		return
	}
	var body struct {
		ReleaseID string `json:"releaseId"`
	}
	if r.ContentLength > 0 && !httpx.Decode(w, r, &body) {
		return
	}
	if body.ReleaseID == "" {
		latest, latestErr := s.Store.LatestSuccessfulRelease(r.Context(), project.ID)
		if latestErr != nil {
			if latestErr == sql.ErrNoRows {
				httpx.Error(w, http.StatusConflict, "no_release", "No successful release is available.")
				return
			}
			writeError(w, latestErr)
			return
		}
		previous, previousErr := s.Store.PreviousSuccessfulRelease(r.Context(), project.ID, latest.ID)
		if previousErr != nil {
			httpx.Error(w, http.StatusConflict, "no_previous_release", "No earlier successful release is available.")
			return
		}
		body.ReleaseID = previous.ID
	}
	actor := identity(r)
	op := store.Operation{ID: uuid.NewString(), Kind: "deployment.rollback", TargetType: "project", TargetID: project.ID, Summary: "Roll back " + project.Name, RequestedBy: actor.UserID, Payload: payload(map[string]any{"trigger": "rollback", "rollbackReleaseId": body.ReleaseID})}
	op, err = s.Store.CreateOperation(r.Context(), op, idempotency(r))
	if err != nil {
		writeError(w, err)
		return
	}
	s.Operations.Enqueue(op.ID)
	s.auditRequest(r, "deployment.rollback", "project", project.ID, "Queued rollback")
	httpx.JSON(w, http.StatusAccepted, op)
}

func (s *Server) releases(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.DB.QueryContext(r.Context(), `SELECT id FROM releases WHERE project_id=? ORDER BY version DESC LIMIT ?`, chi.URLParam(r, "projectID"), parseLimit(r, 20, 100))
	if err != nil {
		writeError(w, err)
		return
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			writeError(w, err)
			return
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		writeError(w, err)
		return
	}
	if err := rows.Close(); err != nil {
		writeError(w, err)
		return
	}
	items := make([]store.Release, 0, len(ids))
	for _, id := range ids {
		release, err := s.Store.GetRelease(r.Context(), id)
		if err != nil {
			writeError(w, err)
			return
		}
		items = append(items, release)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) createWebhook(w http.ResponseWriter, r *http.Request) {
	httpx.Error(w, http.StatusNotImplemented, "not_implemented", "Webhook management is reserved for the next release.")
}

func parseRevision(r *http.Request, bodyRevision int) (int, error) {
	if bodyRevision > 0 {
		return bodyRevision, nil
	}
	value := strings.Trim(r.Header.Get("If-Match"), "\"")
	if value == "" {
		return 0, fmt.Errorf("If-Match or configRevision is required")
	}
	return strconv.Atoi(value)
}
