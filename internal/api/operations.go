package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/rousoftware/asgard/internal/httpx"
)

func (s *Server) operationsList(w http.ResponseWriter, r *http.Request) {
	items, err := s.Store.ListOperations(r.Context(), parseLimit(r, 50, 200))
	if err != nil {
		writeError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
}
func (s *Server) operation(w http.ResponseWriter, r *http.Request) {
	item, err := s.Store.GetOperation(r.Context(), chi.URLParam(r, "operationID"))
	if err != nil {
		writeError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, item)
}
func (s *Server) operationLogs(w http.ResponseWriter, r *http.Request) {
	items, err := s.Store.OperationLogs(r.Context(), chi.URLParam(r, "operationID"), parseLimit(r, 1000, 5000))
	if err != nil {
		writeError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
}
func (s *Server) cancelOperation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "operationID")
	if err := s.Operations.Cancel(id); err != nil {
		httpx.Error(w, http.StatusConflict, "cannot_cancel", err.Error())
		return
	}
	s.auditRequest(r, "operation.cancel", "operation", id, "Canceled operation")
	w.WriteHeader(http.StatusNoContent)
}
