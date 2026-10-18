package composecfg

import (
	"sort"
	"strings"
)

// Every key of the Compose schema is classified, and the classification is
// part of the contract an importing agent sees.
//
// The old layer classified nothing: any key outside a short allow list failed
// the import with one generic message, so an ordinary project — a `logging:`
// block, a `profiles:` list, a `container_name:` — was refused wholesale and
// had to be edited before Asgard would take it. The three policies below are
// the whole point of this file. A field Asgard reads is applied; a field with
// no Asgard equivalent is accepted and reported as a warning that names it, so
// the workload's author can see exactly which promise the platform is not
// keeping; and only a field that would break the sandbox, reach the host, or
// contradict the release model is refused.
type Policy string

const (
	// Applied: the field is read and reaches the container Asgard runs.
	Applied Policy = "applied"
	// Ignored: the field is understood, has no effect here, and is reported as
	// a warning on the import. The import succeeds.
	Ignored Policy = "ignored"
	// Rejected: honoring the field would let a workload out of its sandbox or
	// contradict Asgard's release model, so the import fails with the note.
	Rejected Policy = "rejected"
)

// Field is one classified Compose key.
type Field struct {
	Name   string `json:"name"`
	Policy Policy `json:"policy"`
	// Note explains an ignored or rejected field: what the platform does
	// instead, or why the request is refused. Applied fields need no note.
	Note string `json:"note,omitempty"`
}

// ExtensionPrefix is Compose's escape hatch: any key starting with it is
// project-local metadata, legal at every level of the document. Real files use
// it for anchors — an `x-logging` block that every service merges in with
// `<<: *logging` — so refusing it refused ordinary, well-factored files.
const ExtensionPrefix = "x-"

// mergeKey is YAML's mapping merge (`<<: *anchor`). It is resolved before
// validation, so it never reaches the key tables.
const mergeKey = "<<"

// UnsupportedFieldError is the message for a key that is not part of Compose at
// all: a typo rather than a feature.
// buildDockerfileInlineError refuses a Dockerfile written inside the Compose
// file. Asgard builds the project's own source tree, where a Dockerfile is a
// file the project ships; an inline one would have to be written into that tree
// to be buildable, and then it would be the tree's file rather than the file's.
const buildDockerfileInlineError = "An inline Dockerfile is not supported; ship the Dockerfile next to the build context."
const UnsupportedFieldError = "This field is not supported by Asgard's safe Compose contract."

// Notes shared by whole families of keys, so the reason stays consistent.
const (
	noteResourceKnob = "Asgard sets CPU, memory, and PID limits through its own per-service limits; this cgroup-level setting is not applied."
	noteNoHostAccess = "Asgard runs workloads without access to the host."
)

// fieldTable is an immutable list of classified keys plus the lookup built from
// it. Building the lookup once, at package initialization, keeps Parse free of
// shared mutable state: imports run concurrently in the control plane.
type fieldTable struct {
	fields []Field
	index  map[string]Field
}

func newFieldTable(fields ...Field) *fieldTable {
	index := make(map[string]Field, len(fields))
	for _, field := range fields {
		index[field.Name] = field
	}
	return &fieldTable{fields: fields, index: index}
}

// policy reports how one key is classified. The boolean is false for a key that
// is not in the table at all, which is a typo rather than a Compose feature.
func (t *fieldTable) policy(key string) (Field, bool) {
	field, ok := t.index[key]
	return field, ok
}

// topLevelFields classifies the keys of the document root.
var topLevelFields = newFieldTable(
	Field{Name: "name", Policy: Applied},
	Field{Name: "version", Policy: Applied},
	Field{Name: "services", Policy: Applied},
	Field{Name: "volumes", Policy: Applied},
	Field{Name: "networks", Policy: Applied},
	Field{Name: "x-asgard", Policy: Applied},
	Field{Name: "include", Policy: Rejected, Note: "Compose `include` pulls services in from other files, which would place them outside the imported source tree. Inline the included file or import it as its own project."},
	Field{Name: "configs", Policy: Applied},
	Field{Name: "secrets", Policy: Applied},
)

