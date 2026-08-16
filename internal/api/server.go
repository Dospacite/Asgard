package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rousoftware/asgard/internal/auth"
	"github.com/rousoftware/asgard/internal/config"
	"github.com/rousoftware/asgard/internal/dockerx"
	"github.com/rousoftware/asgard/internal/frontend"
	"github.com/rousoftware/asgard/internal/httpx"
	"github.com/rousoftware/asgard/internal/importer"
	"github.com/rousoftware/asgard/internal/networking"
	"github.com/rousoftware/asgard/internal/oauth"
	"github.com/rousoftware/asgard/internal/operations"
	"github.com/rousoftware/asgard/internal/proxy"
	"github.com/rousoftware/asgard/internal/secrets"
	"github.com/rousoftware/asgard/internal/store"
)

type Dependencies struct {
	Config     config.Config
	Store      *store.Store
	Auth       *auth.Service
	Docker     *dockerx.Engine
	Networks   *networking.Manager
	Operations *operations.Manager
	Importer   *importer.Importer
	Proxy      *proxy.Generator
	OAuth      *oauth.Server
	MCP        http.Handler
	Secrets    *secrets.Box
}
type Server struct {
	Dependencies
	router   http.Handler
	attempts *loginLimiter
}

func New(deps Dependencies) *Server {
	s := &Server{Dependencies: deps, attempts: newLoginLimiter()}
	s.router = s.routes()
	return s
}
func (s *Server) Handler() http.Handler { return s.router }

func (s *Server) routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.RealIP, middleware.StripSlashes)
	r.Use(s.recoverer)
	r.Use(httpx.SecurityHeaders)
	r.Use(s.requestLog)
	r.Get("/healthz", s.health)
	r.Get("/readyz", s.ready)
	if s.OAuth != nil {
		r.Get("/.well-known/oauth-protected-resource", s.OAuth.ProtectedResource)
		r.Get("/.well-known/oauth-protected-resource/mcp", s.OAuth.ProtectedResource)
		r.Get("/.well-known/oauth-authorization-server", s.OAuth.AuthorizationMetadata)
		r.Get("/.well-known/jwks.json", s.OAuth.JWKS)
		r.Get("/oauth/authorize", s.OAuth.Authorize)
		r.Post("/oauth/authorize", s.OAuth.AuthorizeDecision)
		r.Post("/oauth/token", s.OAuth.Token)
		r.Post("/oauth/register", s.OAuth.Register)
		r.Post("/oauth/revoke", s.OAuth.Revoke)
		if s.MCP != nil {
			r.Handle("/mcp", s.OAuth.VerifyMCP(s.MCP))
		}
	}
	r.Route("/api/v1/auth", func(r chi.Router) {
		r.Get("/status", s.authStatus)
		r.Post("/login", s.login)
		r.Post("/refresh", s.refresh)
		r.Post("/logout", s.logout)
	})
	r.Route("/api/v1", func(r chi.Router) {
		r.Use(s.Auth.Middleware)
		r.Use(s.Auth.CSRFMiddleware)
		r.Get("/me", s.me)
		r.Get("/overview", s.overview)
		r.Get("/system", s.system)
		r.Get("/events", s.events)
		r.Get("/compose-contract", s.composeContract)
		r.Post("/compose-contract/validate", s.validateCompose)
		r.Get("/projects", s.projects)
		r.Post("/imports/git", s.importGit)
		r.Post("/imports/image", s.importImage)
		r.Post("/imports/archive", s.importArchive)
		r.Post("/imports/zip", s.importArchive)
		r.Get("/git-credentials", s.gitCredentials)
		r.Post("/git-credentials", s.createGitCredential)
		r.Delete("/git-credentials/{credentialID}", s.deleteGitCredential)
		r.Route("/projects/{projectID}", func(r chi.Router) {
			r.Get("/", s.project)
			r.Patch("/", s.updateProject)
			r.Get("/source-files", s.sourceFiles)
			r.Patch("/source-files", s.updateSourceFile)
			r.Get("/deployments", s.deployments)
			r.Post("/deployments", s.createDeployment)
			r.Post("/rollbacks", s.rollback)
			r.Get("/releases", s.releases)
			r.Post("/webhook", s.createWebhook)
		})
		r.Route("/services/{serviceID}", func(r chi.Router) {
			r.Get("/", s.service)
			r.Patch("/", s.updateService)
			r.Get("/stats", s.serviceStats)
			r.Get("/logs", s.serviceLogs)
			r.Post("/actions/{action}", s.serviceAction)
		})
		r.Get("/deployments", s.deployments)
		r.Get("/operations", s.operationsList)
		r.Get("/operations/{operationID}", s.operation)
		r.Get("/operations/{operationID}/logs", s.operationLogs)
		r.Post("/operations/{operationID}/cancel", s.cancelOperation)
		r.Get("/containers", s.containers)
		r.Get("/networks", s.networks)
		r.Post("/networks", s.createNetwork)
		r.Get("/networks/topology", s.networkTopology)
		r.Route("/networks/{networkID}", func(r chi.Router) {
			r.Get("/", s.network)
			r.Post("/members", s.attachNetworkMember)
			r.Delete("/members/{serviceID}", s.detachNetworkMember)
			r.Post("/reconcile", s.reconcileNetwork)
		})
		r.Get("/unmanaged-containers", s.unmanagedContainers)
		r.Get("/unmanaged-containers/{containerID}/preview", s.adoptionPreview)
		r.Post("/unmanaged-containers/{containerID}/adopt", s.adoptContainer)
		r.Get("/volumes", s.volumes)
		r.Get("/backups", s.backups)
		r.Post("/volumes/{volumeID}/backups", s.createBackup)
		r.Post("/backups/{backupID}/restore", s.restoreBackup)
		r.Get("/audit", s.audit)
		r.Post("/deletions/preview", s.deletionPreview)
		r.Post("/deletions/confirm", s.deletionConfirm)
	})
	r.Get("/*", frontend.Handler().ServeHTTP)
	r.Get("/", frontend.Handler().ServeHTTP)
	return r
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if value := recover(); value != nil {
				slog.Error("request panic", "value", value, "stack", string(debug.Stack()), "request_id", middleware.GetReqID(r.Context()))
				httpx.Error(w, http.StatusInternalServerError, "internal_error", "An internal error occurred.")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
func (s *Server) requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		slog.Debug("http request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(start), "request_id", middleware.GetReqID(r.Context()))
	})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "ok", "version": "0.2.0"})
}
func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.Health(r.Context()); err != nil {
		httpx.Error(w, http.StatusServiceUnavailable, "database_unavailable", "Database is unavailable.")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "ready"})
}

