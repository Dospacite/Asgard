package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rousoftware/asgard/internal/httpx"
	"github.com/rousoftware/asgard/internal/importer"
	"github.com/rousoftware/asgard/internal/store"
)

const maxGitSecretBytes = 64 << 10

func (s *Server) gitCredentials(w http.ResponseWriter, r *http.Request) {
	items, err := s.Store.ListGitCredentials(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) createGitCredential(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name       string `json:"name"`
		Kind       string `json:"kind"`
		Username   string `json:"username"`
		Host       string `json:"host"`
		Secret     string `json:"secret"`
		Repository string `json:"repository"`
	}
	if !httpx.Decode(w, r, &body) {
		return
	}
	item, secret, err := store.NormalizeGitCredential(body.Name, body.Kind, body.Username, body.Host, body.Secret, body.Repository)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid_credential", err.Error())
		return
	}
	if s.Secrets == nil {
		httpx.Error(w, http.StatusServiceUnavailable, "secrets_unavailable", "Credential storage is unavailable.")
		return
	}
	ciphertext, nonce, err := s.Secrets.Seal(secret)
	if err != nil {
		writeError(w, err)
		return
	}
	created, err := s.Store.CreateGitCredential(r.Context(), item, ciphertext, nonce)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			httpx.Error(w, http.StatusConflict, "duplicate_credential", "A credential with that name already exists.")
			return
		}
		writeError(w, err)
		return
	}
	s.auditRequest(r, "git_credential.create", "git_credential", created.ID, "Stored "+created.Kind+" credential "+created.Name)
	// Prove the credential works now rather than at the moment a release needs
	// it. The credential is stored either way — a verification that fails is
	// information, not a reason to throw the secret away — but the result comes
	// back in the same response so the operator learns immediately.
	//
	// The verification must run before the credential is re-read: it is what
	// writes the fields the response reports. Go does not define the evaluation
	// order of a composite literal's values, so these cannot share one.
	result := s.verify(r, created.ID, body.Repository)
	httpx.JSON(w, http.StatusCreated, map[string]any{"credential": s.verified(r, created.ID), "verification": result})
}

// updateGitCredential rotates a credential in place.
//
// Without this, replacing an expired or leaked token meant minting a new
// credential with a new id and then re-pointing every project at it — and
// nothing could re-point a project either, so the only way to complete a
// rotation was to hand-edit SQLite. Rotating in place keeps the id, so every
// project that already references this credential picks up the new secret with
// no further action.
func (s *Server) updateGitCredential(w http.ResponseWriter, r *http.Request) {
	current, err := s.Store.GetGitCredential(r.Context(), chi.URLParam(r, "credentialID"))
	if err != nil {
		writeError(w, err)
		return
	}
	var body struct {
		Name       string `json:"name"`
		Username   string `json:"username"`
		Host       string `json:"host"`
		Secret     string `json:"secret"`
		Repository string `json:"repository"`
	}
	if !httpx.Decode(w, r, &body) {
		return
	}
	if body.Name == "" {
		body.Name = current.Name
	}
	item, secret, err := store.NormalizeGitCredentialUpdate(current, body.Name, body.Username, body.Host, body.Secret, body.Repository)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid_credential", err.Error())
		return
	}
	var ciphertext, nonce []byte
	if secret != nil {
		if s.Secrets == nil {
			httpx.Error(w, http.StatusServiceUnavailable, "secrets_unavailable", "Credential storage is unavailable.")
			return
		}
		if ciphertext, nonce, err = s.Secrets.Seal(secret); err != nil {
			writeError(w, err)
			return
		}
	}
	updated, err := s.Store.UpdateGitCredential(r.Context(), item, ciphertext, nonce)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "not_found", "Credential not found.")
			return
		}
		if strings.Contains(err.Error(), "UNIQUE") {
			httpx.Error(w, http.StatusConflict, "duplicate_credential", "A credential with that name already exists.")
			return
		}
		writeError(w, err)
		return
	}
	what := "metadata"
	if secret != nil {
		what = "secret"
	}
	s.auditRequest(r, "git_credential.update", "git_credential", updated.ID, "Rotated "+what+" for credential "+updated.Name)
	// Verify before re-reading, for the same reason as on create.
	result := s.verify(r, updated.ID, body.Repository)
	httpx.JSON(w, http.StatusOK, map[string]any{"credential": s.verified(r, updated.ID), "verification": result})
}