// serviceFields classifies every key Compose accepts under a service. The set
// is the Compose Specification's service schema: a key that is missing from it
// would be refused as a typo, and one classified Ignored is accepted with a
// warning. Keep it complete.
var serviceFields = newFieldTable(
	// --- Applied: build, run, and configure the container --------------
	Field{Name: "image", Policy: Applied},
	Field{Name: "build", Policy: Applied},
	Field{Name: "command", Policy: Applied},
	Field{Name: "entrypoint", Policy: Applied},
	Field{Name: "environment", Policy: Applied},
	Field{Name: "env_file", Policy: Applied},
	Field{Name: "ports", Policy: Applied},
	Field{Name: "expose", Policy: Applied},
	Field{Name: "volumes", Policy: Applied},
	Field{Name: "depends_on", Policy: Applied},
	Field{Name: "healthcheck", Policy: Applied},
	Field{Name: "restart", Policy: Applied},
	Field{Name: "working_dir", Policy: Applied},
	Field{Name: "user", Policy: Applied},
	Field{Name: "labels", Policy: Applied},
	Field{Name: "hostname", Policy: Applied},
	Field{Name: "platform", Policy: Applied},
	Field{Name: "read_only", Policy: Applied},
	Field{Name: "shm_size", Policy: Applied},
	Field{Name: "stop_grace_period", Policy: Applied},
	Field{Name: "stop_signal", Policy: Applied},
	Field{Name: "tmpfs", Policy: Applied},
	Field{Name: "sysctls", Policy: Applied},
	Field{Name: "ulimits", Policy: Applied},
	Field{Name: "mem_limit", Policy: Applied},
	Field{Name: "cpus", Policy: Applied},
	Field{Name: "pids_limit", Policy: Applied},
	Field{Name: "deploy", Policy: Applied},

	// Networks are accepted: a service that names the project's own networks,
	// or the default one, is describing exactly the topology Asgard builds. A
	// membership in a network that is not the project's is reported by the
	// parse step, which is where the top-level `networks` block is known.
	Field{Name: "networks", Policy: Applied},

	// --- Ignored: understood, no effect here, reported as a warning ----
	Field{Name: "container_name", Policy: Ignored, Note: "Asgard names containers after the project, service, and release so a release can be rolled back; a fixed name cannot be honored. Address the service by its Compose service name instead."},
	Field{Name: "init", Policy: Ignored, Note: "Asgard always runs containers with an init process."},
	Field{Name: "tty", Policy: Ignored, Note: "Asgard runs services unattended and attaches no terminal."},
	Field{Name: "stdin_open", Policy: Ignored, Note: "Asgard runs services unattended and attaches no standard input."},
	Field{Name: "attach", Policy: Ignored, Note: "Asgard runs services unattended and attaches no terminal."},
	Field{Name: "logging", Policy: Ignored, Note: "Asgard collects every container's output itself, with a fixed local driver and rotation; a Compose logging block is not applied."},
	Field{Name: "pull_policy", Policy: Ignored, Note: "Asgard builds from the project's source or pulls the image on every release."},
	Field{Name: "profiles", Policy: Ignored, Note: "Asgard deploys every service the file declares; Compose profiles are not modeled, so a service under a profile runs too."},
	Field{Name: "dns", Policy: Ignored, Note: "A workload uses the project network's resolver; host DNS settings are not applied."},
	Field{Name: "dns_opt", Policy: Ignored, Note: "A workload uses the project network's resolver; host DNS settings are not applied."},
	Field{Name: "dns_search", Policy: Ignored, Note: "A workload uses the project network's resolver; host DNS settings are not applied."},
	Field{Name: "domainname", Policy: Ignored, Note: "Asgard does not set a container's NIS domain name."},
	Field{Name: "links", Policy: Ignored, Note: "Same-project services already resolve each other by Compose service name."},
	Field{Name: "external_links", Policy: Ignored, Note: "Cross-project access is granted deliberately through an Asgard shared network, not by linking to another project's container."},
	Field{Name: "group_add", Policy: Ignored, Note: "Supplementary host groups are not granted to workloads."},
	Field{Name: "credential_spec", Policy: Ignored, Note: "Windows credential specs do not apply on this host."},
	Field{Name: "isolation", Policy: Ignored, Note: "Windows isolation modes do not apply on this host."},
	Field{Name: "scale", Policy: Ignored, Note: "Asgard runs one container per service per release."},
	Field{Name: "develop", Policy: Ignored, Note: "Compose's develop/watch block is a local workflow; Asgard deploys a release instead."},
	Field{Name: "label_file", Policy: Ignored, Note: "Only inline labels are read."},
	Field{Name: "annotations", Policy: Ignored, Note: "Container annotations are not propagated to the runtime."},
	Field{Name: "provider", Policy: Ignored, Note: "Compose's provider services are not run."},
	Field{Name: "models", Policy: Ignored, Note: "Compose's model services are not run."},
	Field{Name: "post_start", Policy: Ignored, Note: "Lifecycle hooks are not run; use depends_on so a dependent service gates on this one's health."},
	Field{Name: "pre_stop", Policy: Ignored, Note: "Lifecycle hooks are not run; the container receives SIGTERM and stops within the grace period."},
	Field{Name: "cgroup", Policy: Ignored, Note: noteResourceKnob},
	Field{Name: "cpu_count", Policy: Ignored, Note: noteResourceKnob},
	Field{Name: "cpu_percent", Policy: Ignored, Note: noteResourceKnob},
	Field{Name: "cpu_period", Policy: Ignored, Note: noteResourceKnob},
	Field{Name: "cpu_quota", Policy: Ignored, Note: noteResourceKnob},
	Field{Name: "cpu_rt_period", Policy: Ignored, Note: noteResourceKnob},
	Field{Name: "cpu_rt_runtime", Policy: Ignored, Note: noteResourceKnob},
	Field{Name: "cpu_shares", Policy: Ignored, Note: noteResourceKnob},
	Field{Name: "cpuset", Policy: Ignored, Note: noteResourceKnob},
	Field{Name: "blkio_config", Policy: Ignored, Note: "Block-IO weighting is not applied; Asgard reports per-service block I/O instead."},
	Field{Name: "storage_opt", Policy: Ignored, Note: "Per-container storage options are not applied."},
	Field{Name: "mem_reservation", Policy: Ignored, Note: noteResourceKnob},
	Field{Name: "mem_swappiness", Policy: Ignored, Note: noteResourceKnob},
	Field{Name: "memswap_limit", Policy: Ignored, Note: noteResourceKnob},
	Field{Name: "oom_kill_disable", Policy: Ignored, Note: "Asgard's memory limit is enforced with the container's OOM killer enabled."},
	Field{Name: "oom_score_adj", Policy: Ignored, Note: noteResourceKnob},
	Field{Name: "mac_address", Policy: Ignored, Note: "Addresses on the project network are assigned by Docker."},

	// --- Rejected: would break the sandbox or the release model --------
	Field{Name: "privileged", Policy: Rejected, Note: noteNoHostAccess + " Privileged mode grants the host's kernel devices and capabilities."},
	Field{Name: "network_mode", Policy: Rejected, Note: noteNoHostAccess + " A workload joins the project network and, if it is public, the edge network; host or container network mode is refused."},
	Field{Name: "pid", Policy: Rejected, Note: "Sharing a PID namespace would expose the host's or another project's processes."},
	Field{Name: "ipc", Policy: Rejected, Note: "Sharing IPC with the host or another container would expose their shared memory."},
	Field{Name: "uts", Policy: Rejected, Note: "Sharing the host UTS namespace would let the container change the host's name."},
	Field{Name: "userns_mode", Policy: Rejected, Note: "The user namespace mode is fixed by the host's Docker daemon."},
	Field{Name: "devices", Policy: Rejected, Note: noteNoHostAccess + " Device passthrough is refused."},
	Field{Name: "gpus", Policy: Rejected, Note: noteNoHostAccess + " GPU device requests are refused."},
	Field{Name: "device_cgroup_rules", Policy: Rejected, Note: noteNoHostAccess + " Device cgroup rules are refused."},
	Field{Name: "cap_add", Policy: Rejected, Note: "Asgard drops all capabilities and adds back only the ones common images need to initialize their own files. Additions are refused."},
	Field{Name: "cap_drop", Policy: Rejected, Note: "Asgard already drops every capability; a narrower or wider set is not configurable per service."},
	Field{Name: "security_opt", Policy: Rejected, Note: "Asgard always runs with no-new-privileges and the host's default seccomp and AppArmor profiles; other security options are refused."},
	Field{Name: "extra_hosts", Policy: Rejected, Note: "Writing /etc/hosts entries would let a workload reach addresses outside its project network. Use a shared network for another project or a public hostname for anything else."},
	Field{Name: "cgroup_parent", Policy: Rejected, Note: "A parent cgroup would place the workload outside the limits Asgard enforces."},
	Field{Name: "runtime", Policy: Rejected, Note: "The container runtime is fixed by the host's Docker daemon."},
	Field{Name: "volumes_from", Policy: Rejected, Note: "Declare the named volume on the service that needs it instead of inheriting another container's volumes."},
	Field{Name: "secrets", Policy: Applied},
	Field{Name: "configs", Policy: Applied},
	Field{Name: "extends", Policy: Rejected, Note: "Compose `extends` pulls a service definition from another file, which may live outside the imported source tree. Use a YAML anchor (`<<: *base`) inside this file instead."},
)

