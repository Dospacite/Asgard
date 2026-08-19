package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestNestedProjectReadsReleaseDatabaseConnection(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "asgard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	project := Project{
		ID:             "project-1",
		Slug:           "example",
		Name:           "Example",
		SourceType:     "image",
		SourcePath:     "/tmp/example",
		ComposePath:    "compose.yaml",
		PrimaryService: "app",
	}
	service := Service{
		ID:            "service-1",
		ProjectID:     project.ID,
		Name:          "app",
		Role:          "web",
		Image:         "traefik/whoami:v1.11.0",
		Port:          80,
		Hostname:      "example.asgard.example.com",
		HealthPath:    "/",
		CPULimit:      0.5,
		MemoryLimit:   512 << 20,
		PIDsLimit:     256,
		RestartPolicy: "unless-stopped",
	}
	if err := database.CreateProject(context.Background(), project, []Service{service}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	created, err := database.GetProject(ctx, project.ID)
	if err != nil {
		t.Fatalf("GetProject deadlocked or failed: %v", err)
	}
	if len(created.Services) != 1 || created.Services[0].ID != service.ID {
		t.Fatalf("GetProject services = %#v", created.Services)
	}
	projects, err := database.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects deadlocked or failed: %v", err)
	}
	if len(projects) != 1 || len(projects[0].Services) != 1 {
		t.Fatalf("ListProjects returned %#v", projects)
	}
}
