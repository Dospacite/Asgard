package importer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rousoftware/asgard/internal/composecfg"
	"github.com/rousoftware/asgard/internal/store"
)

func TestResyncRefusesProjectsWithoutARepository(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "asgard.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	root := t.TempDir()
	compose := "services:\n  app:\n    image: nginx:1.27\n"
	if err := os.WriteFile(filepath.Join(root, "compose.yaml"), []byte(compose), 0o640); err != nil {
		t.Fatal(err)
	}
	project := store.Project{ID: "project-1", Slug: "uploaded", Name: "Uploaded", SourceType: "zip", SourcePath: root, ComposePath: "compose.yaml"}
	_, validation := composecfg.Parse([]byte(compose), project.ID, project.Slug, root)
	if err := database.CreateProject(context.Background(), project, validation.Services); err != nil {
		t.Fatal(err)
	}

	if _, err := (&Importer{Store: database}).Resync(context.Background(), project.ID); !errors.Is(err, ErrNotGitSource) {
		t.Fatalf("resync error = %v", err)
	}
}

func TestIsGitSource(t *testing.T) {
	for sourceType, want := range map[string]bool{"git": true, "git-private": true, "zip": false, "image": false, "tar.gz": false} {
		if got := IsGitSource(sourceType); got != want {
			t.Fatalf("IsGitSource(%q) = %t", sourceType, got)
		}
	}
}

// A repository almost never commits the .env Asgard writes deployment secrets
// into, so a re-sync that took the clone's view would empty every interpolated
// Compose value the moment it landed.
func TestCarryDotEnvKeepsTheLiveSecretsOverTheClone(t *testing.T) {
	live, staging := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(live, ".env"), []byte("ADMIN_PASSWORD=operator-secret\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, ".env"), []byte("ADMIN_PASSWORD=example\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	project := store.Project{SourcePath: live, ComposePath: "compose.yaml"}

	carried, err := carryDotEnv(project, staging)
	if err != nil || !carried {
		t.Fatalf("carryDotEnv = %t, %v", carried, err)
	}
	data, err := os.ReadFile(filepath.Join(staging, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "ADMIN_PASSWORD=operator-secret\n" {
		t.Fatalf("staged .env = %q", data)
	}
}

func TestCarryDotEnvReportsNothingToKeep(t *testing.T) {
	live, staging := t.TempDir(), t.TempDir()
	project := store.Project{SourcePath: live, ComposePath: "deploy/compose.yaml"}
	carried, err := carryDotEnv(project, staging)
	if err != nil || carried {
		t.Fatalf("carryDotEnv = %t, %v", carried, err)
	}
}

func TestDotEnvPathsCoverTheEditorAndInterpolationLocations(t *testing.T) {
	got := dotEnvPaths("deploy/compose.yaml")
	want := []string{filepath.Join("deploy", ".env"), ".env"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("dotEnvPaths = %#v", got)
	}
}