// buildFields classifies the keys of a build section. The context, Dockerfile,
// arguments, and target are read; everything else either changes how the build
// runs or how it is cached, and is reported rather than silently dropped.
var buildFields = newFieldTable(
	Field{Name: "context", Policy: Applied},
	Field{Name: "dockerfile", Policy: Applied},
	Field{Name: "dockerfile_inline", Policy: Rejected, Note: buildDockerfileInlineError},
	Field{Name: "args", Policy: Applied},
	Field{Name: "target", Policy: Applied},
	Field{Name: "platforms", Policy: Ignored, Note: "Asgard builds for the host's architecture."},
	Field{Name: "labels", Policy: Ignored, Note: "Image labels from Compose are not applied."},
	Field{Name: "cache_from", Policy: Ignored, Note: "Asgard uses its own build cache."},
	Field{Name: "cache_to", Policy: Ignored, Note: "Asgard uses its own build cache."},
	Field{Name: "pull", Policy: Ignored, Note: "Asgard always builds with the current base images."},
	Field{Name: "no_cache", Policy: Ignored, Note: "Asgard builds each release from the imported source."},
	Field{Name: "network", Policy: Ignored, Note: "The build runs on the host's default build network."},
	Field{Name: "shm_size", Policy: Ignored, Note: "The build uses Docker's default shared memory size."},
	Field{Name: "tags", Policy: Ignored, Note: "Asgard tags the image it builds."},
	Field{Name: "ulimits", Policy: Ignored, Note: noteResourceKnob},
	Field{Name: "isolation", Policy: Ignored, Note: "Windows build isolation does not apply on this host."},
	Field{Name: "extra_hosts", Policy: Rejected, Note: "Build-time host entries would reach addresses outside the project. Rely on the network the build already has."},
	Field{Name: "ssh", Policy: Rejected, Note: "Build-time SSH agents are not mounted. Vendor the dependency or fetch it over HTTPS."},
	Field{Name: "secrets", Policy: Rejected, Note: "Build-time secrets are not mounted. Copy what the build needs from the project source."},
	Field{Name: "privileged", Policy: Rejected, Note: "Privileged builds are refused."},
	Field{Name: "entitlements", Policy: Rejected, Note: "Build entitlements would extend the build beyond the sandbox."},
)

