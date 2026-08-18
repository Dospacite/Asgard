package composecfg

import (
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
