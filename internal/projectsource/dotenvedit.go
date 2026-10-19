package projectsource

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/rousoftware/asgard/internal/composecfg"
	"github.com/rousoftware/asgard/internal/store"
)

var plainEnvValue = regexp.MustCompile(`^[A-Za-z0-9_./:@%+,=-]*$`)

// EditDotEnv sets and removes variables in .env content and leaves every other
// line — comments, ordering, quoting — exactly as it was. A variable that is
// set but not yet present is appended.
func EditDotEnv(content string, set map[string]string, unset []string) (string, error) {
	removing := map[string]bool{}
	for _, name := range unset {
		removing[name] = true
	}
	for name := range set {
		if !composecfg.ValidEnvironmentName(name) {
			return "", errors.New(name + " is not a valid variable name")
		}
		if removing[name] {
			return "", errors.New(name + " cannot be both set and removed")
		}
	}
	lines := strings.SplitAfter(content, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	written := map[string]bool{}
	out := make([]string, 0, len(lines)+len(set))
	for _, line := range lines {
		key, _, ok, err := composecfg.ParseEnvLine(strings.TrimSuffix(line, "\n"))
		if err != nil || !ok {
			out = append(out, line)
			continue
		}
		if removing[key] {
			continue
		}
		if value, replace := set[key]; replace {
			if written[key] {
				// A later duplicate would override the line just written.
				continue
			}
			out = append(out, key+"="+QuoteEnvValue(value)+"\n")
			written[key] = true
			continue
		}
		out = append(out, line)
	}
	if len(out) > 0 && !strings.HasSuffix(out[len(out)-1], "\n") {
		out[len(out)-1] += "\n"
	}
	added := make([]string, 0, len(set))
	for name := range set {
		if !written[name] {
			added = append(added, name)
		}
	}
	sort.Strings(added)
	for _, name := range added {
		out = append(out, name+"="+QuoteEnvValue(set[name])+"\n")
	}
	return strings.Join(out, ""), nil
}

// QuoteEnvValue writes a value so the env-file parser reads it back unchanged.
func QuoteEnvValue(value string) string {
	if plainEnvValue.MatchString(value) {
		return value
	}
	if !strings.ContainsAny(value, "'\n\r") {
		return "'" + value + "'"
	}
	return strconv.Quote(value)
}

// RedactDotEnv replaces the values secret reports as sensitive with marker,
// keeping each line's name so the file's shape stays readable.
func RedactDotEnv(content, marker string, secret func(name, value string) bool) string {
	lines := strings.SplitAfter(content, "\n")
	for index, line := range lines {
		key, value, ok, err := composecfg.ParseEnvLine(strings.TrimSuffix(line, "\n"))
		if err != nil || !ok || !secret(key, value) {
			continue
		}
		newline := ""
		if strings.HasSuffix(line, "\n") {
			newline = "\n"
		}
		lines[index] = key + "=" + marker + newline
	}
	return strings.Join(lines, "")
}

// UpdateDotEnv changes single variables in the project's .env and applies the
// result to its services, as a full save through the editor would.
func UpdateDotEnv(ctx context.Context, database *store.Store, project store.Project, domain string, set map[string]string, unset []string) (Workspace, error) {
	if len(set) == 0 && len(unset) == 0 {
		return Workspace{}, &Problem{Code: "dotenv_no_change", Message: "Name at least one variable to set or remove."}
	}
	specs, err := allowedFiles(project)
	if err != nil {
		return Workspace{}, err
	}
	for _, spec := range specs {
		if spec.Kind != "dotenv" {
			continue
		}
		current, err := readFile(project.SourcePath, spec)
		if err != nil {
			return Workspace{}, err
		}
		content, err := EditDotEnv(current.Content, set, unset)
		if err != nil {
			return Workspace{}, &Problem{Code: "dotenv_invalid", Message: err.Error()}
		}
		return Save(ctx, database, project, domain, spec.Path, content, current.Revision)
	}
	return Workspace{}, &Problem{Code: "source_file_not_allowed", Message: "This project has no editable .env file."}
}
