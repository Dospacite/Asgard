package composecfg

import (
	"errors"
	"testing"

	"github.com/rousoftware/asgard/internal/store"
)

func settingsCode(t *testing.T, err error) string {
	t.Helper()
	var settings *SettingsError
	if !errors.As(err, &settings) {
		t.Fatalf("expected a SettingsError, got %v", err)
	}
	return settings.Code
}

func valid() ServiceSettings {
	return ServiceSettings{Role: "web", Public: true, Port: 8080, Hostname: "App.Example.COM", HealthPath: "/healthz", CPULimit: 0.5, MemoryLimit: 512 << 20, PIDsLimit: 256, RestartPolicy: "unless-stopped"}
}

func TestNormalizeAcceptsAndCanonicalizes(t *testing.T) {
	settings := valid()
	if err := settings.Normalize("asgard.example.com"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if settings.Hostname != "app.example.com" {
		t.Fatalf("hostname not lowercased: %q", settings.Hostname)
	}
	if settings.HSTSMode != store.HSTSAuto {
		t.Fatalf("default HSTS mode should be auto, got %q", settings.HSTSMode)
	}
}

// Every rule below was previously enforced by the REST API alone. The MCP
// server wrote to the same store without checking any of them, so each of these
// is a value an agent could once set that the browser rejected on the same
// field.
func TestNormalizeRejectsEachRule(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ServiceSettings)
		code   string
	}{
		{"unknown role", func(s *ServiceSettings) { s.Role = "cron" }, "invalid_role"},
		{"CPU below the floor", func(s *ServiceSettings) { s.CPULimit = 0.01 }, "invalid_resources"},
		{"CPU above the ceiling", func(s *ServiceSettings) { s.CPULimit = 128 }, "invalid_resources"},
		{"memory below the floor", func(s *ServiceSettings) { s.MemoryLimit = 1 << 20 }, "invalid_resources"},
		{"PIDs below the floor", func(s *ServiceSettings) { s.PIDsLimit = 1 }, "invalid_resources"},
		{"unknown restart policy", func(s *ServiceSettings) { s.RestartPolicy = "sometimes" }, "invalid_restart_policy"},
		{"relative health path", func(s *ServiceSettings) { s.HealthPath = "healthz" }, "invalid_health_path"},
		{"public service without a port", func(s *ServiceSettings) { s.Port = 0 }, "invalid_port"},
		{"single-label hostname", func(s *ServiceSettings) { s.Hostname = "app" }, "invalid_hostname"},
		{"control-plane hostname", func(s *ServiceSettings) { s.Hostname = "asgard.example.com" }, "reserved_hostname"},
		{"unknown HSTS mode", func(s *ServiceSettings) { s.HSTSMode = "preload-everything" }, "invalid_hsts_mode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			settings := valid()
			tc.mutate(&settings)
			err := settings.Normalize("asgard.example.com")
			if err == nil {
				t.Fatal("expected rejection")
			}
			if got := settingsCode(t, err); got != tc.code {
				t.Fatalf("got code %q, want %q", got, tc.code)
			}
		})
	}
}

func TestPrivateServiceSkipsRouteRules(t *testing.T) {
	// A private service has no route, so a missing port and hostname are not
	// errors — they are the normal state.
	settings := ServiceSettings{Role: "worker", HealthPath: "/", CPULimit: 0.5, MemoryLimit: 512 << 20, PIDsLimit: 256, RestartPolicy: "always"}
	if err := settings.Normalize("asgard.example.com"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEmptyHealthPathDefaultsToRoot(t *testing.T) {
	settings := valid()
	settings.HealthPath = ""
	if err := settings.Normalize("asgard.example.com"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if settings.HealthPath != "/" {
		t.Fatalf("got %q, want /", settings.HealthPath)
	}
}
