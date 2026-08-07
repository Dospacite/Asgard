package composecfg

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	MaxEnvironmentVariables = 512
	MaxEnvironmentBytes     = 256 << 10
)

var environmentVariablePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func ValidateEnvironment(environment map[string]string) error {
	if len(environment) > MaxEnvironmentVariables {
		return fmt.Errorf("environment can contain at most %d variables", MaxEnvironmentVariables)
	}
	total := 0
	for name, value := range environment {
		if !environmentVariablePattern.MatchString(name) {
			return fmt.Errorf("environment variable %q must start with a letter or underscore and contain only letters, numbers, and underscores", name)
		}
		if strings.ContainsRune(value, 0) {
			return fmt.Errorf("environment variable %q contains a null byte", name)
		}
		total += len(name) + len(value) + 1
		if total > MaxEnvironmentBytes {
			return fmt.Errorf("environment values exceed %d bytes", MaxEnvironmentBytes)
		}
	}
	return nil
}
