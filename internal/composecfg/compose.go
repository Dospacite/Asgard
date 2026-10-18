package composecfg

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mattn/go-shellwords"
	"github.com/rousoftware/asgard/internal/store"
	"gopkg.in/yaml.v3"
)

// Modes for the files and directories of a project's own imported source tree.
//
// The tree is only ever visible where the project's Compose file mounts it: a
// workload sees the paths it declares and nothing else, and one project can
// never reach another's directory. It does have to be readable, though, because
// a service may run as a non-root user — `user: "1000:1000"` with
// `./nginx.conf:/etc/nginx/nginx.conf:ro` is an ordinary hardened Compose file —
// and a 0600 file owned by root is unreadable to exactly that workload. Root
// containers see the same bytes either way, so the narrower mode bought no
// isolation and cost a class of imports that failed at runtime with a permission
// error rather than at validation with an explanation.
const (
	SourceDirMode  os.FileMode = 0o755
	SourceFileMode os.FileMode = 0o644
	// SourceExecMode keeps an executable's bit while staying group- and
	// world-readable, the way a git checkout does.
	SourceExecMode os.FileMode = 0o755
)

// MaxComposeBytes bounds the Compose file Asgard will read.
const MaxComposeBytes = 1 << 20

// Source locates the Compose file being parsed inside its project.
//
// The distinction matters because Compose resolves every relative path — the
// build context, `env_file` entries, `./config/app.json` mounts, and the
// project's `.env` — against the directory holding the Compose file, not
// against the repository root. A project with `deploy/compose.yaml` therefore
// means `deploy/nginx.conf` when it mounts `./nginx.conf`, and reading it from
// the root silently mounted the wrong file or failed validation outright.
type Source struct {
	// Root is the project's imported source tree. Empty means no tree is
	// available: contract previews validate the document's structure and skip
	// the filesystem checks.
	Root string
	// Path is the Compose file's path relative to Root, such as "compose.yaml"
	// or "deploy/compose.yaml". Empty means "compose.yaml".
	Path string
}

// Directory is the Compose file's directory relative to Root, in slash form.
// Empty means the root itself.
func (s Source) Directory() string {
	if s.Path == "" {
		return ""
	}
	dir := path.Dir(filepath.ToSlash(filepath.Clean(s.Path)))
	if dir == "." || dir == "/" || dir == "" || strings.HasPrefix(dir, "..") {
		return ""
	}
	return dir
}

// AbsoluteDir is the Compose file's directory on disk, or "" when no tree is
// available.
func (s Source) AbsoluteDir() string {
	if s.Root == "" {
		return ""
	}
	return filepath.Join(s.Root, filepath.FromSlash(s.Directory()))
}

type Document struct {
	Name     string                     `yaml:"name"`
	Services map[string]RawService      `yaml:"services"`
	Volumes  map[string]DeclaredVolume  `yaml:"volumes"`
	Networks map[string]DeclaredNetwork `yaml:"networks"`
	Secrets  map[string]DeclaredFile    `yaml:"secrets"`
	Configs  map[string]DeclaredFile    `yaml:"configs"`
	Asgard   AsgardConfig               `yaml:"x-asgard"`
}

