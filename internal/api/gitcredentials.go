package api

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/rousoftware/asgard/internal/httpx"
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
		Name     string `json:"name"`
		Kind     string `json:"kind"`
		Username string `json:"username"`
		Host     string `json:"host"`
		Secret   string `json:"secret"`
	}
	if !httpx.Decode(w, r, &body) {
		return
	}
	item, secret, err := store.NormalizeGitCredential(body.Name, body.Kind, body.Username, body.Host, body.Secret)
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
	httpx.JSON(w, http.StatusCreated, created)
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