// healthcheckFields classifies the keys of a healthcheck block.
var healthcheckFields = newFieldTable(
	Field{Name: "test", Policy: Applied},
	Field{Name: "interval", Policy: Applied},
	Field{Name: "timeout", Policy: Applied},
	Field{Name: "retries", Policy: Applied},
	Field{Name: "start_period", Policy: Applied},
	Field{Name: "start_interval", Policy: Applied},
	Field{Name: "disable", Policy: Applied},
)

// deployFields classifies the keys of a deploy block: the Compose section
// written for orchestrators. Asgard is the orchestrator here, so the resource
// limits are read and the scheduling directives are reported.
var deployFields = newFieldTable(
	Field{Name: "resources", Policy: Applied},
	Field{Name: "replicas", Policy: Ignored, Note: "Asgard runs one container per service per release."},
	Field{Name: "mode", Policy: Ignored, Note: "Asgard runs one container per service per release."},
	Field{Name: "placement", Policy: Ignored, Note: "There is a single host; placement constraints have nothing to choose between."},
	Field{Name: "update_config", Policy: Ignored, Note: "Asgard's release process — build, health gate, switch traffic, retire — replaces it."},
	Field{Name: "rollback_config", Policy: Ignored, Note: "Asgard's release process — build, health gate, switch traffic, retire — replaces it."},
	Field{Name: "restart_policy", Policy: Ignored, Note: "Use the service's own `restart` key; Asgard applies it to the container."},
	Field{Name: "endpoint_mode", Policy: Ignored, Note: "Endpoint mode is a swarm concept."},
	Field{Name: "labels", Policy: Ignored, Note: "Deploy labels are not applied; service labels are."},
	Field{Name: "healthcheck", Policy: Ignored, Note: "Use the service's own `healthcheck` block; Asgard applies it to the container."},
)

