package composecfg

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/rousoftware/asgard/internal/store"
	"gopkg.in/yaml.v3"
)

const MaxComposeBytes = 1 << 20

type Document struct {
	Name     string                `yaml:"name"`
	Services map[string]RawService `yaml:"services"`
	Volumes  map[string]any        `yaml:"volumes"`
	Asgard   AsgardConfig          `yaml:"x-asgard"`
}

type AsgardConfig struct {
	PrimaryService string                         `yaml:"primary-service"`
	Services       map[string]AsgardServiceConfig `yaml:"services"`
}

type AsgardServiceConfig struct {
	Role       string `yaml:"role"`
	Public     *bool  `yaml:"public"`
	Port       int    `yaml:"port"`
	HealthPath string `yaml:"health-path"`
	Hostname   string `yaml:"hostname"`
}

type RawService struct {
	Image       string      `yaml:"image"`
	Build       any         `yaml:"build"`
	Command     any         `yaml:"command"`
	Environment any         `yaml:"environment"`
	EnvFile     any         `yaml:"env_file"`
	Ports       []any       `yaml:"ports"`
	Expose      []any       `yaml:"expose"`
	Volumes     []string    `yaml:"volumes"`
	DependsOn   any         `yaml:"depends_on"`
	Healthcheck Healthcheck `yaml:"healthcheck"`
	Restart     string      `yaml:"restart"`
}

type Healthcheck struct {
	Test        any    `yaml:"test"`
	Interval    string `yaml:"interval"`
	Timeout     string `yaml:"timeout"`
	Retries     int    `yaml:"retries"`
	StartPeriod string `yaml:"start_period"`
}

type Build struct {
	Context    string
	Dockerfile string
	Args       map[string]string
	Target     string
}

type ValidationError struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}
type ValidationResult struct {
	Valid          bool              `json:"valid"`
	Errors         []ValidationError `json:"errors"`
	Warnings       []ValidationError `json:"warnings"`
	Services       []store.Service   `json:"services"`
	PrimaryService string            `json:"primaryService"`
}

var slugPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
var servicePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)

func Slug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var out strings.Builder
	dash := false
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			out.WriteRune(r)
			dash = false
		} else if !dash && out.Len() > 0 {
			out.WriteByte('-')
			dash = true
		}
	}
	result := strings.Trim(out.String(), "-")
	if len(result) > 63 {
		result = strings.TrimRight(result[:63], "-")
	}
	return result
}

func ValidateSlug(value string) bool { return slugPattern.MatchString(value) }

