package composecfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rousoftware/asgard/internal/store"
)

// This file is the compatibility corpus: the shapes real Compose files arrive
// in, parsed the way an import parses them, with the outcome each one must have.
//
// Every fixture below is either taken from a project that had to be edited
// before Asgard would take it, or is the pattern that made it need editing —
// a container_name, an external network, a profile, a merge-key base, a
// healthcheck, a job that exits. The assertions are on behaviour: what the
// platform accepted, what it warned about, and what the service it stored says.

// parseFixture writes a project tree and parses its Compose file.
func parseFixture(t *testing.T, composePath string, files map[string]string) ValidationResult {
	t.Helper()
	root := writeProject(t, files)
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(composePath)))
	if err != nil {
		t.Fatalf("read fixture compose file: %v", err)
	}
	_, result := Parse(data, "project-id", "demo", Source{Root: root, Path: composePath})
	return result
}

func warningPaths(result ValidationResult) []string {
	out := make([]string, 0, len(result.Warnings))
	for _, warning := range result.Warnings {
		out = append(out, warning.Path)
	}
	return out
}

func errorPaths(result ValidationResult) []string {
	out := make([]string, 0, len(result.Errors))
	for _, item := range result.Errors {
		out = append(out, item.Path)
	}
	return out
}

func requireValid(t *testing.T, result ValidationResult) {
	t.Helper()
	if !result.Valid {
		t.Fatalf("expected a valid import, got errors %+v", result.Errors)
	}
}

func requireWarned(t *testing.T, result ValidationResult, path string) string {
	t.Helper()
	for _, warning := range result.Warnings {
		if warning.Path == path {
			return warning.Message
		}
	}
	t.Fatalf("expected a warning at %s, warnings were %+v", path, result.Warnings)
	return ""
}

func requireRejected(t *testing.T, result ValidationResult, path string) string {
	t.Helper()
	if result.Valid {
		t.Fatal("expected the import to be refused")
	}
	for _, item := range result.Errors {
		if item.Path == path {
			return item.Message
		}
	}
	t.Fatalf("expected an error at %s, errors were %+v", path, result.Errors)
	return ""
}

func service(t *testing.T, result ValidationResult, name string) store.Service {
	t.Helper()
	return result.Services[serviceNamed(t, result, name)]
}

// TestImportsAProjectThatNeededRewriting is the suoyunculari case: a Compose
// file that names its containers, attaches an existing external network, and
// mounts a volume that already exists on the host. Asgard used to refuse all
// three outright, so the project had to be forked into a second Compose file
// that only Asgard could run.
func TestImportsAProjectThatNeededRewriting(t *testing.T) {
	result := parseFixture(t, "docker-compose.yml", map[string]string{
		".env": "POSTGRES_PASSWORD=secret\nPUBLIC_DIRECTUS_URL=https://cms.example.com\n",
		"docker-compose.yml": `version: '3.9'
services:
  postgres:
    image: postgres:16-alpine
    container_name: suoyunculari-postgres
    restart: unless-stopped
    environment:
      POSTGRES_DB: ${POSTGRES_DB:-suoyunculari}
      POSTGRES_USER: ${POSTGRES_USER:-suo}
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U ${POSTGRES_USER:-suo} -d ${POSTGRES_DB:-suoyunculari}"]
      interval: 5s
      timeout: 5s
      retries: 10
    ports:
      - "5432:5432"
    volumes:
      - postgres_data:/var/lib/postgresql/data

  app:
    container_name: suoyunculari-app
    restart: unless-stopped
    build: .
    depends_on:
      postgres:
        condition: service_healthy
    environment:
      PORT: 4321
      DATABASE_URL: postgres://${POSTGRES_USER:-suo}:${POSTGRES_PASSWORD}@postgres:5432/${POSTGRES_DB:-suoyunculari}
    ports:
      - "4321:4321"
    volumes:
      - pingo_drive_files:/var/lib/pingo-drive-files
      - ./config/app.json:/app/config/app.json:ro
    networks:
      - default
      - web

volumes:
  postgres_data:
  pingo_drive_files:
    external: true
    name: ${PINGO_DRIVE_VOLUME_NAME:-suoyunculari-yk_pingo_drive_files}

networks:
  web:
    external: true
`,
		"config/app.json": "{}",
	})

	requireValid(t, result)
	if got := service(t, result, "app").Environment["DATABASE_URL"]; got != "postgres://suo:secret@postgres:5432/suoyunculari" {
		t.Fatalf("interpolated DATABASE_URL = %q", got)
	}
	requireWarned(t, result, "services.app.container_name")
	requireWarned(t, result, "services.postgres.container_name")
	requireWarned(t, result, "volumes.pingo_drive_files")
	requireWarned(t, result, "services.app.networks.web")
	// The project's own network is the one Asgard creates, so naming it is not
	// worth a warning.
	for _, warning := range result.Warnings {
		if warning.Path == "services.app.networks.default" {
			t.Fatalf("the default network is the project network: %+v", warning)
		}
	}
	volumes := service(t, result, "app").Volumes
	want := []string{"asgard-demo-pingo-drive-files:/var/lib/pingo-drive-files", "@project/config/app.json:/app/config/app.json:ro"}
	if len(volumes) != 2 || volumes[0] != want[0] || volumes[1] != want[1] {
		t.Fatalf("volumes = %v, want %v", volumes, want)
	}
	if health := service(t, result, "postgres").Container.Healthcheck; health == nil || health.Test[1] != "pg_isready -U suo -d suoyunculari" {
		t.Fatalf("healthcheck = %+v", health)
	}
}

