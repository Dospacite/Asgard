package composecfg

import (
	"fmt"
	"strings"

	"github.com/rousoftware/asgard/internal/store"
)

// One definition of the mutable service settings, shared by every surface that
// accepts them.
//
// The REST API and the MCP server both write to the same store, so any rule
// either of them enforces alone is a rule the other can be used to bypass.
// That had already happened: the hostname check existed in three places with
// three behaviours, and the MCP tool validated no resource limit, restart
// policy, or health path at all — an agent could set a 0.01-CPU limit that the
// browser rejected on the same field. The rules live here now and both
// surfaces call Normalize.

// Bounds on the resource limits a service may be given. They are deliberately
// wide: the point is to reject nonsense, not to second-guess the operator.
const (
	MinCPULimit    = 0.05
	MaxCPULimit    = 64
	MinMemoryLimit = 32 << 20
	MaxMemoryLimit = 256 << 30
	MinPIDsLimit   = 16
	MaxPIDsLimit   = 32768
)

// SettingsError carries a stable machine code alongside the operator-facing
// message so an HTTP handler can map it to a status without re-deriving which
// rule failed from the text.
type SettingsError struct {
	Code    string
	Message string
}

func (e *SettingsError) Error() string { return e.Message }

func settingsError(code, message string) error { return &SettingsError{Code: code, Message: message} }

// ServiceSettings is the half of a service an operator or agent may change
// after import. Everything else — name, image, build context, volumes — comes
// from the Compose file and is replaced by a deployment.
type ServiceSettings struct {
	Role          string
	Environment   map[string]string
	Public        bool
	Port          int
	Hostname      string
	HealthPath    string
	HSTSMode      string
	CPULimit      float64
	MemoryLimit   int64
	PIDsLimit     int64
	RestartPolicy string
}

// Normalize canonicalizes the settings in place and reports the first rule
// they break. Callers pass the settings they were given and use them only if
// Normalize returns nil, so a partially-applied update is impossible.
func (s *ServiceSettings) Normalize(controlPlaneDomain string) error {
	if err := ValidateRole(s.Role); err != nil {
		return err
	}
	if err := ValidateEnvironment(s.Environment); err != nil {
		return settingsError("invalid_environment", err.Error())
	}
	if err := ValidateResourceLimits(s.CPULimit, s.MemoryLimit, s.PIDsLimit); err != nil {
		return err
	}
	if err := ValidateRestartPolicy(s.RestartPolicy); err != nil {
		return err
	}
	healthPath, err := NormalizeHealthPath(s.HealthPath)
	if err != nil {
		return err
	}
	s.HealthPath = healthPath
	hostname, err := NormalizeRoute(s.Public, s.Port, s.Hostname, controlPlaneDomain)
	if err != nil {
		return err
	}
	s.Hostname = hostname
	if !store.ValidHSTSMode(s.HSTSMode) {
		return settingsError("invalid_hsts_mode", "HSTS mode must be empty (decide from the hostname's zone), standard, strict, or off.")
	}
	return nil
}

// ValidateRole checks the workload shape a service declares.
func ValidateRole(role string) error {
	switch role {
	case "web", "worker", "stateful":
		return nil
	}
	return settingsError("invalid_role", "Role must be web, worker, or stateful.")
}

// ValidateRestartPolicy checks the Docker restart policy a service runs under.
func ValidateRestartPolicy(policy string) error {
	switch policy {
	case "no", "always", "on-failure", "unless-stopped":
		return nil
	}
	return settingsError("invalid_restart_policy", "Restart policy must be no, always, on-failure, or unless-stopped.")
}

// ValidateResourceLimits checks the per-container quota a service runs under.
func ValidateResourceLimits(cpu float64, memory, pids int64) error {
	if cpu < MinCPULimit || cpu > MaxCPULimit {
		return settingsError("invalid_resources", fmt.Sprintf("CPU limit must be between %g and %g cores.", float64(MinCPULimit), float64(MaxCPULimit)))
	}
	if memory < MinMemoryLimit || memory > MaxMemoryLimit {
		return settingsError("invalid_resources", "Memory limit must be between 32 MiB and 256 GiB.")
	}
	if pids < MinPIDsLimit || pids > MaxPIDsLimit {
		return settingsError("invalid_resources", fmt.Sprintf("PID limit must be between %d and %d.", MinPIDsLimit, MaxPIDsLimit))
	}
	return nil
}

// NormalizeHealthPath defaults an empty health path to the site root and
// rejects anything that is not a rooted path.
func NormalizeHealthPath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "/", nil
	}
	if !strings.HasPrefix(value, "/") {
		return "", settingsError("invalid_health_path", "Health path must begin with /.")
	}
	return value, nil
}

// NormalizeRoute checks the port and hostname of a service that wants a public
// HTTPS route and returns the canonical hostname. A private service keeps
// whatever hostname it has without it being checked — no route is generated for
// it either way, and preserving the value means toggling a service private and
// public again does not lose the name the operator chose.
func NormalizeRoute(public bool, port int, hostname, controlPlaneDomain string) (string, error) {
	hostname = strings.ToLower(strings.TrimSpace(hostname))
	if !public {
		return hostname, nil
	}
	if port < 1 || port > 65535 {
		return "", settingsError("invalid_port", "A public service needs an internal port between 1 and 65535.")
	}
	switch err := ValidatePublicHostname(hostname, controlPlaneDomain); {
	case err == nil:
		return hostname, nil
	case err == ErrHostnameReserved:
		return "", settingsError("reserved_hostname", "The control-plane hostname is reserved.")
	default:
		return "", settingsError("invalid_hostname", "Hostname must be a fully qualified DNS name, such as app.example.com, whose DNS points at this host.")
	}
}
