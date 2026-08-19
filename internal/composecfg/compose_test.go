package composecfg

import (
	"errors"
	"testing"
)

func TestSafeCompose(t *testing.T) {
	raw := []byte(`services:
  web:
    image: nginx:1.27
    expose: [8080]
x-asgard:
  primary-service: web
  services:
    web:
      public: true
      port: 8080
      health-path: /
`)
	_, result := Parse(raw, "project-id", "demo", t.TempDir())
	if !result.Valid {
		t.Fatalf("expected valid: %#v", result.Errors)
	}
	if result.Services[0].Hostname != "demo.asgard.rousoftware.com" {
		t.Fatalf("unexpected hostname %s", result.Services[0].Hostname)
	}
}
func TestRejectsPrivilegedAndBind(t *testing.T) {
	raw := []byte(`services:
  web:
    image: nginx
    privileged: true
    volumes: ["/host:/data"]
`)
	_, result := Parse(raw, "project-id", "demo", t.TempDir())
	if result.Valid {
		t.Fatal("unsafe compose accepted")
	}
	if len(result.Errors) < 2 {
		t.Fatalf("expected multiple errors: %#v", result.Errors)
	}
}

func TestValidatePublicHostnameAcceptsAnyZone(t *testing.T) {
	const controlPlane = "asgard.rousoftware.com"
	accepted := []string{
		"cms--rouwriteups.asgard.rousoftware.com", // still fine inside the wildcard domain
		"blog.rousoftware.com",                    // sibling zone
		"patches.example.com",                     // unrelated registrable domain
		"example.co.uk",                           // apex of an unrelated domain
		"Deep.Sub.Domain.Example.COM",             // case is normalised before checking
	}
	for _, hostname := range accepted {
		if err := ValidatePublicHostname(hostname, controlPlane); err != nil {
			t.Fatalf("expected %q to be accepted, got %v", hostname, err)
		}
	}

	rejected := map[string]error{
		"":                          ErrHostnameInvalid,
		"localhost":                 ErrHostnameInvalid, // single label can never get a public certificate
		"not a hostname":            ErrHostnameInvalid,
		"-leading-dash.example.com": ErrHostnameInvalid,
		"http://example.com":        ErrHostnameInvalid,
		controlPlane:                ErrHostnameReserved,
		"ASGARD.rousoftware.com":    ErrHostnameReserved,
	}
	for hostname, want := range rejected {
		if err := ValidatePublicHostname(hostname, controlPlane); !errors.Is(err, want) {
			t.Fatalf("hostname %q: expected %v, got %v", hostname, want, err)
		}
	}
}