// TestImportsAFileWithAnchorsAndProfiles is the rouwriteups case: one YAML base
// merged into every service, a profile, a one-shot job, and a health path per
// service supplied through x-asgard.
func TestImportsAFileWithAnchorsAndProfiles(t *testing.T) {
	result := parseFixture(t, "compose.yaml", map[string]string{
		"compose.yaml": `name: rouwriteups

x-logging: &default-logging
  driver: json-file
  options:
    max-size: "10m"

x-base: &base
  restart: unless-stopped
  logging: *default-logging
  cap_drop: [ALL]
  security_opt: ["no-new-privileges:true"]

services:
  db:
    <<: *base
    image: postgres:18.1-alpine
    environment:
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD:?set POSTGRES_PASSWORD}
    volumes:
      - db-data:/var/lib/postgresql
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U directus"]
      interval: 5s
      retries: 20

  garage:
    <<: *base
    image: dxflrs/garage:v2.1.0
    volumes:
      - ./garage/garage.toml:/etc/garage.toml:ro

  garage-init:
    <<: *base
    restart: "no"
    image: node:22.22-alpine
    depends_on:
      - garage
    volumes:
      - ./garage/init.mjs:/init/init.mjs:ro
    command: ["node", "/init/init.mjs"]

  web:
    <<: *base
    profiles: ["app"]
    build:
      context: ./web
    depends_on:
      - garage-init

volumes:
  db-data:

x-asgard:
  primary-service: web
  services:
    web:
      role: web
      public: true
      port: 3000
      health-path: /api/health
    db:
      role: stateful
    garage:
      role: stateful
    garage-init:
      role: worker
`,
		"garage/garage.toml": "[admin]\n",
		"garage/init.mjs":    "console.log('init')\n",
		"web/Dockerfile":     "FROM node:22-alpine\n",
		".env":               "POSTGRES_PASSWORD=secret\n",
	})

	requireValid(t, result)
	// Every service merged the base, so every one of them carries the warning —
	// the logging block is understood but has no Asgard equivalent.
	requireWarned(t, result, "services.db.logging")
	requireWarned(t, result, "services.garage.logging")
	requireWarned(t, result, "services.web.profiles")
	// cap_drop ALL and no-new-privileges ask for what Asgard already does.
	for _, item := range result.Errors {
		if strings.Contains(item.Message, "capabilities") || strings.Contains(item.Message, "no-new-privileges") {
			t.Fatalf("hardening that matches the platform was refused: %+v", item)
		}
	}
	if got := service(t, result, "garage").Container.Labels; len(got) != 0 {
		t.Fatalf("unexpected labels %v", got)
	}
	if got := service(t, result, "garage-init").RestartPolicy; got != "no" {
		t.Fatalf("one-shot restart policy = %q", got)
	}
	if got := service(t, result, "web").BuildContext; got != "web" {
		t.Fatalf("build context = %q, want web", got)
	}
}

