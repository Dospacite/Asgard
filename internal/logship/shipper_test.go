package logship

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rousoftware/asgard/internal/dockerx"
	"github.com/rousoftware/asgard/internal/store"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeSource stands in for the Docker engine. Its LogFrames honours since the
// way the daemon does — inclusive — so a repeated boundary line is the fake's
// normal behaviour rather than an error case invented by the test.
type fakeSource struct {
	containers []dockerx.Container
	frames     map[string][]dockerx.LogFrame
	queried    []string
	sinces     map[string]time.Time
}

func (f *fakeSource) Containers(ctx context.Context, all bool) ([]dockerx.Container, error) {
	return f.containers, nil
}

func (f *fakeSource) LogFrames(ctx context.Context, id string, since time.Time, maxBytes int64) ([]dockerx.LogFrame, error) {
	f.queried = append(f.queried, id)
	if f.sinces == nil {
		f.sinces = map[string]time.Time{}
	}
	f.sinces[id] = since
	out := []dockerx.LogFrame{}
	for _, frame := range f.frames[id] {
		if !frame.Time.Before(since) {
			out = append(out, frame)
		}
	}
	return out, nil
}

type recordingSink struct {
	*httptest.Server
	mu       sync.Mutex
	pushes   []lokiPush
	statuses []int
	failures map[int]int
}

func newSink(t *testing.T) *recordingSink {
	t.Helper()
	sink := &recordingSink{failures: map[int]int{}}
	sink.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read push body: %v", err)
		}
		var push lokiPush
		if err := json.Unmarshal(body, &push); err != nil {
			t.Errorf("push body is not a Loki payload: %v (%s)", err, body)
		}
		sink.mu.Lock()
		sink.pushes = append(sink.pushes, push)
		index := len(sink.pushes)
		status := sink.failures[index]
		if status == 0 {
			status = http.StatusNoContent
		}
		sink.statuses = append(sink.statuses, status)
		sink.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(sink.Close)
	return sink
}

// attempts counts every request the shipper made, accepted or not.
func (s *recordingSink) attempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pushes)
}

func (s *recordingSink) fail(request int, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures[request] = status
}

func (s *recordingSink) acceptEverything() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = map[int]int{}
}

// lines returns what the log store would have kept: the values in pushes it
// accepted, in arrival order.
func (s *recordingSink) lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []string{}
	for index, push := range s.pushes {
		if s.statuses[index] > 299 {
			continue
		}
		for _, stream := range push.Streams {
			for _, value := range stream.Values {
				out = append(out, value[1])
			}
		}
	}
	return out
}

