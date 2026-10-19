package mcpserver

import (
	"errors"
	"regexp"
	"strings"

	"github.com/rousoftware/asgard/internal/projectsource"
	"github.com/rousoftware/asgard/internal/store"
)

// RedactedValue replaces a secret-looking environment value in tool output.
const RedactedValue = "[redacted]"

// secretName matches environment names that conventionally hold credentials.
var secretName = regexp.MustCompile(`(?i)(PASS|SECRET|TOKEN|KEY|CREDENTIAL|PRIVATE|COOKIE|SALT|SIGNING|AUTH)`)

// urlPassword matches a URL with a password in its userinfo, such as a
// database URL, whatever the variable holding it is called.
var urlPassword = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://[^/@:\s]*:[^/@\s]+@`)

// isSecret reports whether an environment entry should be withheld.
func isSecret(name, value string) bool {
	if value == "" {
		return false
	}
	return secretName.MatchString(name) || urlPassword.MatchString(value)
}

// redactService withholds the secret-looking values of a service's
// environment. Tool results land in agent transcripts, which are stored and
// shared far more widely than the control plane's database, and an agent that
// needs to change one variable no longer has to read the others back: it can
// send environmentSet instead.
func redactService(svc store.Service) store.Service {
	if len(svc.Environment) == 0 {
		return svc
	}
	environment := make(map[string]string, len(svc.Environment))
	for name, value := range svc.Environment {
		if isSecret(name, value) {
			value = RedactedValue
		}
		environment[name] = value
	}
	svc.Environment = environment
	return svc
}

func redactServices(items []store.Service) []store.Service {
	out := make([]store.Service, len(items))
	for index, item := range items {
		out[index] = redactService(item)
	}
	return out
}

func redactProject(project store.Project) store.Project {
	project.Services = redactServices(project.Services)
	return project
}

// rejectRedacted refuses an update that would store the redaction marker,
// which is what copying a redacted service back into an update would do.
func rejectRedacted(environment map[string]string) error {
	names := []string{}
	for name, value := range environment {
		if value == RedactedValue {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	return errors.New("environment values for " + strings.Join(names, ", ") + " are the redaction marker, not real values; change single variables with environmentSet instead of sending the environment back, or read them with service_get revealSecrets")
}

func redactWorkspace(workspace projectsource.Workspace) projectsource.Workspace {
	values := make(map[string]string, len(workspace.DotEnv))
	for name, value := range workspace.DotEnv {
		if isSecret(name, value) {
			value = RedactedValue
		}
		values[name] = value
	}
	workspace.DotEnv = values
	files := make([]projectsource.File, len(workspace.Files))
	for index, file := range workspace.Files {
		if file.Kind == "dotenv" {
			file.Content = projectsource.RedactDotEnv(file.Content, RedactedValue, isSecret)
		}
		if file.Validation != nil {
			validation := *file.Validation
			validation.Services = redactServices(validation.Services)
			file.Validation = &validation
		}
		files[index] = file
	}
	workspace.Files = files
	return workspace
}
