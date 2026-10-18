package composecfg

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Compose accepts several spellings for the quantities Asgard needs as numbers
// — `mem_limit: 512m`, `memory: 512M`, `shm_size: 1gb`, `stop_grace_period:
// 1m30s`, `start_period: 30s`, `cpus: "1.5"` — and every one of them used to be
// a parse error, because a value that arriva as a mapping or a string was
// compared against a numeric field. The conversions live here so the parsing
// rules are stated once.

// byteSuffixes maps the multipliers Compose accepts to their factor. Compose
// uses binary multiples for every suffix: `1k` and `1kb` and `1KiB` are all
// 1024.
var byteSuffixes = map[string]int64{
	"": 1, "b": 1,
	"k": 1 << 10, "kb": 1 << 10, "kib": 1 << 10,
	"m": 1 << 20, "mb": 1 << 20, "mib": 1 << 20,
	"g": 1 << 30, "gb": 1 << 30, "gib": 1 << 30,
	"t": 1 << 40, "tb": 1 << 40, "tib": 1 << 40,
	"p": 1 << 50, "pb": 1 << 50, "pib": 1 << 50,
}

// parseByteSize converts a Compose size into bytes. YAML gives `512` as an int
// and `512m` as a string, so both arrive here.
func parseByteSize(value any) (int64, error) {
	switch raw := value.(type) {
	case int:
		return int64(raw), nil
	case int64:
		return raw, nil
	case uint64:
		if raw > math.MaxInt64 {
			return 0, errors.New("size is too large")
		}
		return int64(raw), nil
	case float64:
		return int64(raw), nil
	default:
		text := strings.ToLower(strings.TrimSpace(fmt.Sprint(value)))
		if text == "" {
			return 0, errors.New("size is empty")
		}
		unit := ""
		for _, candidate := range []string{"pib", "tib", "gib", "mib", "kib", "pb", "tb", "gb", "mb", "kb", "p", "t", "g", "m", "k", "b"} {
			if strings.HasSuffix(text, candidate) {
				unit = candidate
				text = strings.TrimSuffix(text, candidate)
				break
			}
		}
		number, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
		if err != nil || number < 0 {
			return 0, fmt.Errorf("%q is not a size such as 512m or 1g", fmt.Sprint(value))
		}
		return int64(number * float64(byteSuffixes[unit])), nil
	}
}

// parseCPUs converts a CPU allowance into cores. Compose accepts a decimal
// number of cores, and Docker also accepts the `m` milli form.
func parseCPUs(value any) (float64, error) {
	text := strings.TrimSpace(fmt.Sprint(value))
	if strings.HasSuffix(text, "m") {
		milli, err := strconv.ParseFloat(strings.TrimSuffix(text, "m"), 64)
		if err != nil || milli <= 0 {
			return 0, fmt.Errorf("%q is not a CPU allowance", fmt.Sprint(value))
		}
		return milli / 1000, nil
	}
	cores, err := strconv.ParseFloat(text, 64)
	if err != nil || cores <= 0 {
		return 0, fmt.Errorf("%q is not a CPU allowance such as 0.5 or 2", fmt.Sprint(value))
	}
	return cores, nil
}

// parseCount converts an integer setting, tolerating the quoted form YAML
// produces for a value that came from an interpolated variable.
func parseCount(value any) (int64, error) {
	switch raw := value.(type) {
	case int:
		return int64(raw), nil
	case int64:
		return raw, nil
	case float64:
		return int64(raw), nil
	default:
		number, err := strconv.ParseInt(strings.TrimSpace(fmt.Sprint(value)), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%q is not a whole number", fmt.Sprint(value))
		}
		return number, nil
	}
}

// parseDuration converts a Compose duration. Compose writes them with a unit
// ("30s", "1m30s"); a bare number is read as seconds because that is the only
// reading that is ever meant.
func parseDuration(value any) (time.Duration, error) {
	if number, ok := value.(int); ok {
		return time.Duration(number) * time.Second, nil
	}
	if number, ok := value.(int64); ok {
		return time.Duration(number) * time.Second, nil
	}
	text := strings.TrimSpace(fmt.Sprint(value))
	if text == "" {
		return 0, errors.New("duration is empty")
	}
	if duration, err := time.ParseDuration(text); err == nil {
		if duration < 0 {
			return 0, errors.New("duration must not be negative")
		}
		return duration, nil
	}
	seconds, err := strconv.ParseFloat(text, 64)
	if err != nil || seconds < 0 {
		return 0, fmt.Errorf("%q is not a duration such as 30s, 500ms, or 1m30s", text)
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

// portNumber extracts the container-side port from one entry of `ports` or
// `expose`, in either the short form ("8080:80", "127.0.0.1:8080:80", "80/udp",
// "8000-8010:8000-8010") or the long form ({target: 80, published: 8080}).
//
// The container port — not the published one — is what Asgard routes to: it
// never publishes a port on the host, Traefik reaches the container on the
// project network.
func portNumber(value any) (int, bool) {
	switch entry := value.(type) {
	case map[string]any:
		raw, ok := entry["target"]
		if !ok {
			return 0, false
		}
		return portValue(fmt.Sprint(raw))
	default:
		return portValue(fmt.Sprint(value))
	}
}

func portValue(text string) (int, bool) {
	text = strings.Split(text, "/")[0]
	parts := strings.Split(text, ":")
	candidate := parts[len(parts)-1]
	if strings.Contains(candidate, "-") {
		candidate = strings.Split(candidate, "-")[0]
	}
	port, err := strconv.Atoi(strings.TrimSpace(candidate))
	if err != nil || port <= 0 || port > 65535 {
		return 0, false
	}
	return port, true
}

// normalizeRestart validates a restart policy and canonicalizes the
// `on-failure` form, which Compose lets carry a retry budget
// (`on-failure:5`). The budget is kept in the stored policy so the container
// is created with it.
func normalizeRestart(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unless-stopped", nil
	}
	name, budget, hasBudget := strings.Cut(value, ":")
	switch name {
	case "no", "always", "unless-stopped":
		if hasBudget {
			return "", errors.New("only on-failure takes a retry budget, as on-failure:5")
		}
		return name, nil
	case "on-failure":
		if !hasBudget {
			return name, nil
		}
		retries, err := strconv.Atoi(strings.TrimSpace(budget))
		if err != nil || retries < 1 {
			return "", errors.New("on-failure's retry budget must be a positive number, as on-failure:5")
		}
		return name + ":" + strconv.Itoa(retries), nil
	}
	return "", errors.New("unsupported restart policy; use no, always, on-failure[:retries], or unless-stopped")
}

// RestartPolicy splits a stored restart policy into the container's policy name
// and retry budget.
func RestartPolicy(value string) (string, int) {
	name, budget, hasBudget := strings.Cut(value, ":")
	if !hasBudget {
		return name, 0
	}
	retries, err := strconv.Atoi(budget)
	if err != nil {
		return name, 0
	}
	return name, retries
}