// resourcesFields classifies deploy.resources.
var resourcesFields = newFieldTable(
	Field{Name: "limits", Policy: Applied},
	Field{Name: "reservations", Policy: Ignored, Note: "Asgard enforces a ceiling per service; reservations are not reserved on the host."},
)

// limitsFields classifies deploy.resources.limits.
var limitsFields = newFieldTable(
	Field{Name: "cpus", Policy: Applied},
	Field{Name: "memory", Policy: Applied},
	Field{Name: "pids", Policy: Applied},
	Field{Name: "devices", Policy: Rejected, Note: noteNoHostAccess + " Device passthrough is refused."},
)

// xAsgardFields classifies the routing block.
var xAsgardFields = newFieldTable(
	Field{Name: "primary-service", Policy: Applied},
	Field{Name: "services", Policy: Applied},
)

// volumeDeclFields classifies one entry of the top-level `volumes` block.
// Asgard keeps the project's own volume for every logical name, so a
// declaration that asks for a host-side name or for external data is reported
// by the parse step, which is where the warning has context.
var volumeDeclFields = newFieldTable(
	Field{Name: "external", Policy: Applied},
	Field{Name: "name", Policy: Applied},
	Field{Name: "driver", Policy: Ignored, Note: "Asgard creates project volumes with the local driver."},
	Field{Name: "driver_opts", Policy: Ignored, Note: "Volume driver options are not applied."},
	Field{Name: "labels", Policy: Ignored, Note: "Volume labels are not applied."},
)

// networkDeclFields classifies one entry of the top-level `networks` block.
var networkDeclFields = newFieldTable(
	Field{Name: "external", Policy: Applied},
	Field{Name: "name", Policy: Applied},
	Field{Name: "internal", Policy: Ignored, Note: "Asgard's project network is not routable from outside the host; a shared network can be created as internal when cross-project traffic needs no egress."},
	Field{Name: "driver", Policy: Ignored, Note: "Services join the project's own bridge network."},
	Field{Name: "driver_opts", Policy: Ignored, Note: "Services join the project's own bridge network."},
	Field{Name: "attachable", Policy: Ignored, Note: "Services join the project's own bridge network."},
	Field{Name: "enable_ipv6", Policy: Ignored, Note: "IPv6 on the project network is set by the host's Docker daemon."},
	Field{Name: "enable_ipv4", Policy: Ignored, Note: "Services join the project's own bridge network."},
	Field{Name: "ipam", Policy: Ignored, Note: "Subnets are assigned by the host's Docker daemon."},
	Field{Name: "labels", Policy: Ignored, Note: "Network labels are not applied."},
)

// secretDeclFields classifies one entry of the top-level `secrets` block. A
// file the project itself ships is served to the service that asks for it; a
// secret that lives in the host's store, or that comes from the environment
// that ran the container, is not.
var secretDeclFields = newFieldTable(
	Field{Name: "file", Policy: Applied},
	Field{Name: "external", Policy: Rejected, Note: "An external secret lives in the host's own secret store, which Asgard does not read. Ship the value as a file in the project, or put it in the project's .env file."},
	Field{Name: "environment", Policy: Rejected, Note: "An environment-backed secret is read from the host that runs the container, and Asgard never exposes its own environment to a workload. Ship the value as a file in the project."},
	Field{Name: "name", Policy: Rejected, Note: "Naming a secret that already exists on the host would attach something outside the project. Ship the value as a file in the project."},
	Field{Name: "content", Policy: Rejected, Note: "Inline secret content would have to be written somewhere outside the project's source. Ship the file in the project instead."},
	Field{Name: "labels", Policy: Ignored, Note: "Secret labels are not applied."},
	Field{Name: "driver", Policy: Ignored, Note: "Asgard serves the secret from the project's own source."},
	Field{Name: "driver_opts", Policy: Ignored, Note: "Asgard serves the secret from the project's own source."},
	Field{Name: "template_driver", Policy: Ignored, Note: "Template rendering is not applied; the file is served as it is."},
)

