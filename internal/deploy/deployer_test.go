package deploy

import (
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
