package dockerx

import (
	"encoding/binary"
	"testing"
	"time"
)

func frame(stream byte, payload string) []byte {
	out := make([]byte, 8+len(payload))
	out[0] = stream
	binary.BigEndian.PutUint32(out[4:8], uint32(len(payload)))
	copy(out[8:], payload)
	return out
}

func TestSplitFramesDemultiplexesStreamsWithTimestamps(t *testing.T) {
	raw := append(frame(1, "2026-01-02T03:04:05.000000001Z starting up\n"), frame(2, "2026-01-02T03:04:06Z connection refused\n")...)

	frames := splitFrames(raw)

	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2: %+v", len(frames), frames)
	}
	if frames[0].Stream != "stdout" || frames[0].Line != "starting up" {
		t.Errorf("first frame = %+v, want stdout/starting up", frames[0])
	}
	if frames[1].Stream != "stderr" || frames[1].Line != "connection refused" {
		t.Errorf("second frame = %+v, want stderr/connection refused", frames[1])
	}
	want := time.Date(2026, 1, 2, 3, 4, 5, 1, time.UTC)
	if !frames[0].Time.Equal(want) {
		t.Errorf("first frame time = %s, want %s", frames[0].Time, want)
	}
}

// A single write of several lines is one record with one timestamp; the lines
// after the first belong to it rather than being lost.
func TestSplitFramesKeepsMultiLineRecordsTogether(t *testing.T) {
	raw := frame(2, "2026-01-02T03:04:05Z panic: boom\n\ngoroutine 1 [running]:\nmain.main()\n")

	frames := splitFrames(raw)

	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1: %+v", len(frames), frames)
	}
	want := "panic: boom\n\ngoroutine 1 [running]:\nmain.main()"
	if frames[0].Line != want {
		t.Errorf("frame line = %q, want %q", frames[0].Line, want)
	}
}

// A container with a TTY gets no framing, so the payload is plain text.
func TestSplitFramesFallsBackToPlainText(t *testing.T) {
	frames := splitFrames([]byte("2026-01-02T03:04:05Z no frames here\n"))

	if len(frames) != 1 || frames[0].Line != "no frames here" || frames[0].Stream != "stdout" {
		t.Fatalf("frames = %+v, want one stdout frame reading 'no frames here'", frames)
	}
}

// The byte limit can cut a frame header in half; that is the edge of a pass,
// not corruption to be parsed as text.
func TestSplitFramesStopsOnPartialFrame(t *testing.T) {
	raw := frame(1, "2026-01-02T03:04:05Z complete\n")
	raw = append(raw, 1, 0, 0, 0, 0, 0, 1, 0)

	frames := splitFrames(raw)

	if len(frames) != 1 || frames[0].Line != "complete" {
		t.Fatalf("frames = %+v, want only the complete record", frames)
	}
}