// configDeclFields classifies one entry of the top-level `configs` block.
var configDeclFields = newFieldTable(
	Field{Name: "file", Policy: Applied},
	Field{Name: "external", Policy: Rejected, Note: "An external config lives in the host's own config store, which Asgard does not read. Ship the file in the project instead."},
	Field{Name: "name", Policy: Rejected, Note: "Naming a config that already exists on the host would attach something outside the project. Ship the file in the project instead."},
	Field{Name: "content", Policy: Rejected, Note: "Inline config content would have to be written somewhere outside the project's source. Ship the file in the project instead."},
	Field{Name: "environment", Policy: Rejected, Note: "An environment-backed config is read from the host that runs the container. Ship the file in the project instead."},
	Field{Name: "labels", Policy: Ignored, Note: "Config labels are not applied."},
)

// grantFields classifies one service-to-secret or service-to-config grant.
// The source, the target path, and nothing else: Compose's uid, gid, and mode
// would require Asgard to copy the file somewhere with different ownership,
// and the file is served read-only from the project's own source instead.
var grantFields = newFieldTable(
	Field{Name: "source", Policy: Applied},
	Field{Name: "target", Policy: Applied},
	Field{Name: "uid", Policy: Ignored, Note: "The file keeps the ownership it has in the project source; a workload reads it as whatever user it runs as."},
	Field{Name: "gid", Policy: Ignored, Note: "The file keeps the ownership it has in the project source; a workload reads it as whatever user it runs as."},
	Field{Name: "mode", Policy: Ignored, Note: "The file keeps the mode it has in the project source."},
)

// networkMembershipFields classifies the options of one service-to-network
// membership.
var networkMembershipFields = newFieldTable(
	Field{Name: "aliases", Policy: Applied},
	Field{Name: "ipv4_address", Policy: Ignored, Note: "Addresses on the project network are assigned by Docker."},
	Field{Name: "ipv6_address", Policy: Ignored, Note: "Addresses on the project network are assigned by Docker."},
	Field{Name: "link_local_ips", Policy: Ignored, Note: "Addresses on the project network are assigned by Docker."},
	Field{Name: "mac_address", Policy: Ignored, Note: "Addresses on the project network are assigned by Docker."},
	Field{Name: "priority", Policy: Ignored, Note: "A service joins the project network through a single interface."},
	Field{Name: "interface_name", Policy: Ignored, Note: "A service joins the project network through a single interface."},
	Field{Name: "gw_priority", Policy: Ignored, Note: "Gateways are assigned by the host's Docker daemon."},
)

// xAsgardServiceFields classifies one entry of x-asgard.services.
var xAsgardServiceFields = newFieldTable(
	Field{Name: "role", Policy: Applied},
	Field{Name: "public", Policy: Applied},
	Field{Name: "port", Policy: Applied},
	Field{Name: "health-path", Policy: Applied},
	Field{Name: "hostname", Policy: Applied},
)

// classify walks the decoded keys of one mapping and records what each policy
// implies: nothing for an applied field, a warning naming an ignored one, an
// error for a rejected one. Extension keys and the merge key are legal
// everywhere and produce nothing — the merge key has already been resolved by
// the time a decoded mapping is walked, so a merged field is classified here
// exactly as if it had been written inline.
//
// accepted names keys the caller has already satisfied: a rejected field whose
// request is exactly what Asgard already does (a cap_drop of ALL, a
// security_opt of no-new-privileges) is not a request for more privilege.
func classify(mapping map[string]any, table *fieldTable, path string, result *ValidationResult, accepted ...string) {
	allowed := map[string]bool{}
	for _, key := range accepted {
		allowed[key] = true
	}
	for _, key := range sortedKeys(mapping) {
		if key == mergeKey || strings.HasPrefix(key, ExtensionPrefix) || allowed[key] {
			continue
		}
		field, known := table.policy(key)
		if !known {
			result.Errors = append(result.Errors, ValidationError{path + "." + key, UnsupportedFieldError})
			continue
		}
		switch field.Policy {
		case Ignored:
			result.Warnings = append(result.Warnings, ValidationError{path + "." + key, "Ignored: " + field.Note})
		case Rejected:
			result.Errors = append(result.Errors, ValidationError{path + "." + key, field.Note})
		}
	}
}

// classifyNested runs classify over a nested mapping when the key holds one.
func classifyNested(mapping map[string]any, key string, table *fieldTable, path string, result *ValidationResult) {
	child, ok := mapping[key].(map[string]any)
	if !ok {
		return
	}
	classify(child, table, path+"."+key, result)
}

// sortedKeys returns a mapping's keys in a stable order, so two imports of the
// same file produce the same list of warnings.
func sortedKeys(mapping map[string]any) []string {
	keys := make([]string, 0, len(mapping))
	for key := range mapping {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
