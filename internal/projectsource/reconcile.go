package projectsource

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/rousoftware/asgard/internal/store"
)

// ReconcileReport says what applying a Compose file did to each service's
// stored configuration. A parsed preview shows only what the file asks for; an
// operator also needs to know which of those values took effect and which an
// earlier override held back, or a change that never reaches the containers
// looks applied.
type ReconcileReport struct {
	Services []ServiceChange `json:"services"`
}

// ServiceChange describes one service. Field names follow the service's JSON
// form; environment variables appear as environment.NAME. Values are never
// included, because environments hold secrets.
type ServiceChange struct {
	Service string `json:"service"`
	// Action is added, updated, or unchanged.
	Action string `json:"action"`
	// Changed lists the stored fields this reconcile changed.
	Changed []string `json:"changed,omitempty"`
	// KeptOverrides lists fields where the stored value was set through Asgard
	// and still differs from the file, so the file's value was not applied.
	// Change them with a service configuration update to let the file's value
	// through.
	KeptOverrides []string `json:"keptOverrides,omitempty"`
}

// Service returns the change recorded for one service.
func (r ReconcileReport) Service(name string) ServiceChange {
	for _, change := range r.Services {
		if change.Service == name {
			return change
		}
	}
	return ServiceChange{}
}

// Changed reports whether any stored service configuration changed.
func (r ReconcileReport) Changed() bool {
	for _, change := range r.Services {
		if change.Action != "unchanged" {
			return true
		}
	}
	return false
}

// Summary is a one-line account for logs and audit entries.
func (r ReconcileReport) Summary() string {
	var added, updated, overridden []string
	for _, change := range r.Services {
		switch change.Action {
		case "added":
			added = append(added, change.Service)
		case "updated":
			updated = append(updated, change.Service)
		}
		if len(change.KeptOverrides) > 0 {
			overridden = append(overridden, fmt.Sprintf("%s (%s)", change.Service, strings.Join(change.KeptOverrides, ", ")))
		}
	}
	parts := []string{}
	if len(added) > 0 {
		parts = append(parts, "added "+strings.Join(added, ", "))
	}
	if len(updated) > 0 {
		parts = append(parts, "updated "+strings.Join(updated, ", "))
	}
	if len(parts) == 0 {
		parts = append(parts, "no service configuration changed")
	}
	if len(overridden) > 0 {
		parts = append(parts, "kept Asgard overrides on "+strings.Join(overridden, "; "))
	}
	return strings.Join(parts, "; ")
}

// describeChange compares the stored service with the merged one that replaces
// it, and the merged one with what the file asked for.
func describeChange(current, merged, parsed store.Service) ServiceChange {
	change := ServiceChange{Service: current.Name, Action: "unchanged"}
	fields := []struct {
		name             string
		before, after    any
		fromFile         any
		operatorEditable bool
	}{
		{"role", current.Role, merged.Role, parsed.Role, true},
		{"image", current.Image, merged.Image, parsed.Image, false},
		{"buildContext", current.BuildContext, merged.BuildContext, parsed.BuildContext, false},
		{"dockerfile", current.Dockerfile, merged.Dockerfile, parsed.Dockerfile, false},
		{"command", emptyAsNil(current.Command), emptyAsNil(merged.Command), nil, false},
		{"container", current.Container, merged.Container, nil, false},
		{"public", current.Public, merged.Public, parsed.Public, true},
		{"port", current.Port, merged.Port, parsed.Port, true},
		{"hostname", current.Hostname, merged.Hostname, parsed.Hostname, true},
		{"healthPath", current.HealthPath, merged.HealthPath, parsed.HealthPath, true},
		{"restartPolicy", current.RestartPolicy, merged.RestartPolicy, parsed.RestartPolicy, true},
		{"cpuLimit", current.CPULimit, merged.CPULimit, parsed.CPULimit, true},
		{"memoryLimit", current.MemoryLimit, merged.MemoryLimit, parsed.MemoryLimit, true},
		{"pidsLimit", current.PIDsLimit, merged.PIDsLimit, parsed.PIDsLimit, true},
		{"dependsOn", emptyAsNil(current.DependsOn), emptyAsNil(merged.DependsOn), nil, false},
		{"volumes", emptyAsNil(current.Volumes), emptyAsNil(merged.Volumes), nil, false},
	}
	for _, field := range fields {
		if !sameJSON(field.before, field.after) {
			change.Changed = append(change.Changed, field.name)
		}
		if field.operatorEditable && !sameJSON(field.after, field.fromFile) {
			change.KeptOverrides = append(change.KeptOverrides, field.name)
		}
	}
	keys := map[string]bool{}
	for key := range current.Environment {
		keys[key] = true
	}
	for key := range merged.Environment {
		keys[key] = true
	}
	envChanged := []string{}
	for key := range keys {
		before, hadBefore := current.Environment[key]
		after, hasAfter := merged.Environment[key]
		if hadBefore != hasAfter || before != after {
			envChanged = append(envChanged, "environment."+key)
		}
	}
	envKept := []string{}
	for key, fileValue := range parsed.Environment {
		if value, ok := merged.Environment[key]; ok && value != fileValue {
			envKept = append(envKept, "environment."+key)
		}
	}
	sort.Strings(envChanged)
	sort.Strings(envKept)
	change.Changed = append(change.Changed, envChanged...)
	change.KeptOverrides = append(change.KeptOverrides, envKept...)
	if len(change.Changed) > 0 {
		change.Action = "updated"
	}
	return change
}

func emptyAsNil(items []string) []string {
	if len(items) == 0 {
		return nil
	}
	return items
}

func sameJSON(a, b any) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}
