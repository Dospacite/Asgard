package projectsource

import (
	"context"
	"testing"

	"github.com/rousoftware/asgard/internal/composecfg"
)

func TestEditDotEnvChangesOnlyTheNamedVariables(t *testing.T) {
	original := "# deployment settings\nTTS_RPM=8\nGOOGLE='{\"private_key\":\"a\\nb\"}'\nOLD=gone\n"
	edited, err := EditDotEnv(original, map[string]string{"TTS_RPM": "100", "NEW": "two words"}, []string{"OLD"})
	if err != nil {
		t.Fatal(err)
	}
	want := "# deployment settings\nTTS_RPM=100\nGOOGLE='{\"private_key\":\"a\\nb\"}'\nNEW='two words'\n"
	if edited != want {
		t.Fatalf("edited =\n%s\nwant\n%s", edited, want)
	}
}

func TestQuoteEnvValueRoundTrips(t *testing.T) {
	for _, value := range []string{"", "plain", "has space", "it's", "a#b", "line\nbreak", `quote"inside`, "postgres://u:p@h/db"} {
		values, err := composecfg.ParseEnvFile([]byte("V=" + QuoteEnvValue(value) + "\n"))
		if err != nil {
			t.Fatalf("%q: %v", value, err)
		}
		if values["V"] != value {
			t.Errorf("%q read back as %q", value, values["V"])
		}
	}
}

func TestUpdateDotEnvAppliesToServices(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, root, ".env", "TTS_RPM=8\nSECRET='keep me'\n")
	project, database := createProjectIn(t, root, envFileCompose)
	if _, err := UpdateDotEnv(context.Background(), database, project, "asgard.example.com", map[string]string{"TTS_RPM": "100"}, nil); err != nil {
		t.Fatal(err)
	}
	project, _ = database.GetProject(context.Background(), project.ID)
	env := project.Services[0].Environment
	if env["TTS_RPM"] != "100" || env["SECRET"] != "keep me" {
		t.Fatalf("environment = %#v", env)
	}
}

func TestRedactDotEnvKeepsNames(t *testing.T) {
	got := RedactDotEnv("API_KEY=abc\nPORT=80\n", "[redacted]", func(name, _ string) bool { return name == "API_KEY" })
	if got != "API_KEY=[redacted]\nPORT=80\n" {
		t.Fatalf("got %q", got)
	}
}
