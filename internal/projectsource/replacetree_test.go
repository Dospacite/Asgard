package projectsource

import (
	"context"
	"os"
	"path/filepath"
	"testing"
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
	validation, err := ReplaceTree(context.Background(), database, project, "asgard.rousoftware.com", string(current), incoming, incomingRoot, func() error {
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

	for name, incoming := range map[string]string{
		"invalid": "services:\n  app:\n    image: nginx:1.27\n    networks: [default]\n",
		"removal": "services:\n  replacement:\n    image: nginx:1.27\n",
	} {
		t.Run(name, func(t *testing.T) {
			swapped := false
			if _, err := ReplaceTree(context.Background(), database, project, "asgard.rousoftware.com", original, incoming, incomingRoot, func() error {
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
