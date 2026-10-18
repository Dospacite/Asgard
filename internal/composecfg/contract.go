package composecfg

import "sort"

// Contract is what an importing agent reads before it writes a Compose file:
// the exact subset Asgard accepts, and what happens to every key of the Compose
// schema that is not in it.
//
// It is generated from the same tables the parser uses, so the contract cannot
// drift from the behaviour it describes — the previous version was a hand-kept
// list next to a hand-kept parser, and the two had already disagreed.
var Contract = buildContract()

func buildContract() map[string]any {
	return map[string]any{
		"version":  "1.1",
		"topLevel": appliedNames(topLevelFields),
		"rejected": append(rejectedNames(topLevelFields), rejectedNames(serviceFields)...),
		"ignored":  ignoredNames(serviceFields),
		// fieldNotes explains every key that is not applied — why it has no
		// effect here, or why it is refused.
		"fieldNotes":    fieldNotes(),
		"serviceFields": appliedNames(serviceFields),
		"nestedFields": map[string]any{
			"build":                    appliedNames(buildFields),
			"healthcheck":              appliedNames(healthcheckFields),
			"deploy":                   appliedNames(deployFields),
			"deploy.resources":         appliedNames(resourcesFields),
			"deploy.resources.limits":  appliedNames(limitsFields),
			"volumes[]":                appliedNames(volumeFields),
			"networks.<name>":          appliedNames(networkMembershipFields),
			"x-asgard":                 appliedNames(xAsgardFields),
			"x-asgard.services.<name>": appliedNames(xAsgardServiceFields),
		},
		"roles":    []string{"web", "worker", "stateful"},
		"defaults": map[string]any{"cpu": 0.5, "memoryBytes": 512 << 20, "pids": 256, "healthPath": "/"},
		// What the container itself is given, beyond the fields above.
		"container": map[string]any{
			"nonRootUser":  "any uid, name, or uid:gid",
			"capabilities": []string{"CHOWN", "DAC_OVERRIDE", "FOWNER", "SETGID", "SETUID"},
			"alwaysOn":     []string{"no-new-privileges", "all capabilities dropped", "an init process", "the host's seccomp and AppArmor profiles", "Asgard's own log driver with rotation"},
		},
		"volumeSources": []string{
			"declared named volumes, which Asgard scopes to the project as asgard-<project>-<volume>",
			"project-relative paths such as ./config/app.json, resolved against the Compose file's directory",
			"a path the project does not ship but that has no file extension, such as ./data, which is created in the project's source tree (declare it as a named volume if the data should outlive the project)",
		},
		"volumeModes":   []string{"ro", "rw", "z", "Z", "cached", "delegated", "consistent", "nocopy", "shared", "slave", "private", "rshared", "rslave", "rprivate"},
		"relativePaths": "Relative paths — the build context, env_file entries, project-relative volume sources, and the project's .env — resolve against the directory holding the Compose file, exactly as Compose resolves them.",
		"profiles":      "Asgard deploys every service the file declares; Compose profiles are reported and the service runs anyway.",
		"ports":         "A published host port is not used: Asgard gives a service an internal port and fronts it with Traefik. The container port, taken from ports or expose, is what routes.",
		"healthcheck":   "A declared healthcheck is applied to the container and its start period and retries also set how long the release waits before giving up.",
		"oneShot":       "A service with restart: \"no\" and no healthcheck is a job: Asgard waits for it to finish, exiting zero is success, and any other exit fails the release. That is what depends_on's service_completed_successfully waits for. A service that declares a healthcheck, or that carries any other restart policy, is judged as a service instead; under on-failure a clean exit is still accepted as success.",
		"restart":       []string{"no", "always", "on-failure", "on-failure:<retries>", "unless-stopped"},
		"interpolation": map[string]any{
			"source": "the project's own .env file, beside the Compose file; the control plane's environment is never exposed",
			"forms":  []string{"$NAME", "${NAME}", "${NAME:-default}", "${NAME-default}", "${NAME:+alt}", "${NAME+alt}", "${NAME:?error}", "${NAME?error}", "$$ for a literal dollar"},
		},
		"extension": map[string]any{
			"x-asgard": map[string]any{"primary-service": "service name", "services": map[string]any{"<name>": map[string]any{"role": "web|worker|stateful", "public": "boolean", "port": "internal container port", "health-path": "HTTP path", "hostname": "optional hostname under the configured wildcard"}}},
			"x-*":      "Any key starting with x- is project-local metadata and is ignored at every level. YAML anchors and merge keys (`<<: *base`) are resolved before validation.",
		},
	}
}

// appliedNames lists the keys a table applies, sorted.
func appliedNames(table *fieldTable) []string {
	names := []string{}
	for _, field := range table.fields {
		if field.Policy == Applied {
			names = append(names, field.Name)
		}
	}
	sort.Strings(names)
	return names
}

func rejectedNames(table *fieldTable) []string {
	names := []string{}
	for _, field := range table.fields {
		if field.Policy == Rejected {
			names = append(names, field.Name)
		}
	}
	sort.Strings(names)
	return names
}

func ignoredNames(table *fieldTable) []string {
	names := []string{}
	for _, field := range table.fields {
		if field.Policy == Ignored {
			names = append(names, field.Name)
		}
	}
	sort.Strings(names)
	return names
}

// union merges name lists, dropping repeats and sorting the result.
func union(lists ...[]string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, list := range lists {
		for _, name := range list {
			if seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// fieldNotes maps every key that is not applied to the reason it is reported,
// keyed by the path shape it appears at: a field name alone is ambiguous, since
// `ulimits` is applied on a service and reported inside a build.
func fieldNotes() map[string]string {
	notes := map[string]string{}
	for _, scoped := range []struct {
		prefix string
		table  *fieldTable
	}{
		{"compose", topLevelFields},
		{"services.<name>", serviceFields},
		{"services.<name>.build", buildFields},
		{"services.<name>.healthcheck", healthcheckFields},
		{"services.<name>.deploy", deployFields},
		{"services.<name>.deploy.resources", resourcesFields},
		{"services.<name>.deploy.resources.limits", limitsFields},
		{"services.<name>.volumes[]", volumeFields},
		{"services.<name>.networks.<name>", networkMembershipFields},
		{"services.<name>.secrets[]", grantFields},
		{"volumes.<name>", volumeDeclFields},
		{"networks.<name>", networkDeclFields},
		{"secrets.<name>", secretDeclFields},
		{"configs.<name>", configDeclFields},
	} {
		for _, field := range scoped.table.fields {
			if field.Policy == Applied {
				continue
			}
			notes[scoped.prefix+"."+field.Name] = field.Note
		}
	}
	return notes
}
