package deploy

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rousoftware/asgard/internal/store"
)

func TestStopPriorBeforeCandidate(t *testing.T) {
	tests := []struct {
		name string
		svc  store.Service
		want bool
	}{
		{name: "private web", svc: store.Service{Role: "web", Public: false}, want: true},
		{name: "public web", svc: store.Service{Role: "web", Public: true}, want: false},
		{name: "public worker", svc: store.Service{Role: "worker", Public: true}, want: true},
		{name: "public stateful", svc: store.Service{Role: "stateful", Public: true}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stopPriorBeforeCandidate(tt.svc); got != tt.want {
				t.Fatalf("stopPriorBeforeCandidate() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestKeepable(t *testing.T) {
	tests := []struct {
		name string
		svc  store.Service
		want bool
	}{
		{name: "long-running service", svc: store.Service{RestartPolicy: "unless-stopped", Volumes: []string{"asgard-demo-data:/var/lib/data"}}, want: true},
		{name: "one-shot job", svc: store.Service{RestartPolicy: "no"}, want: false},
		{name: "job-shaped but probed", svc: store.Service{RestartPolicy: "no", Container: store.ContainerConfig{Healthcheck: &store.Healthcheck{Test: []string{"CMD", "true"}}}}, want: true},
		{name: "mounts project source", svc: store.Service{RestartPolicy: "always", Volumes: []string{"@project/Caddyfile:/etc/caddy/Caddyfile:ro"}}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := keepable(tt.svc); got != tt.want {
				t.Fatalf("keepable() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestReleaseSummaryNamesWhatWasRecreated(t *testing.T) {
	services := []store.Service{{ID: "a", Name: "api"}, {ID: "b", Name: "db"}}
	cases := map[string]map[string]bool{
		"Release r7 is live; nothing changed, so all 2 services kept their running containers": {"a": true, "b": true},
		"Release r7 is live; recreated api and kept 1 unchanged":                               {"b": true},
		"Release r7 is live; recreated all 2 services":                                         {},
	}
	for want, kept := range cases {
		if got := releaseSummary(7, services, kept); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestCheckHostnamesRefusesTakenAndDuplicateNames(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "asgard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	now := store.Now()
	for _, query := range []string{
		`INSERT INTO projects(id,slug,name,source_type,source_path,created_at,updated_at) VALUES('other','other','Other','git','/x','` + now + `','` + now + `')`,
		`INSERT INTO services(id,project_id,name,created_at,updated_at) VALUES('s','other','web','` + now + `','` + now + `')`,
		`INSERT INTO routes(id,project_id,service_id,hostname,target_port,created_at,updated_at) VALUES('r','other','s','taken.example.com',80,'` + now + `','` + now + `')`,
	} {
		if _, err := database.DB.ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	deployer := &Deployer{Store: database}
	if err := deployer.checkHostnames(ctx, "mine", []store.Service{{Name: "web", Public: true, Hostname: "taken.example.com"}}); err == nil || !strings.Contains(err.Error(), "project other") {
		t.Fatalf("taken hostname: err = %v", err)
	}
	if err := deployer.checkHostnames(ctx, "mine", []store.Service{{Name: "a", Public: true, Hostname: "dup.example.com"}, {Name: "b", Public: true, Hostname: "dup.example.com"}}); err == nil {
		t.Fatal("duplicate hostname accepted")
	}
	if err := deployer.checkHostnames(ctx, "other", []store.Service{{Name: "web", Public: true, Hostname: "taken.example.com"}}); err != nil {
		t.Fatalf("own hostname refused: %v", err)
	}
}
