package dockerx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEvaluateReadinessRequiresStableUnhealthyContainerFreeWindow(t *testing.T) {
	now := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	ready, stableSince, err := evaluateReadiness(Container{State: "running"}, time.Time{}, now)
	if err != nil || ready || !stableSince.Equal(now) {
		t.Fatalf("initial readiness = ready %t, stable %s, err %v", ready, stableSince, err)
	}
	ready, _, err = evaluateReadiness(Container{State: "running"}, stableSince, now.Add(readinessStabilityWindow-time.Millisecond))
	if err != nil || ready {
		t.Fatalf("container became ready before stability window: ready %t, err %v", ready, err)
	}
	ready, _, err = evaluateReadiness(Container{State: "running"}, stableSince, now.Add(readinessStabilityWindow))
	if err != nil || !ready {
		t.Fatalf("stable container did not become ready: ready %t, err %v", ready, err)
	}
}

func TestEvaluateReadinessRejectsRestartLoop(t *testing.T) {
	_, _, err := evaluateReadiness(Container{State: "running", Restarts: 1}, time.Now(), time.Now())
	if err == nil || !strings.Contains(err.Error(), "restarted 1") {
		t.Fatalf("restart loop error = %v", err)
	}
}

func TestEvaluateReadinessUsesDockerHealth(t *testing.T) {
	ready, _, err := evaluateReadiness(Container{State: "running", Health: "healthy"}, time.Time{}, time.Now())
	if err != nil || !ready {
		t.Fatalf("healthy container = ready %t, err %v", ready, err)
	}
	_, _, err = evaluateReadiness(Container{State: "running", Health: "unhealthy"}, time.Time{}, time.Now())
	if err == nil {
		t.Fatal("expected unhealthy container to fail readiness")
	}
}

func TestWaitHTTPReadyRetriesUntilSuccess(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts < 2 {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if err := (&Engine{}).WaitHTTPReady(context.Background(), server.URL, 3*time.Second, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("expected two attempts, got %d", attempts)
	}
}