// TestImportsAnExtendedContainerDefinition covers the fields a modern Compose
// file uses that Asgard previously refused as unknown: entrypoint, user, the
// working directory, labels, the resource limits of both spellings, tmpfs,
// sysctls, ulimits, and the container-level safety switches.
func TestImportsAnExtendedContainerDefinition(t *testing.T) {
	result := parseFixture(t, "compose.yaml", map[string]string{
		"compose.yaml": `services:
  api:
    image: ghcr.io/example/api@sha256:${DIGEST:-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa}
    entrypoint: ["/usr/local/bin/entrypoint.sh"]
    command: "serve --workers 4"
    user: "1000:1000"
    working_dir: /srv
    hostname: api-internal
    platform: linux/amd64
    labels:
      com.example.tier: backend
      com.example.team: platform
    read_only: true
    shm_size: 256m
    stop_grace_period: 30s
    stop_signal: SIGQUIT
    tmpfs:
      - /tmp:size=64m
      - /run
    sysctls:
      net.core.somaxconn: 1024
    ulimits:
      nofile:
        soft: 20000
        hard: 40000
      nproc: 65535
    cpus: "1.5"
    mem_limit: 1g
    pids_limit: 512
    build:
      context: .
      target: development
      args:
        - GIT_SHA=${GIT_SHA:-unknown}
        - BUILD_FLAG=1
      dockerfile: Dockerfile
    deploy:
      resources:
        reservations:
          cpus: "0.5"
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:3000/healthz"]
      interval: 15s
      timeout: 5s
      retries: 6
      start_period: 45s
      start_interval: 2s
    restart: on-failure:5
    ports:
      - target: 3000
        published: "3000"
        protocol: tcp
    expose:
      - "3000"
`,
	})

	requireValid(t, result)
	api := service(t, result, "api")
	container := api.Container
	if container.Entrypoint == nil || (*container.Entrypoint)[0] != "/usr/local/bin/entrypoint.sh" {
		t.Fatalf("entrypoint = %v", container.Entrypoint)
	}
	// A string command is an argv split by shell-word rules, the way Compose
	// reads it — not a shell command line.
	if got := strings.Join(api.Command, " "); got != "serve --workers 4" {
		t.Fatalf("command = %q", got)
	}
	if container.User != "1000:1000" || container.WorkingDir != "/srv" || container.ContainerHostname != "api-internal" {
		t.Fatalf("container identity = %+v", container)
	}
	if container.Platform != "linux/amd64" {
		t.Fatalf("platform = %q", container.Platform)
	}
	if container.Labels["com.example.tier"] != "backend" || len(container.Labels) != 2 {
		t.Fatalf("labels = %v", container.Labels)
	}
	if !container.ReadOnly || container.ShmSize != 256<<20 || container.StopSignal != "SIGQUIT" || container.StopTimeoutSeconds != 30 {
		t.Fatalf("container switches = %+v", container)
	}
	if container.Tmpfs["/tmp"] != "size=64m" || container.Tmpfs["/run"] != "" {
		t.Fatalf("tmpfs = %v", container.Tmpfs)
	}
	if container.Sysctls["net.core.somaxconn"] != "1024" {
		t.Fatalf("sysctls = %v", container.Sysctls)
	}
	if len(container.Ulimits) != 2 || container.Ulimits[0].Name != "nofile" || container.Ulimits[0].Soft != 20000 || container.Ulimits[0].Hard != 40000 || container.Ulimits[1].Soft != 65535 {
		t.Fatalf("ulimits = %+v", container.Ulimits)
	}
	if api.CPULimit != 1.5 || api.MemoryLimit != 1<<30 || api.PIDsLimit != 512 {
		t.Fatalf("limits = %v/%v/%v", api.CPULimit, api.MemoryLimit, api.PIDsLimit)
	}
	if health := container.Healthcheck; health == nil || health.Interval != 15*time.Second || health.Retries != 6 || health.StartPeriod != 45*time.Second || health.StartInterval != 2*time.Second {
		t.Fatalf("healthcheck = %+v", health)
	}
	if api.Port != 3000 {
		t.Fatalf("port = %d, want the container port from the long Syntax ports entry", api.Port)
	}
	if api.RestartPolicy != "on-failure:5" {
		t.Fatalf("restart policy = %q", api.RestartPolicy)
	}
	if name, retries := RestartPolicy(api.RestartPolicy); name != "on-failure" || retries != 5 {
		t.Fatalf("restart policy split = %q/%d", name, retries)
	}
	// The reservations block is understood and reported; the limits beside it
	// are the ones applied.
	requireWarned(t, result, "services.api.deploy.resources.reservations")
	if api.Image != "ghcr.io/example/api@sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("digest-pinned image = %q", api.Image)
	}
	// The build settings belong to the image this release runs, so they travel
	// with the release's container configuration rather than being re-read from
	// the file at deploy time.
	if container.BuildTarget != "development" {
		t.Fatalf("build target = %q", container.BuildTarget)
	}
	if container.BuildArgs["GIT_SHA"] != "unknown" || container.BuildArgs["BUILD_FLAG"] != "1" {
		t.Fatalf("build args = %v", container.BuildArgs)
	}
	if api.BuildContext != "." {
		t.Fatalf("build context = %q", api.BuildContext)
	}
}

