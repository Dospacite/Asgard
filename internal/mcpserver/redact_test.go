package mcpserver

import (
	"testing"

	"github.com/rousoftware/asgard/internal/store"
)

func TestRedactServiceWithholdsSecretsOnly(t *testing.T) {
	svc := store.Service{Environment: map[string]string{
		"API_KEY":      "abc",
		"DATABASE_URL": "postgresql://user:hunter2@db:5432/app",
		"REDIS_URL":    "redis://redis:6379/0",
		"TTS_RPM":      "100",
		"EMPTY_SECRET": "",
	}}
	got := redactService(svc).Environment
	want := map[string]string{"API_KEY": RedactedValue, "DATABASE_URL": RedactedValue, "REDIS_URL": "redis://redis:6379/0", "TTS_RPM": "100", "EMPTY_SECRET": ""}
	for name, value := range want {
		if got[name] != value {
			t.Errorf("%s = %q, want %q", name, got[name], value)
		}
	}
	if svc.Environment["API_KEY"] != "abc" {
		t.Fatal("redaction modified the stored service")
	}
}

func TestRejectRedactedRefusesTheMarker(t *testing.T) {
	if err := rejectRedacted(map[string]string{"API_KEY": RedactedValue}); err == nil {
		t.Fatal("marker accepted as a value")
	}
	if err := rejectRedacted(map[string]string{"API_KEY": "real"}); err != nil {
		t.Fatal(err)
	}
}
