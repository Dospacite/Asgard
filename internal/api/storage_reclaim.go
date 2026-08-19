package api

import (
	"net/http"

	"github.com/rousoftware/asgard/internal/httpx"
	"github.com/rousoftware/asgard/internal/reclaim"
)

// storageReclaim frees Docker artifacts Asgard itself created.
//
// The scheduled sweep and the post-deployment pass handle the steady state;
// this exists for the moment an operator is staring at a full disk and wants it
// dealt with now, and — as a dry run — for seeing what would go before it does.
func (s *Server) storageReclaim(w http.ResponseWriter, r *http.Request) {
	if s.Reclaimer == nil {
		httpx.Error(w, http.StatusServiceUnavailable, "reclaim_unavailable", "Storage reclamation is unavailable.")
		return
	}
	var body struct {
		DryRun       bool `json:"dryRun"`
		KeepReleases int  `json:"keepReleases"`
	}
	if r.ContentLength > 0 && !httpx.Decode(w, r, &body) {
		return
	}
	policy := s.Reclaimer.Policy
	policy.DryRun = body.DryRun
	if body.KeepReleases > 0 {
		policy.KeepReleases = body.KeepReleases
	}
	// A per-request policy override must not mutate the shared reclaimer the
	// scheduled sweep and the deployer both hold.
	scoped := &reclaim.Reclaimer{Store: s.Store, Docker: s.Docker, Policy: policy}
	result, err := scoped.Run(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	if !body.DryRun {
		s.auditRequest(r, "storage.reclaim", "system", "", result.Summary())
	}
	httpx.JSON(w, http.StatusOK, result)
}