// TestImportsDeployLimitsAndAnonymousVolumes covers the orchestrator-facing
// spelling of the limits, a mount with no source, the long volume syntax, and
// the mount modes a SELinux host adds to every line.
func TestImportsDeployLimitsAndAnonymousVolumes(t *testing.T) {
	result := parseFixture(t, "compose.yaml", map[string]string{
		"compose.yaml": `services:
  app:
    image: example/app:1
    volumes:
      - /var/lib/app
      - ./conf/app.ini:/etc/app.ini:ro,z
      - type: bind
        source: ./conf
        target: /etc/conf
        read_only: true
        bind:
          create_host_path: true
      - type: volume
        source: app-cache
        target: /cache
      - data:/var/lib/data:cached
    deploy:
      resources:
        limits:
          cpus: "2"
          memory: 2gb
          pids: 4096
      replicas: 3
      restart_policy:
        condition: on-failure
volumes:
  data:
  app-cache:
    driver_opts:
      type: tmpfs
`,
		"conf/app.ini": "[main]\n",
	})

	requireValid(t, result)
	app := service(t, result, "app")
	if app.CPULimit != 2 || app.MemoryLimit != 2<<30 || app.PIDsLimit != 4096 {
		t.Fatalf("limits = %v/%v/%v", app.CPULimit, app.MemoryLimit, app.PIDsLimit)
	}
	if len(app.Volumes) != 5 {
		t.Fatalf("volumes = %v", app.Volumes)
	}
	anonymous := app.Volumes[0]
	if !strings.HasPrefix(anonymous, "asgard-demo-app-") || !strings.HasSuffix(anonymous, ":/var/lib/app") {
		t.Fatalf("anonymous volume = %q, want a stable project-scoped volume", anonymous)
	}
	if app.Volumes[1] != "@project/conf/app.ini:/etc/app.ini:ro" {
		t.Fatalf("selinux mode not tolerated: %q", app.Volumes[1])
	}
	if app.Volumes[2] != "@project/conf:/etc/conf:ro" {
		t.Fatalf("long-form bind mount = %q", app.Volumes[2])
	}
	if app.Volumes[3] != "asgard-demo-app-cache:/cache" {
		t.Fatalf("long-form volume mount = %q", app.Volumes[3])
	}
	if app.Volumes[4] != "asgard-demo-data:/var/lib/data" {
		t.Fatalf("cache mode not tolerated: %q", app.Volumes[4])
	}
	requireWarned(t, result, "services.app.deploy.replicas")
	requireWarned(t, result, "services.app.deploy.restart_policy")
	requireWarned(t, result, "volumes.app-cache.driver_opts")
}

// TestStorageTheProjectCreatesAtDeploy covers the shape almost every Compose
// file has: a data directory that the host creates on first start. It must be
// accepted — Asgard creates it in the project's own tree — while a *file* the
// project forgot to ship is still a clear error, because mounting an empty
// directory over a missing config file fails much later and much less clearly.
func TestStorageTheProjectCreatesAtDeploy(t *testing.T) {
	result := parseFixture(t, "compose.yaml", map[string]string{
		"compose.yaml": `services:
  db:
    image: postgres:16-alpine
    volumes:
      - ./pgdata:/var/lib/postgresql/data
      - ./uploads:/app/uploads
  app:
    image: example/app:1
    volumes:
      - ./config/app.yaml:/etc/app.yaml:ro
`,
	})
	// The data directories are accepted even though the project does not ship
	// them; the missing config *file* still stops the import.
	requireRejected(t, result, "services.app.volumes.0")
	if got := service(t, result, "db").Volumes; len(got) != 2 || got[0] != "@project/pgdata:/var/lib/postgresql/data" || got[1] != "@project/uploads:/app/uploads" {
		t.Fatalf("volumes = %v", got)
	}
}

// TestComposeFileInASubdirectory pins Compose's path resolution rules: every
// relative path is resolved against the directory holding the Compose file, and
// the project's .env beside it is the one that interpolates.
func TestComposeFileInASubdirectory(t *testing.T) {
	result := parseFixture(t, "deploy/compose.yaml", map[string]string{
		"deploy/.env":           "TAG=1.27\nAPP_SECRET=from-deploy-dir\n",
		".env":                  "TAG=0.0-broken\n",
		"deploy/nginx.conf":     "server {}\n",
		"deploy/env.list":       "LOG_LEVEL=debug\n",
		"deploy/app/Dockerfile": "FROM nginx:${TAG}\n",
		"deploy/compose.yaml": `services:
  web:
    image: nginx:${TAG}
    env_file:
      - env.list
    environment:
      APP_SECRET: ${APP_SECRET}
    volumes:
      - ./nginx.conf:/etc/nginx/nginx.conf:ro
    build:
      context: ./app
`,
	})

	requireValid(t, result)
	web := service(t, result, "web")
	if web.Image != "nginx:1.27" {
		t.Fatalf("image = %q, want the value from deploy/.env", web.Image)
	}
	if web.Environment["LOG_LEVEL"] != "debug" || web.Environment["APP_SECRET"] != "from-deploy-dir" {
		t.Fatalf("environment = %v", web.Environment)
	}
	if len(web.Volumes) != 1 || web.Volumes[0] != "@project/deploy/nginx.conf:/etc/nginx/nginx.conf:ro" {
		t.Fatalf("volumes = %v", web.Volumes)
	}
	if web.BuildContext != "deploy/app" {
		t.Fatalf("build context = %q, want deploy/app", web.BuildContext)
	}
}

