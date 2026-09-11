package dockerx

import (
	"context"
	"encoding/binary"
	"io"
	"strings"
	"time"

	"github.com/moby/moby/client"
)

// LogFrame is one line of container output together with the stream it arrived
// on and the time the daemon stamped it with.
type LogFrame struct {
	Stream string
	Time   time.Time
	Line   string
}

// LogFrames reads the output a container produced after since and returns it as
// timestamped frames.
//
// A container without a TTY has its output framed: each chunk carries an 8-byte
// header naming stdout or stderr and the chunk length. Demultiplexing here
// rather than joining the two streams is what lets a collector label a line as
// stdout or stderr, which is the difference between reading an error and
// guessing at one.
func (e *Engine) LogFrames(ctx context.Context, id string, since time.Time, maxBytes int64) ([]LogFrame, error) {
	if maxBytes <= 0 {
		maxBytes = 4 << 20
	}
	options := client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Timestamps: true}
	if !since.IsZero() {
		options.Since = since.UTC().Format(time.RFC3339Nano)
	}
	result, err := e.client.ContainerLogs(ctx, id, options)
	if err != nil {
		return nil, err
	}
	defer result.Close()
	// The limit bounds one read, not the container's history: the caller keeps a
	// checkpoint and resumes from it, so a service that logs faster than the
	// collector drains simply takes several passes.
	raw, err := io.ReadAll(io.LimitReader(result, maxBytes))
	if err != nil {
		return nil, err
	}
	return splitFrames(raw), nil
}

func splitFrames(raw []byte) []LogFrame {
	frames := make([]LogFrame, 0, 16)
	framed := false
	for len(raw) >= 8 && (raw[0] == 1 || raw[0] == 2) {
		size := int(binary.BigEndian.Uint32(raw[4:8]))
		if size < 0 || 8+size > len(raw) {
			// A half-read frame at the byte limit is not corruption, it is the
			// edge of this pass; the next one starts from the checkpoint.
			break
		}
		framed = true
		frames = appendLines(frames, streamName(raw[0]), raw[8:8+size])
		raw = raw[8+size:]
	}
	if !framed {
		frames = appendLines(frames, "stdout", raw)
	}
	return frames
}

func streamName(header byte) string {
	if header == 2 {
		return "stderr"
	}
	return "stdout"
}

func appendLines(frames []LogFrame, stream string, payload []byte) []LogFrame {
	text := strings.TrimSuffix(string(payload), "\n")
	if text == "" {
		return frames
	}
	for _, line := range strings.Split(text, "\n") {
		stamp, message, ok := splitStamp(line)
		if !ok {
			// The daemon stamps every record it writes. A line without a stamp
			// is the second line of a multi-line write — a stack trace, most
			// often — and belongs to the record before it.
			if len(frames) > 0 && frames[len(frames)-1].Stream == stream {
				frames[len(frames)-1].Line += "\n" + line
			}
			continue
		}
		frames = append(frames, LogFrame{Stream: stream, Time: stamp, Line: message})
	}
	return frames
}

// splitStamp separates the RFC3339Nano timestamp the daemon prefixes to each
// record when timestamps are requested.
func splitStamp(line string) (time.Time, string, bool) {
	index := strings.IndexByte(line, ' ')
	if index <= 0 {
		return time.Time{}, "", false
	}
	stamp, err := time.Parse(time.RFC3339Nano, line[:index])
	if err != nil {
		return time.Time{}, "", false
	}
	return stamp, line[index+1:], true
}
