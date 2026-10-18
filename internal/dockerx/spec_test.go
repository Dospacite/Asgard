package dockerx

import (
	"testing"
	"time"

	"github.com/rousoftware/asgard/internal/store"
)

func composeService() store.Service {
	entrypoint := []string{"/usr/local/bin/entrypoint.sh"}
	return store.Service{
		ID:            "service-1",
		Name:          "api",
		Role:          "web",
		Image:         "example/api:1",
		Command:       []string{"/bin/sh", "-c", "serve"},
		Environment:   map[string]string{"PORT": "3000"},
		Public:        true,
		Port:          3000,
		CPULimit:      1.5,
		MemoryLimit:   1 << 30,
		PIDsLimit:     512,
		RestartPolicy: "on-failure:5",
		Volumes:       []string{"asgard-demo-data:/var/lib/data", "@project/conf/app.ini:/etc/app.ini:ro"},
		Container: store.ContainerConfig{
			Entrypoint:         &entrypoint,
			WorkingDir:         "/srv",
			User:               "1000:1000",
			ContainerHostname:  "api-internal",
			Labels:             map[string]string{"com.example.tier": "backend", LabelManaged: "false", "com.rousoftware.asgard.project-id": "spoofed"},
			Healthcheck:        &store.Healthcheck{Test: []string{"CMD", "true"}, Interval: 10 * time.Second, Retries: 4, StartPeriod: 30 * time.Second},
			ReadOnly:           true,
			ShmSize:            256 << 20,
			StopSignal:         "SIGQUIT",
			StopTimeoutSeconds: 30,
			Tmpfs:              map[string]string{"/tmp": "size=64m"},
			Sysctls:            map[string]string{"net.core.somaxconn": "1024"},
			Ulimits:            []store.Ulimit{{Name: "nofile", Soft: 20000, Hard: 40000}},
			Platform:           "linux/amd64",
			NetworkAliases:     []string{"api.internal"},
		},
	}
}