// TestRefusesWhatWouldLeaveTheSandbox is the other half of widening the
// accepted set: the fields that were refused for a reason stay refused, and
// each one says why.
func TestRefusesWhatWouldLeaveTheSandbox(t *testing.T) {
	result := parseFixture(t, "compose.yaml", map[string]string{
		"compose.yaml": `services:
  app:
    image: example/app:1
    privileged: true
    network_mode: host
    pid: host
    ipc: host
    userns_mode: host
    cap_add: [SYS_ADMIN]
    security_opt: ["apparmor=unconfined"]
    extra_hosts:
      - "host.docker.internal:host-gateway"
    devices:
      - /dev/kvm
    cgroup_parent: host.slice
    volumes_from:
      - other
    extends:
      file: ../base.yml
      service: base
    imagee: example/app:2
`,
	})
	for _, path := range []string{
		"services.app.privileged", "services.app.network_mode", "services.app.pid", "services.app.ipc",
		"services.app.userns_mode", "services.app.cap_add", "services.app.security_opt", "services.app.extra_hosts",
		"services.app.devices", "services.app.cgroup_parent", "services.app.volumes_from", "services.app.extends",
		"services.app.imagee",
	} {
		requireRejected(t, result, path)
	}
	// The refusal explains itself rather than repeating one generic sentence.
	if message := requireRejected(t, result, "services.app.extra_hosts"); !strings.Contains(message, "etc/hosts") {
		t.Fatalf("extra_hosts reason = %q", message)
	}
	if message := requireRejected(t, result, "services.app.imagee"); message != UnsupportedFieldError {
		t.Fatalf("typo reason = %q", message)
	}
}

// TestTopLevelRefusalsAreStated covers the document-level keys that cannot be
// honored, and the extension keys that always can.
func TestTopLevelRefusalsAreStated(t *testing.T) {
	result := parseFixture(t, "compose.yaml", map[string]string{
		"compose.yaml": `include:
  - other.yaml
x-anything:
  note: metadata is fine anywhere
services:
  app:
    image: example/app:1
    x-custom: true
`,
	})
	requireRejected(t, result, "compose.include")
	if len(result.Errors) != 1 {
		t.Fatalf("errors = %+v", result.Errors)
	}
}

// TestServesFileBackedSecretsAndConfigs: Compose's secrets and configs have two
// sources, and only one of them is something a self-hosted platform can honor —
// a file the project ships. Serving it is what lets the samples and projects
// that use `secrets:` import at all; the other sources are refused by name.
func TestServesFileBackedSecretsAndConfigs(t *testing.T) {
	files := map[string]string{
		"db/password.txt": "hunter2\n",
		"conf/nginx.conf": "server {}\n",
		"compose.yaml": `services:
  api:
    image: example/api:1
    secrets:
      - db-password
      - source: db-password
        target: /etc/api/db-password
    configs:
      - source: nginx-conf
        target: /etc/nginx/nginx.conf
        mode: 0440
      - plain-config
secrets:
  db-password:
    file: db/password.txt
configs:
  nginx-conf:
    file: ./conf/nginx.conf
  plain-config:
    file: ./conf/nginx.conf
`,
	}
	result := parseFixture(t, "compose.yaml", files)
	requireValid(t, result)
	want := []string{
		"@project/db/password.txt:/run/secrets/db-password:ro",
		"@project/db/password.txt:/etc/api/db-password:ro",
		"@project/conf/nginx.conf:/etc/nginx/nginx.conf:ro",
		"@project/conf/nginx.conf:/plain-config:ro",
	}
	got := service(t, result, "api").Volumes
	if len(got) != len(want) {
		t.Fatalf("volumes = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("volume %d = %q, want %q", index, got[index], want[index])
		}
	}
	// The mode Compose would copy the file with is reported, not silently lost.
	requireWarned(t, result, "services.api.configs.0.mode")
}