func (s *recordingSink) labelValues(name string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	out := []string{}
	for index, push := range s.pushes {
		if s.statuses[index] > 299 {
			continue
		}
		for _, stream := range push.Streams {
			if value := stream.Stream[name]; value != "" && !seen[value] {
				seen[value] = true
				out = append(out, value)
			}
		}
	}
	return out
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	opened, err := store.Open(filepath.Join(t.TempDir(), "asgard.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { opened.Close() })
	return opened
}

func container(id, name string, labels map[string]string) dockerx.Container {
	base := map[string]string{
		"com.rousoftware.asgard.managed": "true",
		"com.rousoftware.asgard.project": "demo",
		"com.rousoftware.asgard.service": "app",
	}
	for key, value := range labels {
		base[key] = value
	}
	return dockerx.Container{ID: id, Name: name, State: "running", Managed: true, Labels: base}
}

// base anchors the fixtures near the present. The shipper always reads forward
// from a checkpoint or a lookback window, so frames pinned to a fixed date in
// the past would never fall inside the window any test exercises.
var base = time.Now().UTC().Truncate(time.Second)

func at(offset time.Duration) time.Time {
	return base.Add(offset)
}

func newShipper(t *testing.T, source LogSource, cursors Cursors, sink string) *Shipper {
	t.Helper()
	return &Shipper{
		Docker:     source,
		Store:      cursors,
		Sink:       sink,
		Interval:   time.Hour,
		Lookback:   time.Minute,
		BatchBytes: 1 << 20,
		RetryDelay: time.Millisecond,
		Client:     &http.Client{Timeout: 5 * time.Second},
		Logger:     quietLogger(),
	}
}

func TestShipPushesStreamsWithLabelsAndCheckpoint(t *testing.T) {
	sink := newSink(t)
	database := testStore(t)
	source := &fakeSource{
		containers: []dockerx.Container{container("c1", "asgard-demo-app-r1", nil)},
		frames: map[string][]dockerx.LogFrame{
			"c1": {
				{Stream: "stdout", Time: at(0), Line: "listening on 3000"},
				{Stream: "stderr", Time: at(time.Millisecond), Line: "connection refused"},
			},
		},
	}
	shipper := newShipper(t, source, database, sink.URL)

	shipper.ship(context.Background())

	if got := sink.lines(); len(got) != 2 {
		t.Fatalf("pushed %d lines, want 2: %v", len(got), got)
	}
	if got := sink.labelValues("project"); len(got) != 1 || got[0] != "demo" {
		t.Errorf("project labels = %v, want [demo]", got)
	}
	if got := sink.labelValues("stream"); len(got) != 2 {
		t.Errorf("stream labels = %v, want stdout and stderr", got)
	}
	cursor, found, err := database.LogCursor(context.Background(), "c1")
	if err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	if !found || !cursor.Time.Equal(at(time.Millisecond)) || cursor.Line != "connection refused" {
		t.Fatalf("cursor = %+v found=%v, want the last frame", cursor, found)
	}
}

// A sink that never accepts the batch must leave the checkpoint alone, so the
// next pass sends the same lines instead of dropping them.
func TestShipKeepsCheckpointWhenSinkRejects(t *testing.T) {
	sink := newSink(t)
	sink.fail(1, http.StatusInternalServerError)
	sink.fail(2, http.StatusInternalServerError)
	sink.fail(3, http.StatusInternalServerError)
	database := testStore(t)
	source := &fakeSource{
		containers: []dockerx.Container{container("c1", "asgard-demo-app-r1", nil)},
		frames:     map[string][]dockerx.LogFrame{"c1": {{Stream: "stdout", Time: at(0), Line: "hello"}}},
	}
	shipper := newShipper(t, source, database, sink.URL)

	shipper.ship(context.Background())

	if _, found, err := database.LogCursor(context.Background(), "c1"); err != nil || found {
		t.Fatalf("cursor found=%v err=%v, want no checkpoint after a rejected batch", found, err)
	}
	if sink.attempts() != 3 {
		t.Fatalf("sink saw %d attempts, want 3", sink.attempts())
	}
	if got := sink.lines(); len(got) != 0 {
		t.Fatalf("sink accepted %v, want nothing while it rejects", got)
	}

	// The line is recoverable rather than lost: the next pass sends it again.
	sink.acceptEverything()
	shipper.ship(context.Background())

	if got := sink.lines(); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("after recovery the sink accepted %v, want the line once", got)
	}
}

func TestShipDropsTheLineTheCheckpointAlreadyCovers(t *testing.T) {
	sink := newSink(t)
	database := testStore(t)
	if err := database.SetLogCursor(context.Background(), store.LogCursor{ContainerID: "c1", Time: at(0), Stream: "stdout", Line: "hello"}); err != nil {
		t.Fatalf("seed cursor: %v", err)
	}
	source := &fakeSource{
		containers: []dockerx.Container{container("c1", "asgard-demo-app-r1", nil)},
		frames: map[string][]dockerx.LogFrame{"c1": {
			{Stream: "stdout", Time: at(0), Line: "hello"},
			{Stream: "stdout", Time: at(2 * time.Millisecond), Line: "world"},
		}},
	}
	shipper := newShipper(t, source, database, sink.URL)

	shipper.ship(context.Background())

	got := sink.lines()
	if len(got) != 1 || got[0] != "world" {
		t.Fatalf("pushed %v, want only the line after the checkpoint", got)
	}
}

func TestShipIgnoresSystemHelperAndUnmanagedContainers(t *testing.T) {
	sink := newSink(t)
	database := testStore(t)
	helper := container("c3", "asgard-demo-app-helper-r1", map[string]string{"com.rousoftware.asgard.helper": "true"})
	stopped := container("c2", "asgard-demo-app-r0", nil)
	stopped.State = "exited"
	unmanaged := dockerx.Container{ID: "c4", Name: "someone-elses", State: "running", Managed: false, Labels: map[string]string{}}
	// The compose services that run the collector itself are managed but belong
	// to no project; collecting them would file Loki's log inside Loki.
	system := dockerx.Container{
		ID: "c5", Name: "asgard-loki-1", State: "running", Managed: true,
		Labels: map[string]string{
			"com.rousoftware.asgard.managed": "true",
			"com.rousoftware.asgard.system":  "log-collector",
		},
	}
	source := &fakeSource{
		containers: []dockerx.Container{container("c1", "asgard-demo-app-r1", nil), stopped, helper, unmanaged, system},
		frames:     map[string][]dockerx.LogFrame{},
	}
	for _, id := range []string{"c2", "c3", "c4", "c5"} {
		if err := database.SetLogCursor(context.Background(), store.LogCursor{ContainerID: id, Time: at(0), Stream: "stdout", Line: "stale"}); err != nil {
			t.Fatalf("seed cursor %s: %v", id, err)
		}
	}
	shipper := newShipper(t, source, database, sink.URL)

	shipper.ship(context.Background())

	if len(source.queried) != 1 || source.queried[0] != "c1" {
		t.Fatalf("queried %v, want only the running container of a project", source.queried)
	}
	// A stopped container of a project keeps its checkpoint: its id survives a
	// restart, so forgetting it would make the next pass re-read and re-send
	// everything in the lookback window.
	if _, found, err := database.LogCursor(context.Background(), "c2"); err != nil || !found {
		t.Errorf("checkpoint for the stopped project container was dropped, err=%v", err)
	}
	for _, id := range []string{"c3", "c4", "c5"} {
		if _, found, err := database.LogCursor(context.Background(), id); err != nil || found {
			t.Errorf("checkpoint for %s survived, want it pruned", id)
		}
	}
}

// A container that no longer exists must not leave a checkpoint behind: every
// release creates new container ids, so without pruning the table grows by one
// row per service per deployment forever.
func TestShipForgetsCheckpointsOfRemovedContainers(t *testing.T) {
	sink := newSink(t)
	database := testStore(t)
	source := &fakeSource{
		containers: []dockerx.Container{container("c1", "asgard-demo-app-r1", nil)},
		frames:     map[string][]dockerx.LogFrame{},
	}
	gone := "container-from-an-earlier-release"
	if err := database.SetLogCursor(context.Background(), store.LogCursor{ContainerID: gone, Time: at(0), Stream: "stdout", Line: "old"}); err != nil {
		t.Fatalf("seed cursor: %v", err)
	}
	shipper := newShipper(t, source, database, sink.URL)

	shipper.ship(context.Background())

	if _, found, err := database.LogCursor(context.Background(), gone); err != nil || found {
		t.Fatalf("checkpoint survived for a container that is gone, err=%v", err)
	}
}

// A pass that dies partway through a burst must resume at the first unsent
// line: everything before it is acknowledged, nothing after it is lost.
func TestShipResumesFromTheLastAcceptedBatch(t *testing.T) {
	sink := newSink(t)
	// Every attempt at the second batch is refused, so the pass ends there
	// rather than being rescued by a retry.
	for _, request := range []int{2, 3, 4} {
		sink.fail(request, http.StatusBadGateway)
	}
	database := testStore(t)
	source := &fakeSource{
		containers: []dockerx.Container{container("c1", "asgard-demo-app-r1", nil)},
		frames: map[string][]dockerx.LogFrame{"c1": {
			{Stream: "stdout", Time: at(0), Line: "first"},
			{Stream: "stdout", Time: at(time.Millisecond), Line: "second"},
			{Stream: "stdout", Time: at(2 * time.Millisecond), Line: "third"},
		}},
	}
	shipper := newShipper(t, source, database, sink.URL)
	// One frame per batch: the byte budget is the only thing splitting them.
	shipper.BatchBytes = int64(len("first") + frameOverhead + 1)

	shipper.ship(context.Background())

	cursor, found, err := database.LogCursor(context.Background(), "c1")
	if err != nil || !found {
		t.Fatalf("cursor found=%v err=%v, want the first batch acknowledged", found, err)
	}
	if cursor.Line != "first" {
		t.Fatalf("cursor line = %q, want the first batch's line", cursor.Line)
	}

	sink.acceptEverything()
	shipper.ship(context.Background())

	got := sink.lines()
	want := []string{"first", "second", "third"}
	if len(got) != len(want) {
		t.Fatalf("pushed %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pushed %v, want %v", got, want)
		}
	}
	// The second pass read from the frame the first pass acknowledged, not from
	// scratch and not from the lookback window.
	if since := source.sinces["c1"]; !since.Equal(at(0)) {
		t.Errorf("second pass read from %s, want the last acknowledged frame %s", since, at(0))
	}
}

// The first read of a container with no checkpoint is bounded by the lookback,
// not by the container's whole history.
func TestShipStartsFromLookbackWithoutCheckpoint(t *testing.T) {
	sink := newSink(t)
	database := testStore(t)
	source := &fakeSource{
		containers: []dockerx.Container{container("c1", "asgard-demo-app-r1", nil)},
		frames:     map[string][]dockerx.LogFrame{},
	}
	shipper := newShipper(t, source, database, sink.URL)
	shipper.Lookback = 10 * time.Minute

	before := time.Now().UTC()
	shipper.ship(context.Background())

	since := source.sinces["c1"]
	if since.Before(before.Add(-11*time.Minute)) || since.After(before.Add(-9*time.Minute)) {
		t.Fatalf("first read started at %s, want about ten minutes back from %s", since, before)
	}
}
