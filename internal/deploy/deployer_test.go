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
