package dockerx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rousoftware/asgard/internal/store"
)

func TestEvaluateReadinessRequiresStableUnhealthyContainerFreeWindow(t *testing.T) {
	now := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	ready, stableSince, err := evaluateReadiness(Container{State: "running"}, ReadinessSpec{}, time.Time{}, now)
	if err != nil || ready || !stableSince.Equal(now) {
		t.Fatalf("initial readiness = ready %t, stable %s, err %v", ready, stableSince, err)
	}
	ready, _, err = evaluateReadiness(Container{State: "running"}, ReadinessSpec{}, stableSince, now.Add(readinessStabilityWindow-time.Millisecond))
	if err != nil || ready {
		t.Fatalf("container became ready before stability window: ready %t, err %v", ready, err)
	}
	ready, _, err = evaluateReadiness(Container{State: "running"}, ReadinessSpec{}, stableSince, now.Add(readinessStabilityWindow))
	if err != nil || !ready {
		t.Fatalf("stable container did not become ready: ready %t, err %v", ready, err)
	}
}

func TestEvaluateReadinessRejectsRestartLoop(t *testing.T) {
	_, _, err := evaluateReadiness(Container{State: "running", Restarts: 1}, ReadinessSpec{}, time.Now(), time.Now())
	if err == nil || !strings.Contains(err.Error(), "restarted 1") {
		t.Fatalf("restart loop error = %v", err)
	}
}

func TestEvaluateReadinessUsesDockerHealth(t *testing.T) {
	ready, _, err := evaluateReadiness(Container{State: "running", Health: "healthy"}, ReadinessSpec{}, time.Time{}, time.Now())
	if err != nil || !ready {
		t.Fatalf("healthy container = ready %t, err %v", ready, err)
	}
	_, _, err = evaluateReadiness(Container{State: "running", Health: "unhealthy"}, ReadinessSpec{}, time.Time{}, time.Now())
	if err == nil {
		t.Fatal("expected unhealthy container to fail readiness")
	}
}

// A job is judged by its exit code: the work being done is the goal, so
// finishing zero is ready and any other exit is a failed release. This is what
// makes depends_on's service_completed_successfully mean what it says — the
// dependents are not started until the job is done.
func TestEvaluateReadinessJudgesAJobByItsExitCode(t *testing.T) {
	job := ReadinessSpec{Job: true, AllowCleanExit: true}
	ready, _, err := evaluateReadiness(Container{State: "exited", ExitCode: 0}, job, time.Time{}, time.Now())
	if err != nil || !ready {
		t.Fatalf("successful job = ready %t, err %v", ready, err)
	}
	ready, _, err = evaluateReadiness(Container{State: "exited", ExitCode: 1}, job, time.Time{}, time.Now())
	if err == nil || ready {
		t.Fatalf("failed job must fail readiness, got ready %t, err %v", ready, err)
	}
	// Still running: the work is not done, and no stability window applies.
	ready, _, err = evaluateReadiness(Container{State: "running"}, job, time.Now().Add(-time.Hour), time.Now())
	if err != nil || ready {
		t.Fatalf("running job = ready %t, err %v", ready, err)
	}
	// The same container under a long-running policy is a release failure.
	if _, _, err = evaluateReadiness(Container{State: "exited", ExitCode: 0}, ReadinessSpec{}, time.Time{}, time.Now()); err == nil {
		t.Fatal("a long-running service that exited must fail readiness")
	}
	// A service Docker will not restart after a clean exit but that is still
	// running — `restart: on-failure` on a worker — is judged as a service.
	worker := ReadinessSpec{AllowCleanExit: true}
	ready, _, err = evaluateReadiness(Container{State: "running"}, worker, time.Now().Add(-time.Hour), time.Now())
	if err != nil || !ready {
		t.Fatalf("running worker = ready %t, err %v", ready, err)
	}
}

// What a service is judged by comes from what its file declared: `restart: no`
// with no healthcheck is a job, a declared healthcheck means it keeps running,
// and a restart policy other than "no" means the same.
func TestReadinessForDerivesIntentFromTheFile(t *testing.T) {
	migration := store.Service{RestartPolicy: "no"}
	if spec := ReadinessFor(migration); !spec.Job || !spec.AllowCleanExit {
		t.Fatalf("one-shot migration = %+v", spec)
	}
	server := store.Service{RestartPolicy: "unless-stopped"}
	if spec := ReadinessFor(server); spec.Job || spec.AllowCleanExit {
		t.Fatalf("long-running server = %+v", spec)
	}
	// A service with restart "no" that declares a healthcheck is asking to be
	// probed, not waited out.
	probed := store.Service{RestartPolicy: "no", Container: store.ContainerConfig{Healthcheck: &store.Healthcheck{Test: []string{"CMD", "true"}}}}
	if spec := ReadinessFor(probed); spec.Job {
		t.Fatalf("healthchecked service = %+v", spec)
	}
	if spec := ReadinessFor(store.Service{RestartPolicy: "on-failure:3"}); spec.Job || !spec.AllowCleanExit {
		t.Fatalf("on-failure worker = %+v", spec)
	}
}

// The readiness budget follows the healthcheck the file declared, so a service
// that says it needs two minutes to start is given two minutes.
func TestReadinessTimeoutFollowsTheDeclaredHealthcheck(t *testing.T) {
	plain := store.Service{RestartPolicy: "unless-stopped"}
	if got := ReadinessTimeout(plain); got != readinessFloor {
		t.Fatalf("service without a healthcheck = %s, want %s", got, readinessFloor)
	}
	slow := store.Service{RestartPolicy: "unless-stopped", Container: store.ContainerConfig{Healthcheck: &store.Healthcheck{Test: []string{"CMD-SHELL", "true"}, Interval: 10 * time.Second, Retries: 20, StartPeriod: 30 * time.Second}}}
	want := 30*time.Second + 21*10*time.Second + readinessSlack
	if got := ReadinessTimeout(slow); got != want {
		t.Fatalf("declared healthcheck budget = %s, want %s", got, want)
	}
	huge := store.Service{Container: store.ContainerConfig{Healthcheck: &store.Healthcheck{Test: []string{"CMD-SHELL", "true"}, Interval: time.Hour, Retries: 1000}}}
	if got := ReadinessTimeout(huge); got != readinessCeiling {
		t.Fatalf("absurd budget = %s, want the ceiling %s", got, readinessCeiling)
	}
	disabled := store.Service{Container: store.ContainerConfig{Healthcheck: &store.Healthcheck{Test: []string{"NONE"}, Interval: time.Minute, Retries: 10}}}
	if got := ReadinessTimeout(disabled); got != readinessFloor {
		t.Fatalf("disabled healthcheck = %s, want the floor %s", got, readinessFloor)
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