// DeclaredFile is one entry of the top-level `secrets` or `configs` block.
//
// Compose has two sources for these: a file, and something that already exists
// on the host (an external store entry, or a value in the environment that ran
// the container). Only the first is something Asgard can serve — a file inside
// the project's own source tree — because the others would mean reading the
// host or the control plane's environment into a workload.
type DeclaredFile struct {
	File     string `yaml:"file"`
	External any    `yaml:"external"`
	Name     string `yaml:"name"`
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

// DeclaredVolume is one entry of the top-level `volumes` block. Asgard keeps
// the project's own volume for every logical name, so only the declaration is
// read: `external` and `name` say that the file expected to mount something
// that already exists on the host, which is reported.
type DeclaredVolume struct {
	External any    `yaml:"external"`
	Name     string `yaml:"name"`
	Driver   string `yaml:"driver"`
}

// External marks a volume the file expects to already exist.
func (v DeclaredVolume) IsExternal() bool { return truthy(v.External) }

// ExternalName is the host-side name the file expects, when it names one.
func (v DeclaredVolume) ExternalName() string {
	if v.Name != "" {
		return v.Name
	}
	if mapping, ok := v.External.(map[string]any); ok {
		return fmt.Sprint(mapping["name"])
	}
	return ""
}

// DeclaredNetwork is one entry of the top-level `networks` block.
type DeclaredNetwork struct {
	External any    `yaml:"external"`
	Name     string `yaml:"name"`
	Internal bool   `yaml:"internal"`
	Driver   string `yaml:"driver"`
}

// External marks a network the file expects to already exist.
func (n DeclaredNetwork) IsExternal() bool { return truthy(n.External) }

type RawService struct {
	Image           string `yaml:"image"`
	Build           any    `yaml:"build"`
	Command         any    `yaml:"command"`
	Entrypoint      any    `yaml:"entrypoint"`
	Environment     any    `yaml:"environment"`
	EnvFile         any    `yaml:"env_file"`
	Ports           []any  `yaml:"ports"`
	Expose          []any  `yaml:"expose"`
	Volumes         []any  `yaml:"volumes"`
	DependsOn       any    `yaml:"depends_on"`
	Healthcheck     any    `yaml:"healthcheck"`
	Restart         string `yaml:"restart"`
	WorkingDir      string `yaml:"working_dir"`
	User            string `yaml:"user"`
	Labels          any    `yaml:"labels"`
	Hostname        string `yaml:"hostname"`
	Platform        string `yaml:"platform"`
	ReadOnly        any    `yaml:"read_only"`
	ShmSize         any    `yaml:"shm_size"`
	StopGracePeriod any    `yaml:"stop_grace_period"`
	StopSignal      string `yaml:"stop_signal"`
	Tmpfs           any    `yaml:"tmpfs"`
	Sysctls         any    `yaml:"sysctls"`
	Ulimits         any    `yaml:"ulimits"`
	MemLimit        any    `yaml:"mem_limit"`
	CPUs            any    `yaml:"cpus"`
	PIDsLimit       any    `yaml:"pids_limit"`
	Deploy          any    `yaml:"deploy"`
	Networks        any    `yaml:"networks"`
	Secrets         any    `yaml:"secrets"`
	Configs         any    `yaml:"configs"`
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
var platformPattern = regexp.MustCompile(`^[a-z0-9]+/[a-z0-9._-]+(/[a-z0-9._-]+)?$`)
var environmentKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

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

// parseContext carries everything one Parse call needs: the project's identity,
// where its source lives, and the accumulated result. Passing it down keeps the
// per-field parsers free of parameter lists that grow with every Compose
// feature.
type parseContext struct {
	projectID  string
	slug       string
	source     Source
	composeDir string
	document   Document
	result     *ValidationResult
}

func (c *parseContext) error(path, message string) {
	c.result.Errors = append(c.result.Errors, ValidationError{path, message})
}

func (c *parseContext) warn(path, message string) {
	c.result.Warnings = append(c.result.Warnings, ValidationError{path, message})
}

func Parse(data []byte, projectID, projectSlug string, source Source) (Document, ValidationResult) {
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
	context := &parseContext{projectID: projectID, slug: projectSlug, source: source, composeDir: source.Directory(), result: &result}
	missing := map[string]bool{}
	if err := interpolateNode(&rootNode, interpolationValues(source), missing); err != nil {
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
	context.document = doc

	// The keys are classified from the decoded document rather than from the
	// YAML tree, so a mapping merged in with `<<: *anchor` is classified
	// exactly like one written inline.
	var raw map[string]any
	if err := rootNode.Decode(&raw); err != nil {
		result.Errors = append(result.Errors, ValidationError{"compose", err.Error()})
		return doc, result
	}
	classify(raw, topLevelFields, "compose", &result)
	classifyNested(raw, "x-asgard", xAsgardFields, "compose", &result)
	context.checkAsgardBlock(raw)

	if len(doc.Services) == 0 {
		result.Errors = append(result.Errors, ValidationError{"services", "At least one service is required."})
		return doc, result
	}
	if len(doc.Services) > 25 {
		result.Errors = append(result.Errors, ValidationError{"services", "A project can contain at most 25 services."})
	}
	context.checkDeclarations(raw)

	services := raw["services"]
	serviceMaps, _ := services.(map[string]any)
	names := make([]string, 0, len(doc.Services))
	for name := range doc.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		serviceMap, _ := serviceMaps[name].(map[string]any)
		service, ok := context.service(name, doc.Services[name], serviceMap)
		if !ok {
			continue
		}
		result.Services = append(result.Services, service)
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

// checkAsgardBlock validates the routing block: every key it may carry, and
// every service it names. The block is the one part of an imported file an
// operator writes by hand, so a misspelled `healthpath` used to be accepted in
// silence — and the service kept the default health path with nothing to say
// why it was being probed at /.
func (c *parseContext) checkAsgardBlock(raw map[string]any) {
	block, ok := raw["x-asgard"].(map[string]any)
	if !ok {
		return
	}
	services, _ := block["services"].(map[string]any)
	for _, name := range sortedKeys(services) {
		if _, declared := c.document.Services[name]; !declared {
			c.error("compose.x-asgard.services."+name, "x-asgard configures a service this file does not declare.")
		}
		entry, _ := services[name].(map[string]any)
		classify(entry, xAsgardServiceFields, "compose.x-asgard.services."+name, c.result)
	}
}

// checkDeclarations reads the top-level `volumes` and `networks` blocks. A
// declaration Asgard cannot honor — an external volume or network, or one with
// a fixed host-side name — is reported once here, in the place the file says
// it, rather than once per service that references it.
func (c *parseContext) checkDeclarations(raw map[string]any) {
	if volumes, ok := raw["volumes"].(map[string]any); ok {
		for _, name := range sortedKeys(volumes) {
			entry, _ := volumes[name].(map[string]any)
			classify(entry, volumeDeclFields, "volumes."+name, c.result)
			declared := c.document.Volumes[name]
			if declared.IsExternal() {
				c.warn("volumes."+name, fmt.Sprintf("This volume is declared external, so the data it names already exists on the host. Asgard gives the project its own volume (asgard-%s-%s) instead; it starts empty. Move the data into Asgard, or keep using the host-side volume outside it.", c.slug, Slug(name)))
			} else if external := declared.ExternalName(); external != "" {
				c.warn("volumes."+name, fmt.Sprintf("Asgard names this project's volume asgard-%s-%s and does not use the host-side name %q: a project's volumes stay scoped to the project so two projects cannot share one by accident.", c.slug, Slug(name), external))
			}
		}
	}
	if networks, ok := raw["networks"].(map[string]any); ok {
		for _, name := range sortedKeys(networks) {
			entry, _ := networks[name].(map[string]any)
			classify(entry, networkDeclFields, "networks."+name, c.result)
		}
	}
	c.checkFiles(raw, "secrets", secretDeclFields, c.document.Secrets)
	c.checkFiles(raw, "configs", configDeclFields, c.document.Configs)
}

// checkFiles validates the top-level `secrets` and `configs` blocks: which
// source each one names, and whether the file it names is actually there.
func (c *parseContext) checkFiles(raw map[string]any, kind string, table *fieldTable, declared map[string]DeclaredFile) {
	block, ok := raw[kind].(map[string]any)
	if !ok {
		return
	}
	for _, name := range sortedKeys(block) {
		entry, _ := block[name].(map[string]any)
		classify(entry, table, kind+"."+name, c.result)
		source := declared[name]
		if source.File == "" {
			continue
		}
		if _, err := c.resolveProjectFile(source.File); err != nil {
			c.error(kind+"."+name+".file", err.Error())
		}
	}
}

// serviceNetworks reads a service's `networks` entries: which networks it joins,
// and the extra names it answers to on them.
//
// Every service in a project already shares the project's own bridge network,
// which is where Asgard's service-name discovery happens, so memberships in the
// project's own networks are satisfied by construction. A membership in
// something that is not the project's — an external network, or one with a
// fixed host-side name — cannot be attached, and is reported.
func (c *parseContext) serviceNetworks(raw any, serviceName, path string) []string {
	if raw == nil {
		return nil
	}
	type membership struct {
		name    string
		options map[string]any
	}
	memberships := []membership{}
	switch value := raw.(type) {
	case []any:
		for _, item := range value {
			memberships = append(memberships, membership{name: fmt.Sprint(item)})
		}
	case map[string]any:
		for _, name := range sortedKeys(value) {
			options, _ := value[name].(map[string]any)
			memberships = append(memberships, membership{name: name, options: options})
		}
	default:
		c.error(path+".networks", "networks must be a list of names or a mapping of names to options")
		return nil
	}
	aliases := []string{}
	for _, item := range memberships {
		if item.name == "default" {
			continue
		}
		declared, exists := c.document.Networks[item.name]
		if !exists {
			c.error(path+".networks", fmt.Sprintf("network %q is not declared in the top-level networks block", item.name))
			continue
		}
		classify(item.options, networkMembershipFields, path+".networks."+item.name, c.result)
		switch {
		case declared.IsExternal():
			c.warn(path+".networks."+item.name, fmt.Sprintf("Membership in the external network %q is not attached: a project only joins its own network and the Asgard shared networks an operator grants it. Use an Asgard shared network for cross-project connectivity.", item.name))
		case declared.Name != "" || declared.Driver != "":
			c.warn(path+".networks."+item.name, fmt.Sprintf("The network %q is declared with its own host-side settings, which Asgard does not create. The service joins the project's network instead.", item.name))
		}
		for _, alias := range stringList(item.options["aliases"]) {
			if alias == serviceName || !ValidateHostname(strings.ToLower(alias)) {
				continue
			}
			aliases = append(aliases, strings.ToLower(alias))
		}
	}
	return dedupe(aliases)
}

// service builds one stored service from its Compose entry, recording every
// problem it finds on the way. It returns ok=false only when the entry is so
// malformed that no service can be described.
func (c *parseContext) service(name string, raw RawService, entry map[string]any) (store.Service, bool) {
	path := "services." + name
	accepted := c.safetyOptionsAlreadyInEffect(entry, path)
	classify(entry, serviceFields, path, c.result, accepted...)
	c.classifySections(entry, path)

	service := store.Service{ID: uuid.NewString(), ProjectID: c.projectID, Name: name, Role: "web", HealthPath: "/", RestartPolicy: "unless-stopped", Environment: map[string]string{}, Command: []string{}, DependsOn: []string{}, Volumes: []string{}}
	if !servicePattern.MatchString(name) {
		c.error(path, "Service names must contain only letters, numbers, dot, dash, or underscore.")
	}
	if raw.Image == "" && raw.Build == nil {
		c.error(path, "Specify image or build.")
	}
	if raw.Image != "" {
		if err := ValidateImageReference(raw.Image); err != nil {
			c.error(path+".image", err.Error())
		}
	}
	build, err := parseBuild(raw.Build)
	if err != nil {
		c.error(path+".build", err.Error())
	}
	if build.Context != "" || build.Dockerfile != "" {
		context, contextErr := c.projectPath(build.Context)
		if contextErr != nil {
			c.error(path+".build.context", contextErr.Error())
		} else {
			// Composes that build from a subdirectory are ordinary; a build
			// context reaching outside the imported tree is not, because the
			// import would have to run the host's own Dockerfile.
			build.Context = context
		}
	}
	if build.Context == "" && raw.Build != nil {
		build.Context = "."
	}
	service.BuildContext, service.Dockerfile = build.Context, build.Dockerfile
	service.Image = raw.Image

	environment, err := parseEnvironment(raw.Environment, path+".environment", c.result)
	if err != nil {
		c.error(path+".environment", err.Error())
	}
	c.applyEnvFiles(&service, raw.EnvFile, environment, path)
	command, err := parseCommand(raw.Command, path+".command")
	if err != nil {
		c.error(path+".command", err.Error())
	} else {
		service.Command = command
	}

	container, err := c.containerConfig(raw, entry, path)
	if err != nil {
		c.error(path, err.Error())
	}
	container.BuildArgs, container.BuildTarget = build.Args, build.Target
	if len(container.BuildArgs) == 0 {
		container.BuildArgs = nil
	}
	container.NetworkAliases = c.serviceNetworks(raw.Networks, name, path)
	service.Container = container

	deps, warnings := parseDependsOn(raw.DependsOn, path)
	for _, warning := range warnings {
		c.result.Warnings = append(c.result.Warnings, warning)
	}
	for _, dep := range deps {
		if _, ok := c.document.Services[dep]; !ok {
			c.error(path+".depends_on", fmt.Sprintf("Unknown service %q.", dep))
		}
	}
	service.DependsOn = deps

	for index, spec := range raw.Volumes {
		resolved, err := c.resolveVolume(spec, name)
		if err != nil {
			c.error(fmt.Sprintf("%s.volumes.%d", path, index), err.Error())
			continue
		}
		service.Volumes = append(service.Volumes, resolved)
	}
	service.Volumes = append(service.Volumes, c.serviceGrants(raw.Secrets, "secrets", c.document.Secrets, name, path)...)
	service.Volumes = append(service.Volumes, c.serviceGrants(raw.Configs, "configs", c.document.Configs, name, path)...)

	limits, ok := c.limits(raw, entry, path)
	if !ok {
		return service, false
	}
	service.CPULimit, service.MemoryLimit, service.PIDsLimit = limits.CPU, limits.Memory, limits.PIDs

	restart, err := normalizeRestart(raw.Restart)
	if err != nil {
		c.error(path+".restart", err.Error())
	}
	service.RestartPolicy = restart

	c.applyRouting(&service, name, raw, path)
	return service, true
}

// limits reads the resource ceilings a file declares, from either the
// orchestrator-facing `deploy.resources.limits` or the older flat keys, and
// falls back to the platform's deliberately small defaults.
//
// The two spellings cannot be combined — Compose refuses that too — because a
// service that says `cpus: 4` and `deploy.resources.limits.cpus: "1"` has no
// single intended ceiling.
func (c *parseContext) limits(raw RawService, entry map[string]any, path string) (resourceLimits, bool) {
	limits := resourceLimits{CPU: 0.5, Memory: 512 << 20, PIDs: 256}
	deployLimits := map[string]any{}
	if deploy, ok := entry["deploy"].(map[string]any); ok {
		if resources, ok := deploy["resources"].(map[string]any); ok {
			deployLimits, _ = resources["limits"].(map[string]any)
		}
	}
	flat := raw.MemLimit != nil || raw.CPUs != nil || raw.PIDsLimit != nil
	if flat && len(deployLimits) > 0 {
		c.error(path, "Set resource limits in one place: either mem_limit/cpus/pids_limit or deploy.resources.limits, not both.")
		return limits, false
	}
	if raw.MemLimit != nil {
		memory, err := parseByteSize(raw.MemLimit)
		if err != nil {
			c.error(path+".mem_limit", err.Error())
			return limits, false
		}
		limits.Memory = memory
	}
	if raw.CPUs != nil {
		cpu, err := parseCPUs(raw.CPUs)
		if err != nil {
			c.error(path+".cpus", err.Error())
			return limits, false
		}
		limits.CPU = cpu
	}
	if raw.PIDsLimit != nil {
		pids, err := parseCount(raw.PIDsLimit)
		if err != nil {
			c.error(path+".pids_limit", err.Error())
			return limits, false
		}
		limits.PIDs = pids
	}
	if value, ok := deployLimits["memory"]; ok {
		memory, err := parseByteSize(value)
		if err != nil {
			c.error(path+".deploy.resources.limits.memory", err.Error())
			return limits, false
		}
		limits.Memory = memory
	}
	if value, ok := deployLimits["cpus"]; ok {
		cpu, err := parseCPUs(value)
		if err != nil {
			c.error(path+".deploy.resources.limits.cpus", err.Error())
			return limits, false
		}
		limits.CPU = cpu
	}
	if value, ok := deployLimits["pids"]; ok {
		pids, err := parseCount(value)
		if err != nil {
			c.error(path+".deploy.resources.limits.pids", err.Error())
			return limits, false
		}
		limits.PIDs = pids
	}
	if err := ValidateComposeLimits(limits.CPU, limits.Memory, limits.PIDs); err != nil {
		c.error(path, err.Error())
		return limits, false
	}
	return limits, true
}

type resourceLimits struct {
	CPU    float64
	Memory int64
	PIDs   int64
}

// containerConfig reads the container-level settings: the ones Compose hands to
// the Docker API rather than to its own orchestrator.
func (c *parseContext) containerConfig(raw RawService, entry map[string]any, path string) (store.ContainerConfig, error) {
	config := store.ContainerConfig{}
	if raw.Entrypoint != nil {
		entrypoint, err := parseCommand(raw.Entrypoint, path+".entrypoint")
		if err != nil {
			return config, err
		}
		config.Entrypoint = &entrypoint
	}
	config.WorkingDir = strings.TrimSpace(raw.WorkingDir)
	if strings.Contains(config.WorkingDir, "\x00") {
		return config, errors.New("working_dir contains a null byte")
	}
	config.User = strings.TrimSpace(raw.User)
	if strings.ContainsAny(config.User, " \t\r\n\x00") {
		return config, errors.New("user must be a name, a uid, or uid:gid")
	}
	config.ContainerHostname = strings.TrimSpace(raw.Hostname)
	if config.ContainerHostname != "" && !ValidateHostname(strings.ToLower(config.ContainerHostname)) {
		return config, errors.New("hostname must be a DNS label")
	}
	labels, err := parseStringMap(raw.Labels, "labels")
	if err != nil {
		return config, err
	}
	if len(labels) > 0 {
		config.Labels = labels
	}
	if truthy(raw.ReadOnly) {
		config.ReadOnly = true
	}
	if raw.ShmSize != nil {
		size, err := parseByteSize(raw.ShmSize)
		if err != nil {
			return config, fmt.Errorf("shm_size: %w", err)
		}
		config.ShmSize = size
	}
	if raw.StopGracePeriod != nil {
		duration, err := parseDuration(raw.StopGracePeriod)
		if err != nil {
			return config, fmt.Errorf("stop_grace_period: %w", err)
		}
		seconds := int((duration + 999_999_999) / 1_000_000_000)
		if seconds > 0 {
			config.StopTimeoutSeconds = seconds
		}
	}
	if signal := strings.TrimSpace(raw.StopSignal); signal != "" {
		if len(signal) > 32 || strings.ContainsAny(signal, " \t\r\n\x00") {
			return config, errors.New("stop_signal must be a signal name or number, such as SIGTERM or 15")
		}
		config.StopSignal = signal
	}
	if raw.Tmpfs != nil {
		targets, err := parseTmpfs(raw.Tmpfs)
		if err != nil {
			return config, err
		}
		config.Tmpfs = targets
	}
	if raw.Sysctls != nil {
		sysctls, err := parseStringMap(raw.Sysctls, "sysctls")
		if err != nil {
			return config, err
		}
		if len(sysctls) > 0 {
			config.Sysctls = sysctls
		}
	}
	if raw.Ulimits != nil {
		ulimits, err := parseUlimits(raw.Ulimits)
		if err != nil {
			return config, err
		}
		config.Ulimits = ulimits
	}
	if platform := strings.TrimSpace(raw.Platform); platform != "" {
		if !platformPattern.MatchString(platform) {
			return config, errors.New("platform must be os/arch, such as linux/arm64")
		}
		config.Platform = platform
	}
	healthcheck, err := c.healthcheck(raw.Healthcheck, path)
	if err != nil {
		return config, err
	}
	config.Healthcheck = healthcheck
	return config, nil
}

// healthcheck normalizes a Compose healthcheck into the form the container
// runtime takes. Applying it here is what makes the platform's readiness gate
// and the file's own definition of healthy the same definition: a service that
// says how to prove itself healthy is not asked to answer an HTTP request at
// some path Asgard made up.
func (c *parseContext) healthcheck(value any, path string) (*store.Healthcheck, error) {
	if value == nil {
		return nil, nil
	}
	entry, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("healthcheck must be a mapping")
	}
	health := store.Healthcheck{}
	if truthy(entry["disable"]) {
		health.Test = []string{"NONE"}
	}
	if test, exists := entry["test"]; exists {
		normalized, err := healthTest(test)
		if err != nil {
			return nil, err
		}
		health.Test = normalized
	}
	for field, target := range map[string]*time.Duration{
		"interval":       &health.Interval,
		"timeout":        &health.Timeout,
		"start_period":   &health.StartPeriod,
		"start_interval": &health.StartInterval,
	} {
		if raw, exists := entry[field]; exists && raw != nil {
			duration, err := parseDuration(raw)
			if err != nil {
				return nil, fmt.Errorf("healthcheck.%s: %w", field, err)
			}
			*target = duration
		}
	}
	if raw, exists := entry["retries"]; exists && raw != nil {
		retries, err := parseCount(raw)
		if err != nil || retries < 0 || retries > 1000 {
			return nil, errors.New("healthcheck.retries must be a number between 0 and 1000")
		}
		health.Retries = int(retries)
	}
	if len(health.Test) == 0 && health.Interval == 0 && health.Timeout == 0 && health.StartPeriod == 0 && health.Retries == 0 {
		return nil, nil
	}
	if len(health.Test) == 0 {
		return nil, errors.New("healthcheck needs a test; without one there is nothing to check")
	}
	return &health, nil
}

// healthTest accepts the three spellings Compose allows: a shell string, an
// exec list, and the CMD/CMD-SHELL/NONE prefixes Docker expects.
func healthTest(value any) ([]string, error) {
	switch test := value.(type) {
	case string:
		if strings.TrimSpace(test) == "" {
			return nil, errors.New("healthcheck.test is empty")
		}
		return []string{"CMD-SHELL", test}, nil
	case []any:
		parts := make([]string, 0, len(test))
		for _, item := range test {
			parts = append(parts, fmt.Sprint(item))
		}
		if len(parts) == 0 {
			return nil, errors.New("healthcheck.test is empty")
		}
		switch parts[0] {
		case "NONE":
			return []string{"NONE"}, nil
		case "CMD", "CMD-SHELL":
			if len(parts) < 2 {
				return nil, errors.New("healthcheck.test needs a command after " + parts[0])
			}
			return parts, nil
		default:
			return nil, errors.New("healthcheck.test must start with CMD, CMD-SHELL, or NONE")
		}
	default:
		return nil, errors.New("healthcheck.test must be a string or a list")
	}
}

// safetyOptionsAlreadyInEffect recognizes the hardening options that ask for
// exactly what Asgard already does. A security_opt that only repeats
// no-new-privileges, or a cap_drop of ALL, is not a request for more
// privilege — it is a file documenting the same choice the platform made, and
// refusing it refused ordinary hardened Compose files. The same applies to
// `network_mode: bridge`, which names Docker's default network and is satisfied
// by the project's own network.
func (c *parseContext) safetyOptionsAlreadyInEffect(entry map[string]any, path string) []string {
	accepted := []string{}
	if options, ok := entry["security_opt"].([]any); ok {
		allowed := true
		for _, item := range options {
			value := strings.TrimSpace(fmt.Sprint(item))
			if value != "no-new-privileges" && value != "no-new-privileges:true" {
				allowed = false
			}
		}
		if allowed && len(options) > 0 {
			accepted = append(accepted, "security_opt")
		}
	}
	if drops, ok := entry["cap_drop"].([]any); ok {
		allowed := true
		for _, item := range drops {
			if !strings.EqualFold(strings.TrimSpace(fmt.Sprint(item)), "ALL") {
				allowed = false
			}
		}
		if allowed && len(drops) > 0 {
			accepted = append(accepted, "cap_drop")
		}
	}
	if mode, ok := entry["network_mode"].(string); ok {
		switch strings.ToLower(strings.TrimSpace(fmt.Sprint(mode))) {
		case "bridge", "default":
			accepted = append(accepted, "network_mode")
			c.warn(path+".network_mode", "The default bridge network is not used: the service joins the project's own network, where it reaches the other services by name. Host, container, and service network modes are still refused.")
		}
	}
	return accepted
}

// classifySections classifies the nested mappings of one service.
func (c *parseContext) classifySections(entry map[string]any, path string) {
	classifyNested(entry, "build", buildFields, path, c.result)
	classifyNested(entry, "healthcheck", healthcheckFields, path, c.result)
	classifyNested(entry, "deploy", deployFields, path, c.result)
	deploy, _ := entry["deploy"].(map[string]any)
	if deploy == nil {
		return
	}
	classifyNested(deploy, "resources", resourcesFields, path+".deploy", c.result)
	resources, _ := deploy["resources"].(map[string]any)
	if resources == nil {
		return
	}
	classifyNested(resources, "limits", limitsFields, path+".deploy.resources", c.result)
}

// applyEnvFiles merges the service's env_file values underneath its inline
// environment, which keeps Compose's precedence: an explicit `environment`
// entry wins.
func (c *parseContext) applyEnvFiles(service *store.Service, rawEnvFile any, environment map[string]string, path string) {
	service.Environment = environment
	refs, err := parseEnvFiles(rawEnvFile)
	if err != nil {
		c.error(path+".env_file", err.Error())
		return
	}
	if len(refs) == 0 {
		return
	}
	if c.source.Root == "" {
		c.warn(path+".env_file", "Referenced files are read from the project source; validation previews cannot resolve them.")
		return
	}
	fileEnv, err := loadEnvFiles(refs, c.source)
	if err != nil {
		c.error(path+".env_file", err.Error())
		return
	}
	merged := map[string]string{}
	for key, value := range fileEnv {
		merged[key] = value
	}
	for key, value := range environment {
		merged[key] = value
	}
	if err := ValidateEnvironment(merged); err != nil {
		c.error(path+".env_file", err.Error())
		return
	}
	service.Environment = merged
}

// applyRouting fills in the service's role, route, and health path from the
// x-asgard block and the Compose file's own port declarations.
func (c *parseContext) applyRouting(service *store.Service, name string, raw RawService, path string) {
	cfg := c.document.Asgard.Services[name]
	port := cfg.Port
	if port == 0 {
		port = firstPort(raw.Ports, raw.Expose)
	}
	public := false
	if cfg.Public != nil {
		public = *cfg.Public
	} else if port > 0 && name == c.document.Asgard.PrimaryService {
		public = true
	}
	if public && port == 0 {
		c.error(path+".x-asgard.port", "A public service needs an internal port.")
	}
	if cfg.Role != "" {
		service.Role = cfg.Role
	}
	if service.Role != "web" && service.Role != "worker" && service.Role != "stateful" {
		c.error(path+".x-asgard.role", "Role must be web, worker, or stateful.")
	}
	hostname := cfg.Hostname
	if public && hostname == "" {
		if name == c.document.Asgard.PrimaryService || c.document.Asgard.PrimaryService == "" && len(c.document.Services) == 1 {
			hostname = c.slug + "." + PlaceholderDomain
		} else {
			hostname = Slug(name) + "--" + c.slug + "." + PlaceholderDomain
		}
	}
	health := cfg.HealthPath
	if health == "" {
		health = "/"
	}
	if !strings.HasPrefix(health, "/") {
		c.error(path+".x-asgard.health-path", "Health path must start with /.")
	}
	service.Public, service.Port, service.Hostname, service.HealthPath = public, port, hostname, health
}

// resolveVolume validates one Compose volume entry and returns its canonical
// stored form: an Asgard-scoped named volume, a project-relative mount, or a
// stable volume Asgard gives an anonymous mount.
func (c *parseContext) resolveVolume(value any, serviceName string) (string, error) {
	request, err := parseVolume(value, c.result, "services."+serviceName+".volumes")
	if err != nil {
		return "", err
	}
	mode := ""
	if request.ReadOnly {
		mode = ":ro"
	}
	switch {
	case request.Project:
		relative, err := normalizeProjectMount(request.Source, c.source.Root, c.composeDir)
		if err != nil {
			return "", err
		}
		return relative + ":" + request.Target + mode, nil
	case request.Source == "":
		// An anonymous volume. Compose gives the container a fresh volume at
		// this path on every recreate; Asgard gives it a stable one named after
		// the service and the mount path, so data a release writes survives the
		// next release instead of being silently replaced.
		return "asgard-" + c.slug + "-" + Slug(serviceName) + "-" + shortHash(request.Target) + ":" + request.Target + mode, nil
	default:
		declared, ok := c.document.Volumes[request.Source]
		if !ok {
			return "", fmt.Errorf("named volume %q is not declared", request.Source)
		}
		if declared.IsExternal() || declared.ExternalName() != "" {
			// Reported once against the declaration itself.
			_ = declared
		}
		return "asgard-" + c.slug + "-" + Slug(request.Source) + ":" + request.Target + mode, nil
	}
}

// resolveProjectFile resolves one file the project itself ships and returns its
// canonical @project/ mount source. Compose reads a `file:` in the secrets or
// configs block relative to the Compose file, and only a path that stays inside
// the imported tree can be served.
func (c *parseContext) resolveProjectFile(file string) (string, error) {
	value := strings.TrimSpace(file)
	if value == "" {
		return "", errors.New("file is required")
	}
	clean := path.Join(c.composeDir, filepath.ToSlash(value))
	if strings.HasPrefix(value, "/") || strings.HasPrefix(value, "~") || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%s must be a path inside the project", value)
	}
	if c.source.Root != "" {
		if err := requireInsideRoot(c.source.Root, clean); err != nil {
			return "", fmt.Errorf("%s must be a path inside the project", value)
		}
		if info, err := os.Lstat(filepath.Join(c.source.Root, filepath.FromSlash(clean))); err != nil {
			return "", fmt.Errorf("%s is not present in the project source", clean)
		} else if info.IsDir() {
			return "", fmt.Errorf("%s is a directory, not a file", clean)
		}
	}
	return ProjectMountPrefix + clean, nil
}

// serviceGrants turns a service's `secrets` or `configs` entries into the
// read-only mounts that serve them: a file the project ships, mounted where
// Compose mounts it. `secrets` default to /run/secrets/<name> and `configs` to
// /<name>, exactly as Compose mounts them.
func (c *parseContext) serviceGrants(raw any, kind string, declared map[string]DeclaredFile, serviceName, path string) []string {
	if raw == nil {
		return nil
	}
	entries := []any{}
	switch value := raw.(type) {
	case []any:
		entries = value
	default:
		c.error(path+"."+kind, kind+" must be a list of names or of {source, target} mappings")
		return nil
	}
	specs := []string{}
	for index, item := range entries {
		where := fmt.Sprintf("%s.%s.%d", path, kind, index)
		name, target := "", ""
		switch entry := item.(type) {
		case string:
			name = entry
		case map[string]any:
			classify(entry, grantFields, where, c.result)
			if source, ok := entry["source"]; ok {
				name = fmt.Sprint(source)
			}
			if value, ok := entry["target"]; ok {
				target = fmt.Sprint(value)
			}
		default:
			c.error(where, kind+" entries must be a name or {source, target}")
			continue
		}
		if name == "" {
			c.error(where, "a "+strings.TrimSuffix(kind, "s")+" entry needs a source")
			continue
		}
		source, ok := declared[name]
		if !ok {
			c.error(where, fmt.Sprintf("%s %q is not declared in the top-level %s block", strings.TrimSuffix(kind, "s"), name, kind))
			continue
		}
		if source.File == "" {
			// The declaration itself already reported why this cannot be
			// served; nothing more to say about the grant.
			continue
		}
		file, err := c.resolveProjectFile(source.File)
		if err != nil {
			// The declaration is where the file is named, so it is where the
			// problem is reported; repeating it for every service that asks for
			// the secret would bury it.
			continue
		}
		if target == "" {
			if kind == "secrets" {
				target = "/run/secrets/" + name
			} else {
				target = "/" + name
			}
		}
		if !strings.HasPrefix(target, "/") {
			c.error(where+".target", "the mount path inside the container must be absolute")
			continue
		}
		specs = append(specs, file+":"+target+":ro")
	}
	return dedupe(specs)
}

// projectPath resolves a path that Compose would read relative to the Compose
// file, and returns it relative to the project root. An empty value stays
// empty.
func (c *parseContext) projectPath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if strings.HasPrefix(value, "/") || strings.HasPrefix(value, "~") {
		return "", errors.New("Build context must stay inside the project.")
	}
	clean := path.Join(c.composeDir, filepath.ToSlash(value))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("Build context must stay inside the project.")
	}
	if clean == "." || clean == "" {
		return ".", nil
	}
	if c.source.Root != "" {
		if _, err := os.Stat(filepath.Join(c.source.Root, filepath.FromSlash(clean))); err != nil {
			return "", fmt.Errorf("build context %s is not present in the project source", clean)
		}
	}
	return clean, nil
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
			case "dockerfile_inline":
				return b, errors.New(buildDockerfileInlineError)
			case "target":
				b.Target = fmt.Sprint(val)
			case "args":
				args, err := parseStringMap(val, "build.args")
				if err != nil {
					return b, err
				}
				for key, item := range args {
					b.Args[key] = item
				}
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

// parseEnvironment reads both Compose spellings: the mapping and the list of
// KEY=VALUE entries.
//
// A bare `FOO` (or `FOO:` with no value) means "take FOO from the shell that
// ran docker compose". Asgard never exposes the control plane's environment to
// a workload, so the variable is reported and left unset rather than silently
// given some other value.
func parseEnvironment(value any, path string, result *ValidationResult) (map[string]string, error) {
	out := map[string]string{}
	if value == nil {
		return out, nil
	}
	inherited := []string{}
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			if item == nil {
				inherited = append(inherited, key)
				continue
			}
			out[key] = fmt.Sprint(item)
		}
	case []any:
		for _, item := range v {
			pair := strings.SplitN(fmt.Sprint(item), "=", 2)
			if len(pair) != 2 {
				inherited = append(inherited, pair[0])
				continue
			}
			out[pair[0]] = pair[1]
		}
	default:
		return nil, errors.New("environment must be a map or list")
	}
	if len(inherited) > 0 {
		sort.Strings(inherited)
		result.Warnings = append(result.Warnings, ValidationError{path, "These variables ask for the value in the shell that started the container: " + strings.Join(inherited, ", ") + ". Asgard never exposes its own environment to a workload, so they are left unset. Give each one a value in the .env file instead."})
	}
	if err := ValidateEnvironment(out); err != nil {
		return nil, err
	}
	return out, nil
}