func Parse(data []byte, projectID, projectSlug, root string) (Document, ValidationResult) {
	result := ValidationResult{Valid: false, Errors: []ValidationError{}, Warnings: []ValidationError{}, Services: []store.Service{}}
	if len(data) == 0 {
		result.Errors = append(result.Errors, ValidationError{"compose", "Compose file is empty."})
		return Document{}, result
	}
	if len(data) > MaxComposeBytes {
		result.Errors = append(result.Errors, ValidationError{"compose", "Compose file exceeds 1 MiB."})
		return Document{}, result
	}
	var rootNode yaml.Node
	if err := yaml.Unmarshal(data, &rootNode); err != nil {
		result.Errors = append(result.Errors, ValidationError{"compose", err.Error()})
		return Document{}, result
	}
	validateKeys(&rootNode, resultErrorCollector(&result), []string{"name", "version", "services", "volumes", "networks", "x-asgard"}, "compose")
	missing := map[string]bool{}
	if err := interpolateNode(&rootNode, interpolationValues(root), missing); err != nil {
		result.Errors = append(result.Errors, ValidationError{"compose", err.Error()})
		return Document{}, result
	}
	if len(missing) > 0 {
		result.Warnings = append(result.Warnings, ValidationError{"compose", "Substituted an empty value for undefined variables: " + strings.Join(sortedNames(missing), ", ") + ". Define them in the project's .env file or give them a default."})
	}
	var doc Document
	if err := rootNode.Decode(&doc); err != nil {
		result.Errors = append(result.Errors, ValidationError{"compose", err.Error()})
		return doc, result
	}
	if len(doc.Services) == 0 {
		result.Errors = append(result.Errors, ValidationError{"services", "At least one service is required."})
		return doc, result
	}
	if len(doc.Services) > 25 {
		result.Errors = append(result.Errors, ValidationError{"services", "A project can contain at most 25 services."})
	}
	serviceKeys(&rootNode, &result)
	names := make([]string, 0, len(doc.Services))
	for name := range doc.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		raw := doc.Services[name]
		path := "services." + name
		if !servicePattern.MatchString(name) {
			result.Errors = append(result.Errors, ValidationError{path, "Service names must contain only letters, numbers, dot, dash, or underscore."})
		}
		if raw.Image == "" && raw.Build == nil {
			result.Errors = append(result.Errors, ValidationError{path, "Specify image or build."})
		}
		if raw.Image != "" {
			if err := ValidateImageReference(raw.Image); err != nil {
				result.Errors = append(result.Errors, ValidationError{path + ".image", err.Error()})
			}
		}
		build, err := parseBuild(raw.Build)
		if err != nil {
			result.Errors = append(result.Errors, ValidationError{path + ".build", err.Error()})
		}
		if build.Context != "" {
			clean := filepath.Clean(build.Context)
			if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
				result.Errors = append(result.Errors, ValidationError{path + ".build.context", "Build context must stay inside the project."})
			}
		}
		env, err := parseEnvironment(raw.Environment)
		if err != nil {
			result.Errors = append(result.Errors, ValidationError{path + ".environment", err.Error()})
		}
		// env_file values are merged underneath the inline environment, which
		// keeps Compose's precedence: an explicit `environment` entry wins.
		if refs, refErr := parseEnvFiles(raw.EnvFile); refErr != nil {
			result.Errors = append(result.Errors, ValidationError{path + ".env_file", refErr.Error()})
		} else if len(refs) > 0 {
			if root == "" {
				result.Warnings = append(result.Warnings, ValidationError{path + ".env_file", "Referenced files are read from the project source; validation previews cannot resolve them."})
			} else if fileEnv, loadErr := loadEnvFiles(refs, root); loadErr != nil {
				result.Errors = append(result.Errors, ValidationError{path + ".env_file", loadErr.Error()})
			} else {
				merged := map[string]string{}
				for key, value := range fileEnv {
					merged[key] = value
				}
				for key, value := range env {
					merged[key] = value
				}
				if envErr := ValidateEnvironment(merged); envErr != nil {
					result.Errors = append(result.Errors, ValidationError{path + ".env_file", envErr.Error()})
				} else {
					env = merged
				}
			}
		}
		command, err := parseCommand(raw.Command)
		if err != nil {
			result.Errors = append(result.Errors, ValidationError{path + ".command", err.Error()})
		}
		deps := parseDependsOn(raw.DependsOn)
		for _, dep := range deps {
			if _, ok := doc.Services[dep]; !ok {
				result.Errors = append(result.Errors, ValidationError{path + ".depends_on", fmt.Sprintf("Unknown service %q.", dep)})
			}
		}
		volumes := []string{}
		for i, spec := range raw.Volumes {
			resolved, err := resolveVolume(spec, projectSlug, root, doc.Volumes)
			if err != nil {
				result.Errors = append(result.Errors, ValidationError{fmt.Sprintf("%s.volumes.%d", path, i), err.Error()})
				continue
			}
			volumes = append(volumes, resolved)
		}
		cfg := doc.Asgard.Services[name]
		port := cfg.Port
		if port == 0 {
			port = firstPort(raw.Ports, raw.Expose)
		}
		public := false
		if cfg.Public != nil {
			public = *cfg.Public
		} else if port > 0 && name == doc.Asgard.PrimaryService {
			public = true
		}
		if public && port == 0 {
			result.Errors = append(result.Errors, ValidationError{path + ".x-asgard.port", "A public service needs an internal port."})
		}
		role := cfg.Role
		if role == "" {
			role = "web"
		}
		if role != "web" && role != "worker" && role != "stateful" {
			result.Errors = append(result.Errors, ValidationError{path + ".x-asgard.role", "Role must be web, worker, or stateful."})
		}
		hostname := cfg.Hostname
		if public && hostname == "" {
			if name == doc.Asgard.PrimaryService || doc.Asgard.PrimaryService == "" && len(doc.Services) == 1 {
				hostname = projectSlug + ".asgard.rousoftware.com"
			} else {
				hostname = Slug(name) + "--" + projectSlug + ".asgard.rousoftware.com"
			}
		}
		health := cfg.HealthPath
		if health == "" {
			health = "/"
		}
		if !strings.HasPrefix(health, "/") {
			result.Errors = append(result.Errors, ValidationError{path + ".x-asgard.health-path", "Health path must start with /."})
		}
		restart := raw.Restart
		if restart == "" {
			restart = "unless-stopped"
		}
		if restart != "no" && restart != "always" && restart != "on-failure" && restart != "unless-stopped" {
			result.Errors = append(result.Errors, ValidationError{path + ".restart", "Unsupported restart policy."})
		}
		result.Services = append(result.Services, store.Service{ID: uuid.NewString(), ProjectID: projectID, Name: name, Role: role, Image: raw.Image, BuildContext: build.Context, Dockerfile: build.Dockerfile, Command: command, Environment: env, Public: public, Port: port, Hostname: hostname, HealthPath: health, CPULimit: 0.5, MemoryLimit: 512 << 20, PIDsLimit: 256, RestartPolicy: restart, DependsOn: deps, Volumes: volumes})
	}
	primary := doc.Asgard.PrimaryService
	if primary == "" && len(names) == 1 {
		primary = names[0]
	}
	if primary != "" {
		if _, ok := doc.Services[primary]; !ok {
			result.Errors = append(result.Errors, ValidationError{"x-asgard.primary-service", "Primary service does not exist."})
		}
	}
	result.PrimaryService = primary
	result.Valid = len(result.Errors) == 0
	return doc, result
}

