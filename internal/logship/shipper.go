// Package logship copies container output to a log store.
//
// Asgard cannot run the usual host-side shippers. Its Compose contract rejects
// Docker socket mounts and host bind mounts outside the project, so nothing in
// the workload zone can reach /var/lib/docker/containers — and Promtail, Alloy
// and Fluent Bit are all built on reading exactly that. The control plane is
// the one process that already holds a Docker client for the whole host, so the
// tailing happens here and the frames are pushed to whatever accepts a Loki
// push API.
package logship

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rousoftware/asgard/internal/dockerx"
	"github.com/rousoftware/asgard/internal/store"
)

// LogSource is the part of the Docker engine this package reads.
type LogSource interface {
	Containers(ctx context.Context, all bool) ([]dockerx.Container, error)
	LogFrames(ctx context.Context, id string, since time.Time, maxBytes int64) ([]dockerx.LogFrame, error)
}

// Cursors is the part of the store this package needs.
type Cursors interface {
	LogCursor(ctx context.Context, containerID string) (store.LogCursor, bool, error)
	SetLogCursor(ctx context.Context, cursor store.LogCursor) error
	PruneLogCursors(ctx context.Context, keep []string) (int64, error)
}

// Shipper walks every running managed container on an interval and pushes what
// each one wrote since the last pass.
type Shipper struct {
	Docker     LogSource
	Store      Cursors
	Sink       string
	Interval   time.Duration
	Lookback   time.Duration
	BatchBytes int64
	// RetryDelay spaces the attempts made within one pass while the sink is
	// unreachable. The next pass retries anything this one gave up on.
	RetryDelay time.Duration
	Client     *http.Client
	Logger     *slog.Logger

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Start runs the shipper until the parent context is cancelled. An empty sink
// disables it: an install without a collector should not accumulate
// checkpoints for containers nobody reads.
func (s *Shipper) Start(parent context.Context) {
	if s.Sink == "" || s.Interval <= 0 || s.Docker == nil || s.Store == nil {
		return
	}
	if s.Lookback <= 0 {
		s.Lookback = 5 * time.Minute
	}
	if s.BatchBytes <= 0 {
		s.BatchBytes = 1 << 20
	}
	if s.Client == nil {
		s.Client = &http.Client{Timeout: 20 * time.Second}
	}
	if s.Logger == nil {
		s.Logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		// One interval of quiet after startup: a control plane restart is not a
		// reason to hammer the sink before it has finished coming up itself.
		timer := time.NewTimer(s.Interval)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		s.ship(ctx)
		ticker := time.NewTicker(s.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.ship(ctx)
			}
		}
	}()
}

func (s *Shipper) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
}

// ship performs one pass. A container that fails is skipped and retried next
// pass: its checkpoint has not moved, so nothing is lost.
func (s *Shipper) ship(ctx context.Context) {
	containers, err := s.Docker.Containers(ctx, false)
	if err != nil {
		s.Logger.Warn("log shipping skipped", "error", err)
		return
	}
	keep := make([]string, 0, len(containers))
	lines := 0
	for _, container := range containers {
		if !collectable(container) {
			// Nothing here is ever shipped, so a checkpoint for it is stale and
			// its container is about to be forgotten below.
			continue
		}
		// A container that is not running right now keeps its checkpoint: a
		// restart is not a new container, and dropping the checkpoint would
		// re-read its lookback window and write the same lines twice.
		keep = append(keep, container.ID)
		if !shippable(container) {
			continue
		}
		frames, err := s.pendingFrames(ctx, container)
		if err != nil {
			s.Logger.Warn("container logs could not be read", "container", container.Name, "error", err)
			continue
		}
		shipped, err := s.push(ctx, container, frames)
		lines += shipped
		if err != nil {
			s.Logger.Warn("container logs could not be shipped", "container", container.Name, "error", err)
		}
	}
	if _, err := s.Store.PruneLogCursors(ctx, keep); err != nil {
		s.Logger.Warn("stale log checkpoints could not be pruned", "error", err)
	}
	if lines > 0 {
		s.Logger.Info("container logs shipped", "lines", lines)
	}
}

// collectable reports whether a container is one whose output this system
// collects at all: a container of a project, rather than one of the scripts a
// deployment runs to migrate or seed.
//
// The services in deploy/compose.yaml — the control plane, Traefik, Loki,
// Grafana — carry the managed label too, so the managed flag alone is not
// enough. Collecting them would put Loki's own output inside Loki, fill the
// store with Traefik's access log, and file the control plane's stdout under a
// project that does not exist. The project label is what distinguishes a tenant
// of this host from the machinery running it.
func collectable(container dockerx.Container) bool {
	if !container.Managed {
		return false
	}
	if container.Labels["com.rousoftware.asgard.project"] == "" {
		return false
	}
	return container.Labels["com.rousoftware.asgard.helper"] != "true"
}

