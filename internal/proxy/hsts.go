package proxy

import (
	"strings"

	"github.com/rousoftware/asgard/internal/store"
)

// InControlPlaneZone reports whether a hostname sits inside the wildcard domain
// the control plane owns. Only an exact suffix match counts: "notasgard.com"
// does not sit inside "asgard.com".
func InControlPlaneZone(hostname, controlPlaneDomain string) bool {
	hostname = strings.ToLower(strings.TrimSpace(hostname))
	controlPlaneDomain = strings.ToLower(strings.TrimSpace(controlPlaneDomain))
	if hostname == "" || controlPlaneDomain == "" {
		return false
	}
	return hostname == controlPlaneDomain || strings.HasSuffix(hostname, "."+controlPlaneDomain)
}

// resolveHSTS turns a service's stored mode into the concrete policy for one
// hostname.
func resolveHSTS(mode, hostname, controlPlaneDomain string) string {
	if !store.ValidHSTSMode(mode) {
		mode = store.HSTSAuto
	}
	if mode != store.HSTSAuto {
		return mode
	}
	if InControlPlaneZone(hostname, controlPlaneDomain) {
		return store.HSTSStrict
	}
	return store.HSTSStandard
}