// parseCommand reads Compose's two command spellings for `command` and
// `entrypoint`: a list, which is an argv handed to the entrypoint, and a string,
// which Compose splits into an argv with shell-word rules — not through a
// shell. The distinction matters for the flags-as-a-string idiom
// (`command: '--default-authentication-plugin=mysql_native_password'`), which
// only reaches the image's entrypoint if it is not first interpreted as a
// shell command line, and for quoting: `command: 'printf "%s\n" "x y"` means
// three arguments, not one.
//
// shellwords is the same parser compose-go uses, so a file behaves here exactly
// as it does under `docker compose`.
func parseCommand(value any, path string) ([]string, error) {
	if value == nil {
		return []string{}, nil
	}
	switch v := value.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return []string{}, nil
		}
		parts, err := shellwords.Parse(v)
		if err != nil {
			return nil, fmt.Errorf("command could not be parsed as a shell-word list: %w", err)
		}
		return parts, nil
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

// parseDependsOn reads both spellings of a dependency: a list of names and the
// long form that carries a condition. The condition itself is not a scheduling
// hint Asgard ignores quietly: the platform always waits for a dependency's
// own readiness gate, so a file that asked only for "started" is told that it
// gets more than it asked for.
func parseDependsOn(value any, path string) ([]string, []ValidationError) {
	out := []string{}
	warnings := []ValidationError{}
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			out = append(out, fmt.Sprint(item))
		}
	case map[string]any:
		for _, name := range sortedKeys(v) {
			out = append(out, name)
			options, _ := v[name].(map[string]any)
			condition := fmt.Sprint(options["condition"])
			if condition == "service_started" {
				warnings = append(warnings, ValidationError{path + ".depends_on." + name, "Asgard waits for this dependency's health gate before starting the dependent service, which is stricter than service_started."})
			}
			for key := range options {
				switch key {
				case "condition", "required", "restart":
				default:
					warnings = append(warnings, ValidationError{path + ".depends_on." + name + "." + key, UnsupportedFieldError})
				}
			}
		}
	}
	sort.Strings(out)
	return out, warnings
}

