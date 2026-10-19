package projectsource

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rousoftware/asgard/internal/composecfg"
	"github.com/rousoftware/asgard/internal/store"
)

func TestLoadReturnsEditableFilesAndDotEnvValues(t *testing.T) {
	project, database := createProject(t, `services:
  app:
    build: .
`)
	if err := os.WriteFile(filepath.Join(project.SourcePath, "Dockerfile"), []byte("FROM scratch\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project.SourcePath, ".env"), []byte("PUBLIC=value\nQUOTED=\"two words\"\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	workspace, err := Load(project)
	if err != nil {
		t.Fatal(err)
	}
	if len(workspace.Files) != 3 {
		t.Fatalf("files = %#v", workspace.Files)
	}
	if workspace.DotEnv["PUBLIC"] != "value" || workspace.DotEnv["QUOTED"] != "two words" {
		t.Fatalf("dotenv = %#v", workspace.DotEnv)
	}
	for _, file := range workspace.Files {
		if file.Revision == "" {
			t.Fatalf("missing revision for %s", file.Path)
		}
	}
	_ = database
}

func TestSaveComposeUpdatesSourceFieldsAndPreservesRuntimeOverrides(t *testing.T) {
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
	service.Role = "stateful"
	service.Environment["CUSTOM"] = "keep"
	if err := database.UpdateService(context.Background(), service, service.ConfigRevision); err != nil {
		t.Fatal(err)
	}
	project, _ = database.GetProject(context.Background(), project.ID)
	workspace, err := Load(project)
	if err != nil {
		t.Fatal(err)
	}
	compose := workspace.Files[0]
	updated := `services:
  app:
    image: nginx:1.27
    environment:
      FROM_COMPOSE: new
  worker:
    image: busybox:1.36
    command: ["sleep", "3600"]
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
	if _, err := Save(context.Background(), database, project, "asgard.example.com", compose.Path, updated, compose.Revision); err != nil {
		t.Fatal(err)
	}

	project, err = database.GetProject(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(project.Services) != 2 {
		t.Fatalf("services = %#v", project.Services)
	}
	app := serviceNamed(t, project.Services, "app")
	if app.ID != service.ID || app.Image != "nginx:1.27" {
		t.Fatalf("app identity/source = %#v", app)
	}
	if app.Role != "stateful" {
		t.Fatalf("runtime role override was replaced: %s", app.Role)
	}
	if app.Environment["FROM_COMPOSE"] != "new" || app.Environment["CUSTOM"] != "keep" {
		t.Fatalf("merged environment = %#v", app.Environment)
	}
	worker := serviceNamed(t, project.Services, "worker")
	if worker.Role != "worker" {
		t.Fatalf("worker = %#v", worker)
	}
}

func TestSaveRejectsStaleRevisionAndServiceRemoval(t *testing.T) {
	project, database := createProject(t, `services:
  app:
    image: nginx:1.27
`)
	workspace, err := Load(project)
	if err != nil {
		t.Fatal(err)
	}
	compose := workspace.Files[0]
	if _, err := Save(context.Background(), database, project, "asgard.example.com", compose.Path, compose.Content, "stale"); problemCode(err) != "source_revision_conflict" {
		t.Fatalf("stale save error = %v", err)
	}
	removal := `services:
  replacement:
    image: nginx:1.27
`
	if _, err := Save(context.Background(), database, project, "asgard.example.com", compose.Path, removal, compose.Revision); problemCode(err) != "service_removal_blocked" {
		t.Fatalf("removal save error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(project.SourcePath, project.ComposePath))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != compose.Content {
		t.Fatalf("blocked save changed file to %q", data)
	}
}

func TestParseDotEnvReportsInvalidLines(t *testing.T) {
	values, issues := ParseDotEnv("export VALID='hello world'\nINVALID-NAME=value\nBROKEN=\"unterminated\n")
	if values["VALID"] != "hello world" {
		t.Fatalf("values = %#v", values)
	}
	if len(issues) != 2 {
		t.Fatalf("issues = %#v", issues)
	}
}

func createProject(t *testing.T, compose string) (store.Project, *store.Store) {
	t.Helper()
	return createProjectIn(t, t.TempDir(), compose)
}

// createProjectIn imports compose from root, so a test can place files such as
// a .env beside it first.
func createProjectIn(t *testing.T, root, compose string) (store.Project, *store.Store) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "compose.yaml"), []byte(compose), 0o640); err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(filepath.Join(t.TempDir(), "asgard.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	project := store.Project{ID: "project-1", Slug: "example", Name: "Example", SourceType: "git", SourcePath: root, ComposePath: "compose.yaml", PrimaryService: "app"}
	_, validation := composecfg.Parse([]byte(compose), project.ID, project.Slug, composecfg.Source{Root: root, Path: "compose.yaml"})
	if !validation.Valid {
		t.Fatalf("fixture invalid: %#v", validation.Errors)
	}
	project.PrimaryService = validation.PrimaryService
	if err := database.CreateProject(context.Background(), project, validation.Services); err != nil {
		t.Fatal(err)
	}
	project, err = database.GetProject(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	return project, database
}

func serviceNamed(t *testing.T, services []store.Service, name string) store.Service {
	t.Helper()
	for _, service := range services {
		if service.Name == name {
			return service
		}
	}
	t.Fatalf("service %s not found", name)
	return store.Service{}
}

func problemCode(err error) string {
	var problem *Problem
	if errors.As(err, &problem) {
		return problem.Code
	}
	return ""
}

func TestSaveDotEnvAppliesChangedValuesToServices(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("TTS_RPM=8\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	project, database := createProjectIn(t, root, "services:\n  app:\n    image: nginx:1.27\n    env_file: .env\n")
	workspace, err := Load(project)
	if err != nil {
		t.Fatal(err)
	}
	var dotenv File
	for _, file := range workspace.Files {
		if file.Kind == "dotenv" {
			dotenv = file
		}
	}
	if _, err := Save(context.Background(), database, project, "asgard.example.com", dotenv.Path, "TTS_RPM=100\n", dotenv.Revision); err != nil {
		t.Fatal(err)
	}
	project, _ = database.GetProject(context.Background(), project.ID)
	if got := project.Services[0].Environment["TTS_RPM"]; got != "100" {
		t.Fatalf("TTS_RPM = %q after saving .env, want 100", got)
	}
}
