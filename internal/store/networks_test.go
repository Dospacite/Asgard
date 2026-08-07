package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestManagedNetworkMembershipsSpanProjectsAndCascade(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "asgard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()

	first := Project{ID: "project-a", Slug: "project-a", Name: "Project A", SourceType: "image", SourcePath: "/tmp/a", ComposePath: "compose.yaml", PrimaryService: "api"}
	second := Project{ID: "project-b", Slug: "project-b", Name: "Project B", SourceType: "image", SourcePath: "/tmp/b", ComposePath: "compose.yaml", PrimaryService: "worker"}
	api := Service{ID: "service-a", ProjectID: first.ID, Name: "api", Role: "web", Image: "example/api", HealthPath: "/", CPULimit: .5, MemoryLimit: 128 << 20, PIDsLimit: 128, RestartPolicy: "unless-stopped"}
	worker := Service{ID: "service-b", ProjectID: second.ID, Name: "worker", Role: "worker", Image: "example/worker", HealthPath: "/", CPULimit: .5, MemoryLimit: 128 << 20, PIDsLimit: 128, RestartPolicy: "unless-stopped"}
	if err := database.CreateProject(ctx, first, []Service{api}); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateProject(ctx, second, []Service{worker}); err != nil {
		t.Fatal(err)
	}

	network := ManagedNetwork{ID: "network-1", Slug: "application-mesh", Name: "Application mesh", DockerName: "asgard-shared-application-mesh", Description: "Cross-project APIs", Driver: "bridge", Internal: true}
	if err := database.CreateManagedNetwork(ctx, network); err != nil {
		t.Fatal(err)
	}
	if err := database.AddNetworkMember(ctx, network.ID, first.ID, api.ID, "project-a--api"); err != nil {
		t.Fatal(err)
	}
	if err := database.AddNetworkMember(ctx, network.ID, second.ID, worker.ID, "project-b--worker"); err != nil {
		t.Fatal(err)
	}

	created, err := database.GetManagedNetwork(ctx, network.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if !created.Internal || len(created.Members) != 2 {
		t.Fatalf("network = %#v", created)
	}
	refs, err := database.ListServiceNetworks(ctx, api.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Alias != "project-a--api" || refs[0].DockerName != network.DockerName {
		t.Fatalf("service network refs = %#v", refs)
	}
	if err := database.AddNetworkMember(ctx, network.ID, second.ID, worker.ID, "another-alias"); err == nil {
		t.Fatal("expected duplicate service membership to fail")
	}
	if err := database.DeleteManagedNetwork(ctx, network.ID); err == nil {
		t.Fatal("expected non-empty network deletion to fail")
	}

	if _, err := database.DB.ExecContext(ctx, `DELETE FROM projects WHERE id=?`, first.ID); err != nil {
		t.Fatal(err)
	}
	members, err := database.ListNetworkMembers(ctx, network.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0].ServiceID != worker.ID {
		t.Fatalf("members after project cascade = %#v", members)
	}
	if err := database.RemoveNetworkMember(ctx, network.ID, worker.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.DeleteManagedNetwork(ctx, network.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.GetManagedNetwork(ctx, network.ID); !IsNotFound(err) {
		t.Fatalf("expected deleted network to be missing, got %v", err)
	}
}
