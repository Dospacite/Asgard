package projectsource

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rousoftware/asgard/internal/store"
)

func TestReplaceTreeAppliesIncomingComposeAndKeepsRuntimeOverrides(t *testing.T) {
	project, database := createProject(t, `services:
  app:
    image: nginx:1.26
    environment:
      FROM_COMPOSE: old
x-asgard:
  primary-service: app
  services:
    app:
      role: web
      public: true
      port: 80
`)
	service := project.Services[0]
	service.Environment["ADMIN_PASSWORD"] = "operator-secret"
	service.CPULimit = 2
	if err := database.UpdateService(context.Background(), service, service.ConfigRevision); err != nil {
		t.Fatal(err)
	}
	project, _ = database.GetProject(context.Background(), project.ID)

	current, err := os.ReadFile(filepath.Join(project.SourcePath, project.ComposePath))
	if err != nil {
		t.Fatal(err)
	}
	incomingRoot := t.TempDir()
	incoming := `services:
  app:
    image: nginx:1.27
    environment:
      FROM_COMPOSE: new
  worker:
    image: busybox:1.36
x-asgard:
  primary-service: app
  services:
    app:
      role: web
      public: true
      port: 80
    worker:
      role: worker
      public: false
`
	if err := os.WriteFile(filepath.Join(incomingRoot, "compose.yaml"), []byte(incoming), 0o640); err != nil {
		t.Fatal(err)
	}
	swapped := false
	validation, _, err := ReplaceTree(context.Background(), database, project, "asgard.example.com", string(current), incoming, incomingRoot, func() error {
		swapped = true
		return os.WriteFile(filepath.Join(project.SourcePath, project.ComposePath), []byte(incoming), 0o640)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !validation.Valid || !swapped {
		t.Fatalf("validation=%#v swapped=%t", validation, swapped)
	}

	project, err = database.GetProject(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(project.Services) != 2 {
		t.Fatalf("services = %#v", project.Services)
	}
	app := serviceNamed(t, project.Services, "app")
	if app.ID != service.ID {
		t.Fatalf("service identity changed: %s != %s", app.ID, service.ID)
	}
	if app.Image != "nginx:1.27" || app.Environment["FROM_COMPOSE"] != "new" {
		t.Fatalf("incoming source fields not applied: %#v", app)
	}
	if app.Environment["ADMIN_PASSWORD"] != "operator-secret" {
		t.Fatalf("operator environment lost: %#v", app.Environment)
	}
	if app.CPULimit != 2 {
		t.Fatalf("runtime limit lost: %v", app.CPULimit)
	}
	if serviceNamed(t, project.Services, "worker").Role != "worker" {
		t.Fatal("service added by the incoming tree was not created")
	}
}

func TestReplaceTreeLeavesLiveTreeAloneWhenIncomingIsUnusable(t *testing.T) {
	original := `services:
  app:
    image: nginx:1.27
`
	project, database := createProject(t, original)
	incomingRoot := t.TempDir()

	// "invalid" is a file Asgard refuses — here a privileged service — and
	// "removal" is a valid file that would drop a service the project already
	// runs. Neither may reach the live tree.
	for name, incoming := range map[string]string{
		"invalid": "services:\n  app:\n    image: nginx:1.27\n    privileged: true\n",
		"removal": "services:\n  replacement:\n    image: nginx:1.27\n",
	} {
		t.Run(name, func(t *testing.T) {
			swapped := false
			if _, _, err := ReplaceTree(context.Background(), database, project, "asgard.example.com", original, incoming, incomingRoot, func() error {
				swapped = true
				return nil
			}); err == nil {
				t.Fatal("expected the re-synced tree to be rejected")
			}
			if swapped {
				t.Fatal("live tree was replaced despite the rejection")
			}
			data, err := os.ReadFile(filepath.Join(project.SourcePath, project.ComposePath))
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != original {
				t.Fatalf("live Compose file changed to %q", data)
			}
		})
	}
}

// writeProjectFile replaces one file in a project's live source tree.
func writeProjectFile(t *testing.T, root, relative, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, relative), []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
}

const envFileCompose = `services:
  app:
    image: nginx:1.27
    env_file: .env
`

// A .env edited on the host before a re-sync is already on disk when the
// re-sync reads the "previous" Compose file, so comparing against a fresh parse
// made the stored value look like an operator override and kept it forever.
// The baseline has to be what the file produced when it was last applied.
func TestReplaceTreeAppliesDotEnvValueChangedBeforeResync(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, root, ".env", "TTS_RPM=8\nKEEP=same\n")
	project, database := createProjectIn(t, root, envFileCompose)
	if got := project.Services[0].Environment["TTS_RPM"]; got != "8" {
		t.Fatalf("fixture TTS_RPM = %q", got)
	}

	writeProjectFile(t, project.SourcePath, ".env", "TTS_RPM=100\nKEEP=same\n")
	incomingRoot := t.TempDir()
	writeProjectFile(t, incomingRoot, "compose.yaml", envFileCompose)
	writeProjectFile(t, incomingRoot, ".env", "TTS_RPM=100\nKEEP=same\n")

	report, err := replaceTree(t, database, project, envFileCompose, incomingRoot)
	if err != nil {
		t.Fatal(err)
	}
	project, _ = database.GetProject(context.Background(), project.ID)
	if got := project.Services[0].Environment["TTS_RPM"]; got != "100" {
		t.Fatalf("TTS_RPM = %q, want the .env value 100", got)
	}
	change := report.Service("app")
	if change.Action != "updated" || !contains(change.Changed, "environment.TTS_RPM") {
		t.Fatalf("report = %#v", change)
	}
}

// An override set through Asgard still wins over the file, and the re-sync says
// so instead of presenting the file's value as if it had been applied.
func TestReplaceTreeReportsOverridesItKeeps(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, root, ".env", "TTS_RPM=8\n")
	project, database := createProjectIn(t, root, envFileCompose)
	service := project.Services[0]
	service.Environment["TTS_RPM"] = "50"
	if err := database.UpdateService(context.Background(), service, service.ConfigRevision); err != nil {
		t.Fatal(err)
	}
	project, _ = database.GetProject(context.Background(), project.ID)

	incomingRoot := t.TempDir()
	writeProjectFile(t, incomingRoot, "compose.yaml", envFileCompose)
	writeProjectFile(t, incomingRoot, ".env", "TTS_RPM=100\n")
	report, err := replaceTree(t, database, project, envFileCompose, incomingRoot)
	if err != nil {
		t.Fatal(err)
	}
	project, _ = database.GetProject(context.Background(), project.ID)
	if got := project.Services[0].Environment["TTS_RPM"]; got != "50" {
		t.Fatalf("TTS_RPM = %q, want the operator's 50", got)
	}
	change := report.Service("app")
	if change.Action != "unchanged" || !contains(change.KeptOverrides, "environment.TTS_RPM") {
		t.Fatalf("report = %#v", change)
	}
}

func replaceTree(t *testing.T, database *store.Store, project store.Project, current, incomingRoot string) (ReconcileReport, error) {
	t.Helper()
	incoming, err := os.ReadFile(filepath.Join(incomingRoot, "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	_, report, err := ReplaceTree(context.Background(), database, project, "asgard.example.com", current, string(incoming), incomingRoot, func() error {
		return nil
	})
	return report, err
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