// shippable narrows collectable to the containers that have output to read
// right now.
func shippable(container dockerx.Container) bool {
	return collectable(container) && container.State == "running"
}

func (s *Shipper) pendingFrames(ctx context.Context, container dockerx.Container) ([]dockerx.LogFrame, error) {
	cursor, found, err := s.Store.LogCursor(ctx, container.ID)
	if err != nil {
		return nil, err
	}
	since := time.Now().UTC().Add(-s.Lookback)
	if found {
		since = cursor.Time
	}
	frames, err := s.Docker.LogFrames(ctx, container.ID, since, s.BatchBytes)
	if err != nil {
		return nil, err
	}
	return dropShipped(frames, cursor, found), nil
}

// dropShipped removes the record the checkpoint already covers. Docker's since
// bound is inclusive, so without this every pass would resend one line per
// container and each retry would compound it.
func dropShipped(frames []dockerx.LogFrame, cursor store.LogCursor, found bool) []dockerx.LogFrame {
	if !found {
		return frames
	}
	kept := frames[:0]
	for _, frame := range frames {
		if frame.Time.Before(cursor.Time) {
			continue
		}
		if frame.Time.Equal(cursor.Time) && frame.Stream == cursor.Stream && clip(frame.Line) == cursor.Line {
			continue
		}
		kept = append(kept, frame)
	}
	return kept
}

// push sends frames in batches and advances the checkpoint after each one, so a
// sink that fails halfway through a burst resumes at the first unsent line
// instead of replaying the whole pass.
func (s *Shipper) push(ctx context.Context, container dockerx.Container, frames []dockerx.LogFrame) (int, error) {
	labels := streamLabels(container)
	shipped := 0
	for start := 0; start < len(frames); {
		end := start
		size := 0
		for end < len(frames) && (end == start || size+len(frames[end].Line) <= int(s.BatchBytes)) {
			size += len(frames[end].Line) + frameOverhead
			end++
		}
		batch := frames[start:end]
		if err := s.pushBatch(ctx, labels, batch); err != nil {
			return shipped, err
		}
		last := batch[len(batch)-1]
		if err := s.Store.SetLogCursor(ctx, store.LogCursor{ContainerID: container.ID, Time: last.Time, Stream: last.Stream, Line: clip(last.Line)}); err != nil {
			return shipped, err
		}
		shipped += len(batch)
		start = end
	}
	return shipped, nil
}

// frameOverhead approximates the JSON around each line so a batch does not grow
// past the byte budget by exactly the bytes that are not the message.
const frameOverhead = 48

// cursorLineLimit bounds what a checkpoint stores of a line. The checkpoint
// exists to recognise a repeat, not to keep a second copy of the log, and a
// service that writes megabyte lines should not grow the database by them.
const cursorLineLimit = 512

func clip(line string) string {
	if len(line) <= cursorLineLimit {
		return line
	}
	clipped := line[:cursorLineLimit]
	for len(clipped) > 0 && !utf8.ValidString(clipped) {
		clipped = clipped[:len(clipped)-1]
	}
	return clipped
}

func streamLabels(container dockerx.Container) map[string]string {
	project := container.Labels["com.rousoftware.asgard.project"]
	service := container.Labels["com.rousoftware.asgard.service"]
	if service == "" {
		service = container.Name
	}
	return map[string]string{"project": project, "service": service}
}

type lokiStream struct {
	Stream map[string]string `json:"stream"`
	Values [][2]string       `json:"values"`
}

type lokiPush struct {
	Streams []lokiStream `json:"streams"`
}

func (s *Shipper) pushBatch(ctx context.Context, labels map[string]string, batch []dockerx.LogFrame) error {
	values := map[string][][2]string{}
	order := make([]string, 0, 2)
	for _, frame := range batch {
		if _, seen := values[frame.Stream]; !seen {
			order = append(order, frame.Stream)
		}
		values[frame.Stream] = append(values[frame.Stream], [2]string{strconv.FormatInt(frame.Time.UnixNano(), 10), frame.Line})
	}
	streams := make([]lokiStream, 0, len(order))
	for _, name := range order {
		series := map[string]string{"stream": name}
		for key, value := range labels {
			series[key] = value
		}
		streams = append(streams, lokiStream{Stream: series, Values: values[name]})
	}
	body, err := json.Marshal(lokiPush{Streams: streams})
	if err != nil {
		return err
	}
	var lastErr error
	delay := s.RetryDelay
	if delay <= 0 {
		delay = 750 * time.Millisecond
	}
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * delay):
			}
		}
		if lastErr = s.post(ctx, body); lastErr == nil {
			return nil
		}
	}
	return lastErr
}

func (s *Shipper) post(ctx context.Context, body []byte) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.Sink, "/")+"/loki/api/v1/push", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := s.Client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	message, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("log sink returned %s: %s", response.Status, strings.TrimSpace(string(message)))
	}
	return nil
}