// TestRefusesSecretsAsgardCannotServe keeps the refusal honest: a secret that
// lives in the host's store, or in the environment that ran the container, is
// not something Asgard will read on a workload's behalf, and a file that is not
// there is a mistake rather than an empty secret.
func TestRefusesSecretsAsgardCannotServe(t *testing.T) {
	result := parseFixture(t, "compose.yaml", map[string]string{
		"compose.yaml": `services:
  api:
    image: example/api:1
    secrets:
      - from-host
      - from-shell
      - missing-file
    configs:
      - from-host
    networks: [default]
secrets:
  from-host:
    external: true
  from-shell:
    environment: DB_PASSWORD
  missing-file:
    file: ./nowhere.txt
configs:
  from-host:
    external: true
`,
	})
	requireRejected(t, result, "secrets.from-host.external")
	requireRejected(t, result, "secrets.from-shell.environment")
	requireRejected(t, result, "secrets.missing-file.file")
	requireRejected(t, result, "configs.from-host.external")
	// A grant of something that cannot be served is not reported twice: the
	// declaration already says why.
	if len(result.Errors) != 4 {
		t.Fatalf("errors = %+v", result.Errors)
	}
}

// TestEnvironmentInheritedFromTheShell is reported rather than silently given
// some other value: Compose would read these from the shell that ran it, and
// Asgard never exposes its own environment to a workload.
func TestEnvironmentInheritedFromTheShell(t *testing.T) {
	result := parseFixture(t, "compose.yaml", map[string]string{
		"compose.yaml": `services:
  app:
    image: example/app:1
    environment:
      EXPLICIT: value
      FROM_SHELL:
    env_file: .env
`,
		".env": "FROM_FILE=yes\n",
	})
	requireValid(t, result)
	environment := service(t, result, "app").Environment
	if _, present := environment["FROM_SHELL"]; present {
		t.Fatalf("an inherited variable must stay unset, got %v", environment)
	}
	if environment["EXPLICIT"] != "value" || environment["FROM_FILE"] != "yes" {
		t.Fatalf("environment = %v", environment)
	}
	if message := requireWarned(t, result, "services.app.environment"); !strings.Contains(message, "FROM_SHELL") {
		t.Fatalf("warning = %q", message)
	}
}

// TestDependencyConditionsAreReportedOnce: Asgard always waits for a
// dependency's own readiness gate, which is more than `service_started` asked
// for, and the file is told so.
func TestDependencyConditionsAreReportedOnce(t *testing.T) {
	result := parseFixture(t, "compose.yaml", map[string]string{
		"compose.yaml": `services:
  db:
    image: postgres:16-alpine
    healthcheck:
      test: ["CMD-SHELL", "pg_isready"]
  migrate:
    image: example/migrate:1
    restart: "no"
    depends_on:
      db:
        condition: service_completed_successfully
  app:
    image: example/app:1
    depends_on:
      db:
        condition: service_started
      migrate:
        condition: service_completed_successfully
`,
	})
	requireValid(t, result)
	requireWarned(t, result, "services.app.depends_on.db")
	if app := service(t, result, "app"); len(app.DependsOn) != 2 {
		t.Fatalf("dependencies = %v", app.DependsOn)
	}
}

// TestUnsupportedNetworkMembershipIsReported: a service that joins an external
// network does not get it, and the reason names what to use instead.
func TestUnsupportedNetworkMembershipIsReported(t *testing.T) {
	result := parseFixture(t, "compose.yaml", map[string]string{
		"compose.yaml": `services:
  app:
    image: example/app:1
    networks:
      backend:
        aliases: [api, api.internal]
      other:
        ipv4_address: 172.16.0.9
networks:
  backend: {}
  other:
    external: true
`,
	})
	requireValid(t, result)
	if got := service(t, result, "app").Container.NetworkAliases; len(got) != 2 || got[0] != "api" || got[1] != "api.internal" {
		t.Fatalf("network aliases = %v", got)
	}
	requireWarned(t, result, "services.app.networks.other")
	requireWarned(t, result, "services.app.networks.other.ipv4_address")
	requireRejected(t, parseFixture(t, "compose.yaml", map[string]string{
		"compose.yaml": "services:\n  app:\n    image: example/app:1\n    networks: [nowhere]\n",
	}), "services.app.networks")
}

// TestHealthcheckDisableAndDefaults covers the two healthcheck spellings that
// matter to the readiness gate: disabling an image's healthcheck, and a test
// given as a shell string.
func TestHealthcheckDisableAndDefaults(t *testing.T) {
	result := parseFixture(t, "compose.yaml", map[string]string{
		"compose.yaml": `services:
  quiet:
    image: example/app:1
    healthcheck:
      disable: true
  shell:
    image: example/app:2
    healthcheck:
      test: pg_isready -U app
`,
	})
	requireValid(t, result)
	quiet := service(t, result, "quiet").Container.Healthcheck
	if quiet == nil || len(quiet.Test) != 1 || quiet.Test[0] != "NONE" {
		t.Fatalf("disabled healthcheck = %+v", quiet)
	}
	shell := service(t, result, "shell").Container.Healthcheck
	if shell == nil || len(shell.Test) != 2 || shell.Test[0] != "CMD-SHELL" || shell.Test[1] != "pg_isready -U app" {
		t.Fatalf("shell healthcheck = %+v", shell)
	}
}