// verifyGitCredential re-checks one credential on demand.
func (s *Server) verifyGitCredential(w http.ResponseWriter, r *http.Request) {
	item, err := s.Store.GetGitCredential(r.Context(), chi.URLParam(r, "credentialID"))
	if err != nil {
		writeError(w, err)
		return
	}
	var body struct {
		Repository string `json:"repository"`
	}
	// A bare POST with no body is a valid "re-check against what you know".
	_ = json.NewDecoder(r.Body).Decode(&body)
	result := s.verify(r, item.ID, body.Repository)
	s.auditRequest(r, "git_credential.verify", "git_credential", item.ID, "Verified credential "+item.Name+": "+result.Status)
	httpx.JSON(w, http.StatusOK, map[string]any{"credential": s.verified(r, item.ID), "verification": result})
}

// verify runs one verification, tolerating an importer that cannot reach git at
// all so a missing binary surfaces as a failed check rather than a 500.
func (s *Server) verify(r *http.Request, credentialID, repository string) importer.VerifyResult {
	if s.Importer == nil {
		return importer.VerifyResult{CredentialID: credentialID, Status: importer.VerifySkipped, Error: "Credential verification is unavailable.", CheckedAt: time.Now().UTC()}
	}
	result, err := s.Importer.VerifyCredential(r.Context(), credentialID, repository)
	if err != nil {
		return importer.VerifyResult{CredentialID: credentialID, Status: importer.VerifyFailed, Error: err.Error(), CheckedAt: time.Now().UTC()}
	}
	return result
}

// verified re-reads a credential so the response carries the verification
// fields the check just wrote.
func (s *Server) verified(r *http.Request, id string) store.GitCredential {
	item, err := s.Store.GetGitCredential(r.Context(), id)
	if err != nil {
		return store.GitCredential{ID: id}
	}
	return item
}

func (s *Server) deleteGitCredential(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "credentialID")
	item, err := s.Store.GetGitCredential(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.Store.DeleteGitCredential(r.Context(), item.ID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "not_found", "Credential not found.")
			return
		}
		writeError(w, err)
		return
	}
	s.auditRequest(r, "git_credential.delete", "git_credential", item.ID, "Deleted credential "+item.Name)
	w.WriteHeader(http.StatusNoContent)
}

// verifyAllGitCredentials re-checks every stored credential in one pass, which
// is what the settings page asks for when it loads.
func (s *Server) verifyAllGitCredentials(w http.ResponseWriter, r *http.Request) {
	if s.Importer == nil {
		httpx.Error(w, http.StatusServiceUnavailable, "verification_unavailable", "Credential verification is unavailable.")
		return
	}
	// Each check is bounded, but a long list of them is not. Cap the sweep so a
	// slow or unreachable Git host cannot hold the request open indefinitely;
	// whatever completed is still recorded and returned.
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	results, err := s.Importer.VerifyAllCredentials(ctx)
	// A sweep that ran out of time still checked some credentials, and every
	// result it did get is already recorded. Returning those beats failing the
	// whole request because the last host was slow.
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		writeError(w, err)
		return
	}
	items, err := s.Store.ListGitCredentials(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	s.auditRequest(r, "git_credential.verify", "git_credential", "", "Verified all stored Git credentials")
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items, "verifications": results})
}

// setProjectCredential re-points a project at a different stored credential.
//
// This is the other half of rotation. A replaced token is useless if nothing
// can attach it to the projects that need it, which is why the correct response
// to a leaked secret used to require hand-editing the database.
func (s *Server) setProjectCredential(w http.ResponseWriter, r *http.Request) {
	project, err := s.Store.GetProject(r.Context(), chi.URLParam(r, "projectID"))
	if err != nil {
		writeError(w, err)
		return
	}
	var body struct {
		CredentialID string `json:"credentialId"`
	}
	if !httpx.Decode(w, r, &body) {
		return
	}
	credentialID := ""
	if strings.TrimSpace(body.CredentialID) != "" {
		credential, err := s.Store.GetGitCredential(r.Context(), body.CredentialID)
		if err != nil {
			httpx.Error(w, http.StatusNotFound, "not_found", "Credential not found.")
			return
		}
		credentialID = credential.ID
	}
	if err := s.Store.SetProjectSourceCredential(r.Context(), project.ID, credentialID); err != nil {
		httpx.Error(w, http.StatusConflict, "source_not_git", err.Error())
		return
	}
	s.auditRequest(r, "project.credential.update", "project", project.ID, "Changed the project's source credential")
	updated, _ := s.Store.GetProject(r.Context(), project.ID)
	// Rotation usually happens because the old credential broke, so answer the
	// obvious next question without a second round trip.
	var verification any
	if credentialID != "" {
		verification = s.verify(r, credentialID, updated.SourceURL)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"project": updated, "verification": verification})
}
