package dockerx

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The fixture is a real BuildKit progress stream from Docker 29 for a
// two-stage file whose first stage prints a line from a RUN step. The release
// log must name each step once and carry the step's own output.
func TestBuildKitProgressRendersStepsAndOutput(t *testing.T) {
	file, err := os.Open("testdata/buildkit-trace.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	lines := []string{}
	progress := newBuildKitProgress(func(line string) { lines = append(lines, line) })
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var message struct {
			ID  string          `json:"id"`
			Aux json.RawMessage `json:"aux"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			t.Fatal(err)
		}
		if message.ID == buildKitTraceID {
			progress.handle(message.Aux)
		}
	}
	progress.flush()
	log := strings.Join(lines, "\n")
	for _, want := range []string{"[build 2/2] RUN --mount=type=cache,target=/root/.cache echo building > /out.txt && echo \"stage log line\"", "stage log line", "COPY hello.txt /hello.txt"} {
		if !strings.Contains(log, want) {
			t.Fatalf("log is missing %q:\n%s", want, log)
		}
	}
	seen := map[string]bool{}
	for _, line := range lines {
		if strings.HasPrefix(line, "[") && seen[line] {
			t.Fatalf("step announced twice: %q\n%s", line, log)
		}
		seen[line] = true
	}
}

// A truncated or garbled payload is dropped rather than panicking the build.
func TestBuildKitProgressIgnoresMalformedPayloads(t *testing.T) {
	progress := newBuildKitProgress(func(string) { t.Fatal("nothing should be logged") })
	for _, aux := range []string{`"CgUKA2Fi"`, `"/////w=="`, `"not base64"`, `12`} {
		progress.handle(json.RawMessage(aux))
	}
}
