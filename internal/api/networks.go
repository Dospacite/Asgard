package api

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/rousoftware/asgard/internal/composecfg"
	"github.com/rousoftware/asgard/internal/httpx"
	"github.com/rousoftware/asgard/internal/networking"
	"github.com/rousoftware/asgard/internal/store"
)

func (s *Server) networks(w http.ResponseWriter, r *http.Request) {
	items, err := s.Networks.List(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) network(w http.ResponseWriter, r *http.Request) {
	item, err := s.Networks.Get(r.Context(), chi.URLParam(r, "networkID"))
	if err != nil {
		writeError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, item)
}

func (s *Server) networkTopology(w http.ResponseWriter, r *http.Request) {
	item, err := s.Networks.Topology(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, item)
}

func (s *Server) createNetwork(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string `json:"name"`
		Slug        string `json:"slug"`
		Description string `json:"description"`
		Internal    bool   `json:"internal"`
	}
	if !httpx.Decode(w, r, &body) {
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || len(name) > 100 || len(body.Description) > 1000 {
		httpx.Error(w, http.StatusBadRequest, "invalid_network", "Name is required, limited to 100 characters, and descriptions are limited to 1,000 characters.")
		return
	}
	slug := composecfg.Slug(body.Slug)
	if slug == "" {
		slug = composecfg.Slug(name)
	}
	if !composecfg.ValidateSlug(slug) {
		httpx.Error(w, http.StatusBadRequest, "invalid_network_slug", "Network slug must be a valid DNS label.")
		return
	}
	created, err := s.Networks.Create(r.Context(), store.ManagedNetwork{
		ID:          uuid.NewString(),
		Slug:        slug,
		Name:        name,
		DockerName:  "asgard-shared-" + slug,
		Description: strings.TrimSpace(body.Description),
		Driver:      "bridge",
		Internal:    body.Internal,
	})
	if err != nil {
		if isNetworkConflict(err) {
			httpx.Error(w, http.StatusConflict, "network_conflict", "A network with this slug, Docker name, or ownership already exists.")
			return
		}
		httpx.Error(w, http.StatusBadGateway, "network_create_failed", err.Error())
		return
	}
	s.auditRequest(r, "network.create", "network", created.ID, "Created shared network "+created.Name)
	view, err := s.Networks.Get(r.Context(), created.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, view)
}

func (s *Server) attachNetworkMember(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ServiceID string `json:"serviceId"`
		Alias     string `json:"alias"`
	}
	if !httpx.Decode(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.ServiceID) == "" {
		httpx.Error(w, http.StatusBadRequest, "service_required", "Choose a service to attach.")
		return
	}
	if body.Alias != "" && networking.NormalizeAlias(body.Alias) == "" {
		httpx.Error(w, http.StatusBadRequest, "invalid_network_alias", "DNS alias must contain an ASCII letter or number.")
		return
	}
	networkID := chi.URLParam(r, "networkID")
	item, err := s.Networks.Attach(r.Context(), networkID, body.ServiceID, body.Alias)
	if err != nil {
		if store.IsNotFound(err) {
			writeError(w, err)
			return
		}
		if isNetworkConflict(err) {
			httpx.Error(w, http.StatusConflict, "network_membership_conflict", "This service or DNS alias is already attached to the network.")
			return
		}
		httpx.Error(w, http.StatusBadGateway, "network_attach_failed", err.Error())
		return
	}
	s.auditRequest(r, "network.attach", "network", item.ID, "Attached service "+body.ServiceID+" to shared network")
	view, err := s.Networks.Get(r.Context(), item.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, view)
}

func (s *Server) detachNetworkMember(w http.ResponseWriter, r *http.Request) {
	networkID := chi.URLParam(r, "networkID")
	serviceID := chi.URLParam(r, "serviceID")
	item, err := s.Networks.Detach(r.Context(), networkID, serviceID)
	if err != nil {
		if store.IsNotFound(err) {
			writeError(w, err)
			return
		}
		httpx.Error(w, http.StatusBadGateway, "network_detach_failed", err.Error())
		return
	}
	s.auditRequest(r, "network.detach", "network", item.ID, "Detached service "+serviceID+" from shared network")
	view, err := s.Networks.Get(r.Context(), item.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, view)
}

func (s *Server) reconcileNetwork(w http.ResponseWriter, r *http.Request) {
	item, err := s.Networks.Reconcile(r.Context(), chi.URLParam(r, "networkID"))
	if err != nil {
		if store.IsNotFound(err) {
			writeError(w, err)
			return
		}
		httpx.Error(w, http.StatusBadGateway, "network_reconcile_failed", err.Error())
		return
	}
	s.auditRequest(r, "network.reconcile", "network", item.ID, "Reconciled shared network endpoints")
	view, err := s.Networks.Get(r.Context(), item.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, view)
}

func isNetworkConflict(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unique constraint") || strings.Contains(message, "already exists") || strings.Contains(message, "conflicting ownership") || strings.Contains(message, "different internal-access policy")
}
