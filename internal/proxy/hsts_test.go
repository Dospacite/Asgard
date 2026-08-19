package proxy

import (
	"testing"

	"github.com/rousoftware/asgard/internal/store"
)

func TestResolveHSTSDefaultsByZone(t *testing.T) {
	const domain = "asgard.example.com"
	cases := []struct {
		name     string
		mode     string
		hostname string
		want     string
	}{
		// Inside the control plane's own wildcard zone every name is Asgard's
		// to commit, so the strong header stays the default there.
		{"wildcard subdomain keeps the strong header", store.HSTSAuto, "app.asgard.example.com", store.HSTSStrict},
		{"the control plane itself keeps the strong header", store.HSTSAuto, "asgard.example.com", store.HSTSStrict},
		// A custom domain is not Asgard's to commit. includeSubDomains on
		// someone's apex silently forces HTTPS on subdomains Asgard has never
		// heard of, and preload makes that near-irreversible.
		{"a custom apex gets a plain max-age", store.HSTSAuto, "example.org", store.HSTSStandard},
		{"a custom subdomain gets a plain max-age", store.HSTSAuto, "www.example.org", store.HSTSStandard},
		// A suffix that is not a label boundary is a different domain.
		{"a lookalike domain is not in-zone", store.HSTSAuto, "notasgard.example.com", store.HSTSStandard},
		// Explicit choices win in both directions.
		{"strict is honoured outside the zone", store.HSTSStrict, "example.org", store.HSTSStrict},
		{"standard is honoured inside the zone", store.HSTSStandard, "app.asgard.example.com", store.HSTSStandard},
		{"off is honoured", store.HSTSOff, "app.asgard.example.com", store.HSTSOff},
		// An unrecognised stored value must not be treated as an opt-in.
		{"an unknown mode falls back to the zone default", "bogus", "example.org", store.HSTSStandard},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveHSTS(tc.mode, tc.hostname, domain); got != tc.want {
				t.Fatalf("resolveHSTS(%q, %q) = %q, want %q", tc.mode, tc.hostname, got, tc.want)
			}
		})
	}
}

func TestUnconfiguredDomainNeverGrantsStrictHSTS(t *testing.T) {
	// With no control-plane domain configured, nothing is in-zone. Failing
	// toward the weaker header is the only safe direction: the strong one is
	// months to undo.
	if got := resolveHSTS(store.HSTSAuto, "example.org", ""); got != store.HSTSStandard {
		t.Fatalf("got %q, want %q", got, store.HSTSStandard)
	}
}

func TestOnlyStrictSendsIncludeSubDomainsAndPreload(t *testing.T) {
	middlewares := securityMiddlewares()
	strict := middlewares[middlewareName(store.HSTSStrict)].Headers
	if !strict.STSIncludeSubdomains || !strict.STSPreload || strict.STSSeconds != hstsYear {
		t.Fatalf("strict middleware is not strict: %#v", strict)
	}
	standard := middlewares[middlewareName(store.HSTSStandard)].Headers
	if standard.STSIncludeSubdomains || standard.STSPreload {
		t.Fatalf("standard middleware must not claim subdomains: %#v", standard)
	}
	if standard.STSSeconds != hstsYear {
		t.Fatalf("standard middleware should still pin HTTPS for its own host: %#v", standard)
	}
	off := middlewares[middlewareName(store.HSTSOff)].Headers
	if off.STSSeconds != 0 || off.STSIncludeSubdomains || off.STSPreload {
		t.Fatalf("off middleware must send no HSTS: %#v", off)
	}
	// The protections that make no claim about other names apply everywhere.
	for name, item := range middlewares {
		if !item.Headers.FrameDeny || !item.Headers.ContentTypeNosniff || item.Headers.ReferrerPolicy == "" {
			t.Fatalf("%s dropped a baseline header: %#v", name, item.Headers)
		}
	}
}

// The Traefik file provider shares one middleware namespace across every file
// in its directory. The control plane's own control-plane.yml defines
// "asgard-security" with the strong header, and hand-written custom-domain
// files reference it by name; a generated file that redefined it with
// different contents would make the winning definition non-deterministic.
func TestGeneratedMiddlewaresNeverCollideWithTheControlPlane(t *testing.T) {
	const controlPlaneMiddleware = "asgard-security"
	for name := range securityMiddlewares() {
		if name == controlPlaneMiddleware {
			t.Fatalf("generated middleware %q collides with the control plane's own definition", name)
		}
	}
	for _, policy := range []string{store.HSTSStrict, store.HSTSStandard, store.HSTSOff} {
		if got := middlewareName(policy); got == controlPlaneMiddleware {
			t.Fatalf("policy %q maps to the control plane's middleware name", policy)
		}
	}
	// Every policy must map to a distinct middleware, or one silently inherits
	// another's header.
	seen := map[string]string{}
	for _, policy := range []string{store.HSTSStrict, store.HSTSStandard, store.HSTSOff} {
		name := middlewareName(policy)
		if other, ok := seen[name]; ok {
			t.Fatalf("policies %q and %q share middleware %q", other, policy, name)
		}
		seen[name] = policy
	}
	if len(securityMiddlewares()) != len(seen) {
		t.Fatalf("every generated middleware should back exactly one policy")
	}
}