func firstPort(ports, expose []any) int {
	for _, value := range append(append([]any{}, ports...), expose...) {
		if port, ok := portNumber(value); ok {
			return port
		}
	}
	return 0
}

// digestPattern is the `@algorithm:hex` half of a pinned image reference.
var digestPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*([+._-][A-Za-z][A-Za-z0-9]*)*:[0-9a-fA-F]{32,}$`)

func ValidateImageReference(ref string) error {
	ref = strings.TrimSpace(ref)
	if ref == "" || len(ref) > 512 {
		return errors.New("invalid image reference")
	}
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		u, err := url.Parse(ref)
		if err != nil || !strings.EqualFold(u.Host, "hub.docker.com") {
			return errors.New("only Docker Hub URLs or OCI image references are supported")
		}
		return nil
	}
	if strings.ContainsAny(ref, " \t\r\n?&") {
		return errors.New("image reference contains unsupported characters")
	}
	repository, digest, pinned := strings.Cut(ref, "@")
	if repository == "" {
		return errors.New("invalid image reference")
	}
	if strings.Contains(repository, "@") {
		return errors.New("invalid image reference")
	}
	if pinned && !digestPattern.MatchString(digest) {
		return errors.New("an image digest must be written algorithm:hex, such as nginx@sha256:…")
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

// PlaceholderDomain stands in for the control plane's wildcard zone while a
// Compose file is being parsed. Parsing has no configuration in scope, so a
// service that asks for a public route without naming a hostname gets a name
// under this placeholder, and the caller — which does know the configured
// domain — rewrites it before the service is stored. It is a placeholder, not a
// default: nothing is ever served under it.
const PlaceholderDomain = "asgard.internal"

// ValidateHostname reports whether value is a bare DNS hostname.
func ValidateHostname(value string) bool {
	return len(value) <= 253 && hostnamePattern.MatchString(value)
}

// Reasons a hostname cannot back a public HTTPS route.
var (
	ErrHostnameInvalid  = errors.New("hostname must be a fully qualified DNS name such as app.example.com")
	ErrHostnameReserved = errors.New("the control-plane hostname is reserved")
)

// ValidatePublicHostname checks a hostname destined for a public HTTPS route.
//
// Any fully qualified name is accepted, not just names under the control
// plane's wildcard domain: a project may serve its own apex or subdomain as
// long as that name's DNS points at this host, and Traefik will request a
// certificate for it per-host over the HTTP-01 challenge. Two rules remain,
// and neither is about which zone the name lives in — a single-label name can
// never be issued a public certificate, and the control plane will not hand
// out its own name.
func ValidatePublicHostname(value, controlPlaneDomain string) error {
	value = strings.ToLower(strings.TrimSpace(value))
	if !ValidateHostname(value) || !strings.Contains(value, ".") {
		return ErrHostnameInvalid
	}
	if controlPlaneDomain != "" && value == strings.ToLower(strings.TrimSpace(controlPlaneDomain)) {
		return ErrHostnameReserved
	}
	return nil
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

// ValidateComposeLimits bounds the ceilings a Compose file may declare. They
// are wider than the operator-facing ones in ServiceSettings: a file is
// describing the workload it knows, and the wider floor is what lets a tiny
// sidecar with `mem_limit: 16m` import instead of being refused for asking for
// less than the platform's default.
func ValidateComposeLimits(cpu float64, memory, pids int64) error {
	if cpu < 0.01 || cpu > 64 {
		return errors.New("CPU limit must be between 0.01 and 64 cores.")
	}
	if memory < 1<<20 || memory > 256<<30 {
		return errors.New("Memory limit must be between 1 MiB and 256 GiB.")
	}
	if pids < 8 || pids > 65536 {
		return errors.New("PID limit must be between 8 and 65536.")
	}
	return nil
}

// stringList reads a list of scalars, accepting the single-value spelling too.
func stringList(value any) []string {
	out := []string{}
	switch v := value.(type) {
	case nil:
		return out
	case []any:
		for _, item := range v {
			out = append(out, fmt.Sprint(item))
		}
	case string:
		if strings.TrimSpace(v) != "" {
			out = append(out, v)
		}
	}
	return out
}

// parseStringMap reads Compose's two spellings for a set of key/value pairs: a
// mapping and a list of KEY=VALUE entries. It is used for labels, sysctls, and
// build arguments, which all accept both.
func parseStringMap(value any, field string) (map[string]string, error) {
	out := map[string]string{}
	switch v := value.(type) {
	case nil:
		return out, nil
	case map[string]any:
		for key, item := range v {
			if item == nil {
				return nil, fmt.Errorf("%s %q must have a value", field, key)
			}
			out[key] = fmt.Sprint(item)
		}
	case []any:
		for _, item := range v {
			pair := strings.SplitN(fmt.Sprint(item), "=", 2)
			if len(pair) != 2 {
				return nil, fmt.Errorf("%s entry %q must be written KEY=value", field, fmt.Sprint(item))
			}
			out[pair[0]] = pair[1]
		}
	default:
		return nil, fmt.Errorf("%s must be a mapping or a list of KEY=value entries", field)
	}
	return out, nil
}

// parseTmpfs reads the `tmpfs` key: one path, or a list of paths, possibly with
// options after a colon.
func parseTmpfs(value any) (map[string]string, error) {
	targets := map[string]string{}
	for _, entry := range stringListOrSingle(value) {
		target, options, _ := strings.Cut(entry, ":")
		if !strings.HasPrefix(target, "/") {
			return nil, errors.New("tmpfs paths must be absolute")
		}
		targets[target] = options
	}
	if len(targets) == 0 {
		return nil, errors.New("tmpfs must name at least one path")
	}
	return targets, nil
}

func stringListOrSingle(value any) []string {
	switch v := value.(type) {
	case string:
		return []string{v}
	default:
		return stringList(value)
	}
}

// parseUlimits reads the `ulimits` mapping: either a single number for both
// limits or an explicit soft/hard pair.
func parseUlimits(value any) ([]store.Ulimit, error) {
	entries, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("ulimits must be a mapping of name to a number or {soft, hard}")
	}
	out := make([]store.Ulimit, 0, len(entries))
	for _, name := range sortedKeys(entries) {
		item := store.Ulimit{Name: name}
		switch entry := entries[name].(type) {
		case map[string]any:
			soft, err := parseCount(entry["soft"])
			if err != nil {
				return nil, fmt.Errorf("ulimits.%s.soft: %w", name, err)
			}
			hard, err := parseCount(entry["hard"])
			if err != nil {
				return nil, fmt.Errorf("ulimits.%s.hard: %w", name, err)
			}
			item.Soft, item.Hard = soft, hard
		default:
			limit, err := parseCount(entries[name])
			if err != nil {
				return nil, fmt.Errorf("ulimits.%s: %w", name, err)
			}
			item.Soft, item.Hard = limit, limit
		}
		if item.Soft < 0 || item.Hard < 0 {
			return nil, fmt.Errorf("ulimits.%s must not be negative", name)
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// shortHash gives a stable, short suffix for a generated volume name.
func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:12]
}

// dedupe removes repeated names while keeping their order.
func dedupe(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

// truthy reads the several ways YAML and Compose spell a boolean.
func truthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "yes", "on", "1":
			return true
		}
		return false
	default:
		text := strings.ToLower(strings.TrimSpace(fmt.Sprint(value)))
		return text == "true" || text == "1"
	}
}