func identity(r *http.Request) auth.Identity {
	value, _ := auth.IdentityFrom(r.Context())
	return value
}
func (s *Server) auditRequest(r *http.Request, action, targetType, targetID, summary string) {
	actor := identity(r)
	_ = s.Store.Audit(context.WithoutCancel(r.Context()), actor.ActorType, actor.UserID, action, targetType, targetID, summary, httpx.ClientIP(r), r.UserAgent())
}

func parseLimit(r *http.Request, fallback, max int) int {
	value := fallback
	if _, err := fmt.Sscan(r.URL.Query().Get("limit"), &value); err != nil || value < 1 {
		value = fallback
	}
	if value > max {
		value = max
	}
	return value
}
func idempotency(r *http.Request) string { return strings.TrimSpace(r.Header.Get("Idempotency-Key")) }

type loginLimiter struct {
	mu      sync.Mutex
	entries map[string]*loginAttempt
}
type loginAttempt struct {
	failures     int
	window       time.Time
	blockedUntil time.Time
}

func newLoginLimiter() *loginLimiter { return &loginLimiter{entries: map[string]*loginAttempt{}} }
func (l *loginLimiter) allowed(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.entries[key]
	if entry == nil {
		return true
	}
	return time.Now().After(entry.blockedUntil)
}
func (l *loginLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	entry := l.entries[key]
	if entry == nil || now.Sub(entry.window) > 15*time.Minute {
		entry = &loginAttempt{window: now}
		l.entries[key] = entry
	}
	entry.failures++
	if entry.failures >= 5 {
		entry.blockedUntil = now.Add(time.Duration(entry.failures-4) * time.Minute)
	}
}
func (l *loginLimiter) success(key string) { l.mu.Lock(); delete(l.entries, key); l.mu.Unlock() }

func writeError(w http.ResponseWriter, err error) {
	if store.IsNotFound(err) {
		httpx.Error(w, http.StatusNotFound, "not_found", "The requested resource was not found.")
		return
	}
	if errors.Is(err, context.Canceled) {
		httpx.Error(w, 499, "canceled", "The operation was canceled.")
		return
	}
	httpx.Error(w, http.StatusInternalServerError, "internal_error", err.Error())
}
func payload(value any) json.RawMessage { bytes, _ := json.Marshal(value); return bytes }
