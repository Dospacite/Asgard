package composecfg

import (
	"strings"
	"testing"
)

func TestValidateEnvironment(t *testing.T) {
	if err := ValidateEnvironment(map[string]string{"API_URL": "https://example.test", "_MODE": "preview"}); err != nil {
		t.Fatalf("valid environment rejected: %v", err)
	}
	if err := ValidateEnvironment(map[string]string{"INVALID-NAME": "value"}); err == nil {
		t.Fatal("invalid variable name accepted")
	}
	if err := ValidateEnvironment(map[string]string{"VALUE": "contains\x00nul"}); err == nil {
		t.Fatal("null byte accepted")
	}
	if err := ValidateEnvironment(map[string]string{"VALUE": strings.Repeat("x", MaxEnvironmentBytes)}); err == nil {
		t.Fatal("oversized environment accepted")
	}
}