func TestContainerSpecAppliesComposeContainerConfig(t *testing.T) {
	request := CreateRequest{
		Project:        store.Project{ID: "project-1", Slug: "demo"},
		Service:        composeService(),
		ReleaseID:      "release-1",
		Version:        7,
		Image:          "example/api:1",
		ProjectNetwork: "asgard-project-demo",
		EdgeNetwork:    "asgard-edge",
		DataVolume:     "asgard-data",
		SourceSubpath:  "projects/project-1/source",
	}
	config, hostConfig, endpoints, platform, err := containerSpec(request, "asgard-demo-api-r7")
	if err != nil {
		t.Fatal(err)
	}

	if config.Entrypoint[0] != "/usr/local/bin/entrypoint.sh" || (*request.Service.Container.Entrypoint)[0] != "/usr/local/bin/entrypoint.sh" {
		t.Fatalf("entrypoint = %v", config.Entrypoint)
	}
	if config.User != "1000:1000" || config.WorkingDir != "/srv" || config.Hostname != "api-internal" || config.StopSignal != "SIGQUIT" {
		t.Fatalf("container config = %+v", config)
	}
	if config.StopTimeout == nil || *config.StopTimeout != 30 {
		t.Fatalf("stop timeout = %v", config.StopTimeout)
	}
	if config.Healthcheck == nil || config.Healthcheck.Interval != 10*time.Second || config.Healthcheck.Retries != 4 || config.Healthcheck.StartPeriod != 30*time.Second {
		t.Fatalf("healthcheck = %+v", config.Healthcheck)
	}
	// A project's labels are kept, but never ones the control plane needs to
	// find and correlate its own containers.
	if config.Labels["com.example.tier"] != "backend" {
		t.Fatalf("project label lost: %v", config.Labels)
	}
	if config.Labels[LabelManaged] != "true" || config.Labels["com.rousoftware.asgard.project-id"] != "project-1" {
		t.Fatalf("reserved labels were overridden: %v", config.Labels)
	}
	if config.Labels["com.rousoftware.asgard.service"] != "api" || config.Labels["com.rousoftware.asgard.version"] != "7" {
		t.Fatalf("correlation labels = %v", config.Labels)
	}

	if hostConfig.Resources.Memory != 1<<30 || hostConfig.Resources.NanoCPUs != 1500000000 || hostConfig.Resources.PidsLimit == nil || *hostConfig.Resources.PidsLimit != 512 {
		t.Fatalf("limits = %+v", hostConfig.Resources)
	}
	if !hostConfig.ReadonlyRootfs || hostConfig.ShmSize != 256<<20 || hostConfig.Tmpfs["/tmp"] != "size=64m" || hostConfig.Sysctls["net.core.somaxconn"] != "1024" {
		t.Fatalf("host config = %+v", hostConfig)
	}
	if len(hostConfig.Resources.Ulimits) != 1 || hostConfig.Resources.Ulimits[0].Name != "nofile" || hostConfig.Resources.Ulimits[0].Soft != 20000 {
		t.Fatalf("ulimits = %+v", hostConfig.Resources.Ulimits)
	}
	if hostConfig.RestartPolicy.Name != "on-failure" || hostConfig.RestartPolicy.MaximumRetryCount != 5 {
		t.Fatalf("restart policy = %+v", hostConfig.RestartPolicy)
	}
	if platform == nil || platform.OS != "linux" || platform.Architecture != "amd64" {
		t.Fatalf("platform = %+v", platform)
	}

	// The platform's own boundaries are applied regardless of the file.
	if hostConfig.Init == nil || !*hostConfig.Init {
		t.Fatal("containers must run with an init process")
	}
	if len(hostConfig.CapDrop) != 1 || hostConfig.CapDrop[0] != "ALL" {
		t.Fatalf("capabilities not dropped: %v", hostConfig.CapDrop)
	}
	if len(hostConfig.SecurityOpt) != 1 || hostConfig.SecurityOpt[0] != "no-new-privileges:true" {
		t.Fatalf("security options = %v", hostConfig.SecurityOpt)
	}
	if hostConfig.LogConfig.Type != "local" || hostConfig.LogConfig.Config["max-size"] != "10m" {
		t.Fatalf("log config = %+v", hostConfig.LogConfig)
	}

	// The project network carries the service name — which is how one service
	// reaches another inside the project — plus whatever aliases the file asked
	// for. The edge network carries only the container name.
	project := endpoints["asgard-project-demo"]
	if project == nil {
		t.Fatalf("project network missing: %v", endpoints)
	}
	if len(project.Aliases) != 3 || project.Aliases[0] != "asgard-demo-api-r7" || project.Aliases[1] != "api" || project.Aliases[2] != "api.internal" {
		t.Fatalf("project aliases = %v", project.Aliases)
	}
	if len(endpoints["asgard-edge"].Aliases) != 1 {
		t.Fatalf("edge aliases = %v", endpoints["asgard-edge"].Aliases)
	}
}

func TestContainerSpecMountsProjectSourceAsSubpaths(t *testing.T) {
	request := CreateRequest{
		Project:        store.Project{ID: "project-1", Slug: "demo"},
		Service:        store.Service{ID: "service-1", Name: "api", Volumes: []string{"asgard-demo-data:/var/lib/data", "@project/conf/app.ini:/etc/app.ini:ro"}},
		ProjectNetwork: "asgard-project-demo",
		DataVolume:     "asgard-data",
		SourceSubpath:  "projects/project-1/source",
	}
	_, hostConfig, _, _, err := containerSpec(request, "asgard-demo-api-r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(hostConfig.Mounts) != 2 {
		t.Fatalf("mounts = %+v", hostConfig.Mounts)
	}
	if hostConfig.Mounts[0].Source != "asgard-demo-data" || hostConfig.Mounts[0].ReadOnly {
		t.Fatalf("named volume mount = %+v", hostConfig.Mounts[0])
	}
	project := hostConfig.Mounts[1]
	if project.Source != "asgard-data" || project.VolumeOptions == nil || project.VolumeOptions.Subpath != "projects/project-1/source/conf/app.ini" || !project.ReadOnly {
		t.Fatalf("project mount = %+v (options %+v)", project, project.VolumeOptions)
	}
}