// TestStringCommandsAreSplitTheWayComposeSplitsThem pins the parity that
// matters most in practice: a string is an argv, not a shell command line.
// Compose parses it with shell-word rules, so a list of flags reaches the
// image's entrypoint intact, quoting groups arguments, and nothing is ever
// handed to /bin/sh. These cases were taken from what `docker compose up`
// produces on a real daemon.
func TestStringCommandsAreSplitTheWayComposeSplitsThem(t *testing.T) {
	result := parseFixture(t, "compose.yaml", map[string]string{
		"compose.yaml": `services:
  flags:
    image: mariadb:10.6.4-focal
    command: '--default-authentication-plugin=mysql_native_password'
  words:
    image: alpine:3.22
    command: 'echo hello world'
  quoted:
    image: alpine:3.22
    command: 'printf "%s\n" "x y"'
  entry:
    image: alpine:3.22
    entrypoint: '/bin/echo hi'
    command: ['there']
  single:
    image: alpine:3.22
    command: ['/app/server', '--port=3000']
`,
	})
	requireValid(t, result)
	cases := map[string][]string{
		"flags":  {"--default-authentication-plugin=mysql_native_password"},
		"words":  {"echo", "hello", "world"},
		"quoted": {"printf", "%s\n", "x y"},
		"entry":  {"there"},
		"single": {"/app/server", "--port=3000"},
	}
	for name, want := range cases {
		got := service(t, result, name).Command
		if len(got) != len(want) {
			t.Fatalf("%s command = %q, want %q", name, got, want)
		}
		for index := range want {
			if got[index] != want[index] {
				t.Fatalf("%s command = %q, want %q", name, got, want)
			}
		}
	}
	entrypoint := service(t, result, "entry").Container.Entrypoint
	if entrypoint == nil || len(*entrypoint) != 2 || (*entrypoint)[0] != "/bin/echo" || (*entrypoint)[1] != "hi" {
		t.Fatalf("entrypoint = %v", entrypoint)
	}
}

// TestLimitsKeepThePlatformDefaultsWhenTheFileIsSilent, so a service that says
// nothing is still bounded.
func TestLimitsKeepThePlatformDefaultsWhenTheFileIsSilent(t *testing.T) {
	result := parseFixture(t, "compose.yaml", map[string]string{
		"compose.yaml": "services:\n  app:\n    image: example/app:1\n",
	})
	requireValid(t, result)
	app := service(t, result, "app")
	if app.CPULimit != 0.5 || app.MemoryLimit != 512<<20 || app.PIDsLimit != 256 {
		t.Fatalf("defaults = %v/%v/%v", app.CPULimit, app.MemoryLimit, app.PIDsLimit)
	}
	if app.RestartPolicy != "unless-stopped" || app.Role != "web" || app.HealthPath != "/" {
		t.Fatalf("defaults = %+v", app)
	}
}

// TestNonsenseValuesAreRefusedNotDefaulted: a file that asks for something
// impossible gets told, instead of silently running with something else.
func TestNonsenseValuesAreRefusedNotDefaulted(t *testing.T) {
	for name, body := range map[string]string{
		"unreadable size":      "services:\n  app:\n    image: example/app:1\n    mem_limit: enormous\n",
		"impossible cpu":       "services:\n  app:\n    image: example/app:1\n    cpus: \"0\"\n",
		"both limit spellings": "services:\n  app:\n    image: example/app:1\n    mem_limit: 512m\n    deploy:\n      resources:\n        limits:\n          memory: 1g\n",
		"bad restart budget":   "services:\n  app:\n    image: example/app:1\n    restart: on-failure:many\n",
		"bad healthcheck":      "services:\n  app:\n    image: example/app:1\n    healthcheck:\n      test: [\"RUN\", \"true\"]\n",
		"unknown volume mode":  "services:\n  app:\n    image: example/app:1\n    volumes:\n      - data:/data:magic\nvolumes:\n  data:\n",
		"undeclared volume":    "services:\n  app:\n    image: example/app:1\n    volumes:\n      - not-declared:/data\n",
		"escaping build":       "services:\n  app:\n    image: example/app:1\n    build:\n      context: ../../elsewhere\n",
		"escaping env_file":    "services:\n  app:\n    image: example/app:1\n    env_file: ../outside.env\n",
		"empty healthcheck":    "services:\n  app:\n    image: example/app:1\n    healthcheck:\n      interval: 10s\n",
	} {
		t.Run(name, func(t *testing.T) {
			result := parseFixture(t, "compose.yaml", map[string]string{"compose.yaml": body})
			if result.Valid {
				t.Fatalf("accepted: %+v", result.Services)
			}
		})
	}
}

