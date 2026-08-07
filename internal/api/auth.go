package api

import (
	"database/sql"
	"net/http"
	"strings"
	"time"

	"github.com/rousoftware/asgard/internal/httpx"
)

func (s *Server) authStatus(w http.ResponseWriter, r *http.Request) {
	count, err := s.Auth.CountUsers(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"initialized": count > 0, "singleUser": true})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	key := httpx.ClientIP(r)
	if !s.attempts.allowed(key) {
		httpx.Error(w, http.StatusTooManyRequests, "rate_limited", "Too many login attempts. Try again shortly.")
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !httpx.Decode(w, r, &body) {
		return
	}
	user, err := s.Auth.Authenticate(r.Context(), body.Username, body.Password)
	if err != nil {
		s.attempts.fail(key)
		time.Sleep(250 * time.Millisecond)
		httpx.Error(w, http.StatusUnauthorized, "invalid_credentials", "Username or password is incorrect.")
		return
	}
	s.attempts.success(key)
	access, refresh, csrf, err := s.Auth.IssueSession(r.Context(), user)
	if err != nil {
		writeError(w, err)
		return
	}
	s.Auth.SetCookies(w, access, refresh, csrf)
	_ = s.Store.Audit(r.Context(), "user", user.ID, "auth.login", "user", user.ID, "Signed in", key, r.UserAgent())
	httpx.JSON(w, http.StatusOK, map[string]any{"user": user, "csrfToken": csrf})
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("asgard_refresh")
	if err != nil {
		httpx.Error(w, http.StatusUnauthorized, "invalid_session", "Refresh session is missing.")
		return
	}
	access, refresh, csrf, user, err := s.Auth.Refresh(r.Context(), cookie.Value)
	if err != nil {
		s.Auth.ClearCookies(w)
		httpx.Error(w, http.StatusUnauthorized, "invalid_session", "Session expired. Sign in again.")
		return
	}
	s.Auth.SetCookies(w, access, refresh, csrf)
	httpx.JSON(w, http.StatusOK, map[string]any{"user": user, "csrfToken": csrf})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie("asgard_refresh"); err == nil {
		s.Auth.RevokeSession(r.Context(), cookie.Value)
	}
	s.Auth.ClearCookies(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	actor := identity(r)
	var created string
	var last sql.NullString
	err := s.Store.DB.QueryRowContext(r.Context(), `SELECT created_at,last_login_at FROM users WHERE id=?`, actor.UserID).Scan(&created, &last)
	if err != nil {
		writeError(w, err)
		return
	}
	response := map[string]any{"id": actor.UserID, "username": actor.Username, "createdAt": created}
	if last.Valid {
		response["lastLoginAt"] = last.String
	}
	httpx.JSON(w, http.StatusOK, response)
}

func bearer(header string) string {
	if strings.HasPrefix(strings.ToLower(header), "bearer ") {
		return strings.TrimSpace(header[7:])
	}
	return ""
}