func resultErrorCollector(result *ValidationResult) func(ValidationError) {
	return func(err ValidationError) { result.Errors = append(result.Errors, err) }
}

func validateKeys(node *yaml.Node, add func(ValidationError), allowed []string, path string) {
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		node = node.Content[0]
	}
	if node.Kind != yaml.MappingNode {
		return
	}
	set := map[string]bool{}
	for _, key := range allowed {
		set[key] = true
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if !set[key] {
			add(ValidationError{path + "." + key, "This field is not supported by Asgard's safe Compose contract."})
		}
	}
}

func serviceKeys(node *yaml.Node, result *ValidationResult) {
	if node.Kind == yaml.DocumentNode {
		node = node.Content[0]
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value != "services" {
			continue
		}
		services := node.Content[i+1]
		for j := 0; j+1 < len(services.Content); j += 2 {
			name := services.Content[j].Value
			validateKeys(services.Content[j+1], resultErrorCollector(result), []string{"image", "build", "command", "environment", "env_file", "ports", "expose", "volumes", "depends_on", "healthcheck", "restart"}, "services."+name)
		}
	}
}

func parseBuild(value any) (Build, error) {
	if value == nil {
		return Build{}, nil
	}
	switch v := value.(type) {
	case string:
		return Build{Context: v, Dockerfile: "Dockerfile"}, nil
	case map[string]any:
		b := Build{Dockerfile: "Dockerfile", Args: map[string]string{}}
		for key, val := range v {
			switch key {
			case "context":
				b.Context = fmt.Sprint(val)
			case "dockerfile":
				b.Dockerfile = fmt.Sprint(val)
			case "target":
				b.Target = fmt.Sprint(val)
			case "args":
				switch args := val.(type) {
				case map[string]any:
					for k, item := range args {
						b.Args[k] = fmt.Sprint(item)
					}
				default:
					return b, errors.New("build.args must be a map")
				}
			default:
				return b, fmt.Errorf("build.%s is not supported", key)
			}
		}
		if b.Context == "" {
			b.Context = "."
		}
		return b, nil
	default:
		return Build{}, errors.New("build must be a path or mapping")
	}
}

