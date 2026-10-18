package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// The container configuration a Compose file declares has to survive both
// storage paths: the service an operator edits, and the release snapshot a
// rollback rebuilds from. If it only survived the first, a rollback would
// quietly run the container with different settings than the release it claims
// to restore.
func TestContainerConfigSurvivesServicesAndReleases(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "asgard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()

	entrypoint := []string{"/usr/local/bin/entrypoint.sh"}
	want := ContainerConfig{
		Entrypoint:         &entrypoint,
		WorkingDir:         "/srv",
		User:               "1000:1000",
		ContainerHostname:  "api-internal",
		Labels:             map[string]string{"com.example.tier": "backend"},
		Healthcheck:        &Healthcheck{Test: []string{"CMD-SHELL", "pg_isready"}, Interval: 10 * time.Second, Retries: 5, StartPeriod: 30 * time.Second, StartInterval: 2 * time.Second},
		ReadOnly:           true,
		ShmSize:            256 << 20,
		StopSignal:         "SIGQUIT",
		StopTimeoutSeconds: 30,
		Tmpfs:              map[string]string{"/tmp": "size=64m"},
		Sysctls:            map[string]string{"net.core.somaxconn": "1024"},
		Ulimits:            []Ulimit{{Name: "nofile", Soft: 20000, Hard: 40000}},
		Platform:           "linux/amd64",
		NetworkAliases:     []string{"api.internal"},
	}
	project := Project{ID: "project-1", Slug: "demo", Name: "Demo", SourceType: "image", SourcePath: "/tmp/demo", ComposePath: "compose.yaml", PrimaryService: "api"}
	service := Service{ID: "service-1", ProjectID: project.ID, Name: "api", Role: "web", Image: "example/api:1", Container: want, CPULimit: 1.5, MemoryLimit: 1 << 30, PIDsLimit: 512, RestartPolicy: "on-failure:5", Environment: map[string]string{"PORT": "3000"}}
	if err := database.CreateProject(ctx, project, []Service{service}); err != nil {
		t.Fatal(err)
	}

	stored, err := database.GetService(ctx, service.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertContainerConfig(t, "service", stored.Container, want)

	release, _, err := database.BeginRelease(ctx, project.ID, "operation-1", "manual", "commit", "services: {}", []Service{service})
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := database.ReleaseServices(ctx, release.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 1 {
		t.Fatalf("release services = %#v", snapshots)
	}
	assertContainerConfig(t, "release", snapshots[0].Container, want)
	if snapshots[0].RestartPolicy != "on-failure:5" || snapshots[0].MemoryLimit != 1<<30 {
		t.Fatalf("release snapshot = %+v", snapshots[0])
	}
}

func assertContainerConfig(t *testing.T, where string, got, want ContainerConfig) {
	t.Helper()
	if got.Entrypoint == nil || (*got.Entrypoint)[0] != (*want.Entrypoint)[0] {
		t.Fatalf("%s entrypoint = %v", where, got.Entrypoint)
	}
	if got.WorkingDir != want.WorkingDir || got.User != want.User || got.ContainerHostname != want.ContainerHostname {
		t.Fatalf("%s identity = %+v", where, got)
	}
	if got.Labels["com.example.tier"] != "backend" || got.Sysctls["net.core.somaxconn"] != "1024" || got.Tmpfs["/tmp"] != "size=64m" {
		t.Fatalf("%s maps = %+v", where, got)
	}
	if got.Healthcheck == nil || got.Healthcheck.Retries != 5 || got.Healthcheck.StartPeriod != 30*time.Second || got.Healthcheck.StartInterval != 2*time.Second {
		t.Fatalf("%s healthcheck = %+v", where, got.Healthcheck)
	}
	if !got.ReadOnly || got.ShmSize != 256<<20 || got.StopSignal != "SIGQUIT" || got.StopTimeoutSeconds != 30 || got.Platform != "linux/amd64" {
		t.Fatalf("%s switches = %+v", where, got)
	}
	if len(got.Ulimits) != 1 || got.Ulimits[0].Soft != 20000 || got.Ulimits[0].Hard != 40000 {
		t.Fatalf("%s ulimits = %+v", where, got.Ulimits)
	}
	if len(got.NetworkAliases) != 1 || got.NetworkAliases[0] != "api.internal" {
		t.Fatalf("%s aliases = %+v", where, got.NetworkAliases)
	}
}

// A deployment whose release has been trimmed keeps its history: the release
// row is removed when a project ages past the retention window and the foreign
// key nulls the reference, which used to make listing a project's deployments
// fail outright once any release had aged out.
func TestDeploymentsListSurvivesATrimmedRelease(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "asgard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()

	project := Project{ID: "project-1", Slug: "demo", Name: "Demo", SourceType: "image", SourcePath: "/tmp/demo", ComposePath: "compose.yaml", PrimaryService: "app"}
	service := Service{ID: "service-1", ProjectID: project.ID, Name: "app", Role: "web", Image: "example/app:1", CPULimit: 0.5, MemoryLimit: 1 << 20, PIDsLimit: 16, RestartPolicy: "unless-stopped"}
	if err := database.CreateProject(ctx, project, []Service{service}); err != nil {
		t.Fatal(err)
	}
	release, deployment, err := database.BeginRelease(ctx, project.ID, "operation-1", "manual", "", "services: {}", []Service{service})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.FinishRelease(ctx, release.ID, deployment.ID, "succeeded", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := database.DB.ExecContext(ctx, `DELETE FROM releases WHERE id=?`, release.ID); err != nil {
		t.Fatal(err)
	}
	items, err := database.ListDeployments(ctx, project.ID, 10)
	if err != nil {
		t.Fatalf("listing deployments after a release was trimmed: %v", err)
	}
	if len(items) != 1 || items[0].ReleaseID != "" || items[0].Status != "succeeded" {
		t.Fatalf("deployments = %#v", items)
	}
}
