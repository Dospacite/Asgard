package deploy

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rousoftware/asgard/internal/store"
)

func builtService(id, name, context, dockerfile string, args map[string]string) store.Service {
	svc := store.Service{ID: id, Name: name, BuildContext: context, Dockerfile: dockerfile}
	svc.Container.BuildArgs = args
	return svc
}

func TestPlanBuildsSharesIdenticalBuilds(t *testing.T) {
	services := []store.Service{
		{ID: "pg", Name: "postgres", Image: "postgres:17"},
		builtService("api", "backend", ".", "services/backend/Dockerfile", map[string]string{"A": "1", "B": "2"}),
		builtService("w1", "document-worker", "./", "services/backend/Dockerfile", map[string]string{"B": "2", "A": "1"}),
		builtService("w2", "export-worker", ".", "services/backend/Dockerfile", map[string]string{"A": "other"}),
		builtService("web", "web", ".", "apps/web/Dockerfile", nil),
	}
	plan, refs := planBuilds("/src", "flobby", 7, services)
	if len(plan) != 3 {
		t.Fatalf("planned %d builds, want 3", len(plan))
	}
	if got := strings.Join(plan[0].tags, " "); got != "asgard/flobby/backend:r7 asgard/flobby/document-worker:r7" {
		t.Fatalf("shared build tags = %q", got)
	}
	if _, ok := refs["pg"]; ok {
		t.Fatal("a pulled image must not be planned as a build")
	}
	if refs["w1"] != "asgard/flobby/document-worker:r7" || refs["web"] != "asgard/flobby/web:r7" {
		t.Fatalf("refs = %v", refs)
	}
}

func TestRunBuildsRunsInParallelUpToTheLimit(t *testing.T) {
	plan := []*imageBuild{{services: []string{"a"}}, {services: []string{"b"}}, {services: []string{"c"}}, {services: []string{"d"}}}
	var running, peak atomic.Int32
	build := func(ctx context.Context, _, _ string, _ []string, _ string, _ map[string]string, log func(string)) error {
		now := running.Add(1)
		for {
			old := peak.Load()
			if now <= old || peak.CompareAndSwap(old, now) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		log("done")
		running.Add(-1)
		return nil
	}
	var mu sync.Mutex
	lines := []string{}
	err := runBuilds(context.Background(), plan, 2, build, func(line string) { mu.Lock(); lines = append(lines, line); mu.Unlock() })
	if err != nil {
		t.Fatal(err)
	}
	if peak.Load() != 2 {
		t.Fatalf("peak concurrency = %d, want 2", peak.Load())
	}
	sort.Strings(lines)
	if strings.Join(lines, ",") != "[a] done,[b] done,[c] done,[d] done" {
		t.Fatalf("lines = %v", lines)
	}
}

func TestRunBuildsStopsOthersOnFailure(t *testing.T) {
	plan := []*imageBuild{{contextDir: "bad", services: []string{"bad"}}, {contextDir: "slow", services: []string{"slow"}}}
	build := func(ctx context.Context, contextDir, _ string, _ []string, _ string, _ map[string]string, _ func(string)) error {
		if contextDir == "bad" {
			return errors.New("compile error")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
			return nil
		}
	}
	started := time.Now()
	err := runBuilds(context.Background(), plan, 2, build, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "build bad: compile error") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("a failed build did not cancel the others")
	}
}