func parseEnvironment(value any) (map[string]string, error) {
	out := map[string]string{}
	if value == nil {
		return out, nil
	}
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			if item == nil {
				return nil, fmt.Errorf("environment %q must have an explicit value", key)
			}
			out[key] = fmt.Sprint(item)
		}
	case []any:
		for _, item := range v {
			pair := strings.SplitN(fmt.Sprint(item), "=", 2)
			if len(pair) != 2 {
				return nil, fmt.Errorf("environment %q must include an explicit value", pair[0])
			}
			out[pair[0]] = pair[1]
		}
	default:
		return nil, errors.New("environment must be a map or list")
	}
	if err := ValidateEnvironment(out); err != nil {
		return nil, err
	}
	return out, nil
}
func parseCommand(value any) ([]string, error) {
	if value == nil {
		return []string{}, nil
	}
	switch v := value.(type) {
	case string:
		return []string{"/bin/sh", "-c", v}, nil
	case []any:
		out := make([]string, len(v))
		for i, item := range v {
			out[i] = fmt.Sprint(item)
		}
		return out, nil
	default:
		return nil, errors.New("command must be a string or list")
	}
}
func parseDependsOn(value any) []string {
	out := []string{}
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			out = append(out, fmt.Sprint(item))
		}
	case map[string]any:
		for key := range v {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// resolveVolume validates one Compose volume entry and returns its canonical
// stored form: an Asgard-scoped named volume, or a project-relative mount.
func resolveVolume(spec, slug, root string, declared map[string]any) (string, error) {
	parts := strings.Split(spec, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return "", errors.New("volume must be named-volume:/absolute/container/path[:ro] or ./project/path:/absolute/container/path[:ro]")
	}
	source, target := parts[0], parts[1]
	if source == "" {
		return "", errors.New("volume source is required")
	}
	if !filepath.IsAbs(target) {
		return "", errors.New("container mount path must be absolute")
	}
	if len(parts) == 3 && parts[2] != "ro" && parts[2] != "rw" {
		return "", errors.New("only ro or rw volume mode is supported")
	}
	if isBindSource(source) || strings.HasPrefix(source, "~") {
		normalized, err := normalizeProjectMount(source, root)
		if err != nil {
			return "", err
		}
		parts[0] = normalized
		return strings.Join(parts, ":"), nil
	}
	if _, ok := declared[source]; !ok {
		return "", fmt.Errorf("named volume %q is not declared", source)
	}
	parts[0] = "asgard-" + slug + "-" + Slug(source)
	return strings.Join(parts, ":"), nil
}

func firstPort(ports, expose []any) int {
	for _, value := range append(ports, expose...) {
		text := fmt.Sprint(value)
		text = strings.Split(text, "/")[0]
		parts := strings.Split(text, ":")
		candidate := parts[len(parts)-1]
		if strings.Contains(candidate, "-") {
			candidate = strings.Split(candidate, "-")[0]
		}
		port, _ := strconv.Atoi(candidate)
		if port > 0 && port < 65536 {
			return port
		}
	}
	return 0
}

func ValidateImageReference(ref string) error {
	ref = strings.TrimSpace(ref)
	if ref == "" || len(ref) > 512 {
		return errors.New("invalid image reference")
	}
	if strings.ContainsAny(ref, " \t\r\n@?&") {
		return errors.New("image reference contains unsupported characters")
	}
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		u, err := url.Parse(ref)
		if err != nil || !strings.EqualFold(u.Host, "hub.docker.com") {
			return errors.New("only Docker Hub URLs or OCI image references are supported")
		}
	}
	return nil
}

func IsPublicHTTPS(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return errors.New("source URL must be a public HTTPS URL without credentials")
	}
	if u.Port() != "" && u.Port() != "443" {
		return errors.New("source URL must use the standard HTTPS port")
	}
	return checkPublicHost(u.Hostname())
}

// GitSource is a parsed, validated repository location. Credentials never live
// in the URL; they are resolved separately from the credential store so the
// secret stays out of process arguments, stored config, and audit records.
type GitSource struct {
	// Scheme is "https" or "ssh".
	Scheme string
	// URL is the exact location handed to git.
	URL string
	// Host is the resolved remote host.
	Host string
}

var scpLikePattern = regexp.MustCompile(`^([A-Za-z0-9._-]+)@([A-Za-z0-9.-]+):(.+)$`)

// ValidateGitSource accepts a public HTTPS repository, and additionally an SSH
// repository when an SSH credential will be supplied. Hosts resolving to
// private, loopback, or link-local addresses are refused in every case so an
// import cannot be aimed at the host's own network.
func ValidateGitSource(raw string, sshCredential bool) (GitSource, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 2048 {
		return GitSource{}, errors.New("repository URL is required")
	}
	if match := scpLikePattern.FindStringSubmatch(raw); match != nil && !strings.Contains(raw, "://") {
		if !sshCredential {
			return GitSource{}, errors.New("scp-style SSH URLs require an SSH credential; select one or use an HTTPS URL")
		}
		if err := checkPublicHost(match[2]); err != nil {
			return GitSource{}, err
		}
		return GitSource{Scheme: "ssh", URL: raw, Host: match[2]}, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return GitSource{}, errors.New("repository URL could not be parsed")
	}
	if u.User != nil {
		return GitSource{}, errors.New("do not embed credentials in the URL; store them as an Asgard Git credential instead")
	}
	switch u.Scheme {
	case "https":
		if u.Port() != "" && u.Port() != "443" {
			return GitSource{}, errors.New("HTTPS repositories must use the standard HTTPS port")
		}
	case "ssh":
		if !sshCredential {
			return GitSource{}, errors.New("ssh:// repositories require an SSH credential")
		}
	default:
		return GitSource{}, errors.New("repository URL must use https:// or ssh://")
	}
	if err := checkPublicHost(u.Hostname()); err != nil {
		return GitSource{}, err
	}
	return GitSource{Scheme: u.Scheme, URL: u.String(), Host: u.Hostname()}, nil
}

var hostnamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

// ValidateHostname reports whether value is a bare DNS hostname.
func ValidateHostname(value string) bool {
	return len(value) <= 253 && hostnamePattern.MatchString(value)
}

func checkPublicHost(host string) error {
	addresses, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("resolve source host: %w", err)
	}
	for _, ip := range addresses {
		if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() {
			return errors.New("source host resolves to a non-public address")
		}
	}
	return nil
}