// TestExtensionKeysAreLegalEverywhere: x-* is Compose's own escape hatch and
// anchors are how real files avoid repeating themselves. A file that uses both
// must import unchanged, at every level of the document.
func TestExtensionKeysAreLegalEverywhere(t *testing.T) {
	result := parseFixture(t, "compose.yaml", map[string]string{
		"compose.yaml": `x-templates:
  healthcheck: &health
    test: ["CMD", "true"]
    interval: 10s

services:
  app:
    x-meta:
      owner: platform
    image: example/app:1
    healthcheck:
      <<: *health
      retries: 4
    build:
      context: .
      x-cache: local
    deploy:
      x-note: values are informational
      resources:
        limits:
          cpus: "1"
    volumes:
      - type: volume
        source: cache
        target: /cache
        x-flag: true
    ulimits:
      nofile: 1024
volumes:
  cache:
    x-owner: platform
networks:
  default:
    x-note: the project network
`,
	})
	requireValid(t, result)
	app := service(t, result, "app")
	if app.CPULimit != 1 {
		t.Fatalf("limits from a merged section = %v", app.CPULimit)
	}
	if health := app.Container.Healthcheck; health == nil || health.Interval != 10*time.Second || health.Retries != 4 {
		t.Fatalf("merged healthcheck = %+v", health)
	}
	if len(app.Volumes) != 1 || app.Volumes[0] != "asgard-demo-cache:/cache" {
		t.Fatalf("volumes = %v", app.Volumes)
	}
	if len(app.Container.Ulimits) != 1 || app.Container.Ulimits[0].Soft != 1024 {
		t.Fatalf("ulimits = %+v", app.Container.Ulimits)
	}
}

// TestXAsgardTyposAreCaught: the routing block is the one place an operator
// writes by hand, and a misspelled key there used to be ignored in silence —
// leaving a service with the default health path and no explanation.
func TestXAsgardTyposAreCaught(t *testing.T) {
	result := parseFixture(t, "compose.yaml", map[string]string{
		"compose.yaml": `services:
  app:
    image: example/app:1
x-asgard:
  primary-service: app
  services:
    app:
      healthpath: /healthz
    nobody:
      role: web
`,
	})
	requireRejected(t, result, "compose.x-asgard.services.app.healthpath")
	requireRejected(t, result, "compose.x-asgard.services.nobody")
	// A primary service that does not exist is still caught by name.
	missing := parseFixture(t, "compose.yaml", map[string]string{
		"compose.yaml": "services:\n  app:\n    image: example/app:1\nx-asgard:\n  primary-service: nowhere\n",
	})
	requireRejected(t, missing, "x-asgard.primary-service")
}

// TestWarningPathsAreStable keeps the ignored-key reporting honest: a warning
// names the exact key, so an author can find it.
func TestWarningPathsAreStable(t *testing.T) {
	result := parseFixture(t, "compose.yaml", map[string]string{
		"compose.yaml": `services:
  app:
    image: example/app:1
    container_name: app
    logging:
      driver: json-file
    pull_policy: always
    init: true
`,
	})
	requireValid(t, result)
	got := strings.Join(warningPaths(result), ",")
	want := "services.app.container_name,services.app.init,services.app.logging,services.app.pull_policy"
	if got != want {
		t.Fatalf("warning paths = %q, want %q", got, want)
	}
	for _, warning := range result.Warnings {
		if !strings.HasPrefix(warning.Message, "Ignored: ") {
			t.Fatalf("warning without an explanation: %+v", warning)
		}
	}
}

// TestAcceptsTheDefaultBridgeSpelling: `network_mode: bridge` names Docker's
// own default network, which the project network replaces. Refusing it refused
// files that were only describing the ordinary case — while host, container,
// and service network modes stay refused, because those do change what the
// workload can reach.
func TestAcceptsTheDefaultBridgeSpelling(t *testing.T) {
	result := parseFixture(t, "compose.yaml", map[string]string{
		"compose.yaml": `services:
  app:
    image: example/app:1
    network_mode: bridge
`,
	})
	requireValid(t, result)
	requireWarned(t, result, "services.app.network_mode")

	for _, mode := range []string{"host", "none", "service:other", "container:abc"} {
		rejected := parseFixture(t, "compose.yaml", map[string]string{
			"compose.yaml": "services:\n  app:\n    image: example/app:1\n    network_mode: \"" + mode + "\"\n",
		})
		requireRejected(t, rejected, "services.app.network_mode")
	}
}