// A service that exits zero is a job that finished; the same exit under a
// long-running policy is a release failure. Both readings come from the policy
// the file declared.
func TestRestartPolicySplitsIntoNameAndBudget(t *testing.T) {
	for _, tc := range []struct {
		policy  string
		name    string
		retries int
		oneShot bool
	}{
		{"unless-stopped", "unless-stopped", 0, false},
		{"always", "always", 0, false},
		{"no", "no", 0, true},
		{"on-failure", "on-failure", 0, true},
		{"on-failure:5", "on-failure", 5, true},
	} {
		service := store.Service{RestartPolicy: tc.policy}
		config, hostConfig, _, _, err := containerSpec(CreateRequest{Project: store.Project{Slug: "demo"}, Service: service, ProjectNetwork: "asgard-project-demo"}, "asgard-demo-app-r1")
		if err != nil {
			t.Fatal(err)
		}
		if config.Image != "" {
			t.Fatalf("image should be empty for %s", tc.policy)
		}
		if string(hostConfig.RestartPolicy.Name) != tc.name || hostConfig.RestartPolicy.MaximumRetryCount != tc.retries {
			t.Fatalf("%s → %+v", tc.policy, hostConfig.RestartPolicy)
		}
	}
}

// Only a daemon that cannot run the build on BuildKit sends it to the legacy
// builder. A build that failed on its own merits is reported as it is, not run
// a second time on another builder.
func TestNeedsLegacyBuilderRecognizesBuildKitRefusalsOnly(t *testing.T) {
	for _, message := range []string{
		`buildkit not supported by daemon`,
		`Error response from daemon: BuildKit is not enabled on this daemon`,
		`failed to solve: docker/dockerfile:1: no active sessions`,
	} {
		if !needsLegacyBuilder(message) {
			t.Fatalf("not recognized as a daemon without BuildKit: %q", message)
		}
	}
	for _, message := range []string{
		`failed to solve: process "/bin/sh -c npm ci" did not complete successfully: exit code: 1`,
		`failed to solve: failed to compute cache key: "/app/package.json": not found`,
		`pull access denied for example/base, repository does not exist`,
	} {
		if needsLegacyBuilder(message) {
			t.Fatalf("a failed build would be retried for no reason: %q", message)
		}
	}
}

// The spec hash is what lets a release keep a running container, so it must
// ignore what only names the release and change with anything the container
// would be created differently for.
func TestSpecHashIgnoresReleaseIdentityOnly(t *testing.T) {
	request := func() CreateRequest {
		return CreateRequest{Project: store.Project{ID: "project-1", Slug: "demo"}, Service: composeService(), ReleaseID: "release-1", Version: 7, Image: "asgard/demo/api:r7", ProjectNetwork: "asgard-project-demo", EdgeNetwork: "asgard-edge", DataVolume: "asgard-data", SourceSubpath: "projects/project-1/source"}
	}
	hash := func(req CreateRequest, imageID string) string {
		t.Helper()
		value, err := SpecHash(req, imageID)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	base := hash(request(), "sha256:aaa")

	next := request()
	next.ReleaseID, next.Version, next.Image, next.SpecHash = "release-2", 8, "asgard/demo/api:r8", base
	if hash(next, "sha256:aaa") != base {
		t.Fatal("a new release of the same image and configuration must hash the same")
	}
	if hash(request(), "sha256:bbb") == base {
		t.Fatal("a different image must change the hash")
	}
	changed := request()
	changed.Service.Environment = map[string]string{"PORT": "3001"}
	if hash(changed, "sha256:aaa") == base {
		t.Fatal("a changed environment must change the hash")
	}
	changed = request()
	changed.Service.Networks = []store.NetworkRef{{DockerName: "asgard-net-shared", Alias: "api"}}
	if hash(changed, "sha256:aaa") == base {
		t.Fatal("joining a shared network must change the hash")
	}
}

// The hash is stamped on the container it describes; the release identity
// labels still name the release that created it.
func TestContainerSpecStampsSpecHash(t *testing.T) {
	request := CreateRequest{Project: store.Project{ID: "project-1", Slug: "demo"}, Service: composeService(), ReleaseID: "release-1", Version: 7, Image: "example/api:1", ProjectNetwork: "asgard-project-demo", EdgeNetwork: "asgard-edge", DataVolume: "asgard-data", SourceSubpath: "projects/project-1/source", SpecHash: "v1:abc"}
	config, _, _, _, err := containerSpec(request, "asgard-demo-api-r7")
	if err != nil {
		t.Fatal(err)
	}
	if config.Labels[LabelSpec] != "v1:abc" || config.Labels["com.rousoftware.asgard.release-id"] != "release-1" {
		t.Fatalf("labels = %v", config.Labels)
	}
}
