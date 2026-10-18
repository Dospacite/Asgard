# Asgard

Asgard is a self-hosted application cloud for a single Linux host. You point it
at a Docker Compose project, a Git repository, or a public image; it builds a
versioned release, runs the containers under resource limits, gives them
hostnames and HTTPS certificates, and keeps logs, metrics, backups, and an audit
trail. An OAuth-protected MCP server exposes the same operations to coding
agents.

It is built for one administrator and one machine. There is no cluster, no
multi-tenancy, and no scheduler — a VPS with Docker, a domain you control, and
about 1 GB of free RAM is the whole environment.

---

## What you get

**Projects and releases.** Import from a Git repository (public or private), an
uploaded archive (`.zip`, `.tar`, `.tar.gz`, `.tar.bz2`, `.tar.xz`, `.tar.zst`),
or a public OCI image. Every deployment produces a numbered release with a
Compose snapshot, so a rollback is a real thing you can point at.

**A Compose contract that tells the truth.** Asgard applies the fields it can
honor and *says so* about the ones it cannot. Image, build, command, entrypoint,
environment, `env_file`, volumes, labels, `user`, `working_dir`, `read_only`,
`tmpfs`, `sysctls`, `ulimits`, `shm_size`, `stop_grace_period`, `stop_signal`,
`platform`, dependencies, health checks, restart policies, and CPU/memory/PID
limits — in either the `deploy.resources.limits` or the `mem_limit`/`cpus`
spelling — all reach the container. `container_name`, `profiles`, `logging`,
`init`, `tty`, `dns`, `links` and the other keys with no Asgard equivalent are
accepted with a warning that names them, so an ordinary project imports
unchanged instead of having to be forked into an Asgard-only Compose file.
`x-*` extension keys and YAML anchors with `<<:` merge keys are legal
everywhere, and relative paths — build context, `env_file`, project-relative
mounts, and the project's `.env` — resolve against the Compose file's
directory, exactly as Compose resolves them.

Builds run on the host daemon's BuildKit under a client build session, the way
`docker build` runs them, so earlier build stages stay cached between releases,
`RUN --mount=type=cache` works, and a `# syntax=` frontend resolves. Base images
are pulled anonymously. A daemon without BuildKit gets the same build on the
legacy builder, which caches only the final stage. An image build and a storage
reclamation pass never overlap; a scheduled pass that finds a build running is
skipped until the next deployment or sweep.
`build.target` selects a stage, and `build.args` reach the builder. A string
`command` or `entrypoint` is split into an argv the way Compose splits it, not
handed to a shell, so the flags-as-a-string idiom reaches the image's entrypoint
intact.

File-backed Compose `secrets` and `configs` are served where Compose mounts them
— `/run/secrets/<name>` and `/<name>` — from the project's own source, so
`secrets: {db-password: {file: ./db/password.txt}}` works; a secret that lives in
the host's store, or in the environment that ran the container, is refused by
name.

`network_mode: bridge`, `security_opt: [no-new-privileges]`, and `cap_drop: [ALL]`
name the platform's own defaults and are accepted with a note.

Privileged mode, host networking, devices, capability additions, `extra_hosts`,
Docker socket mounts, and host bind mounts outside the project are still
rejected, each with the reason. Named volumes and
project-relative paths are allowed; a data directory the project does not ship,
such as `./data:/data`, is created in the project's own source tree. `./data`
counts as a directory; a missing *file* — `./secrets/key.json` — is still an
error, because mounting an empty directory over it would fail later and less
clearly.

**Routing and TLS.** Traefik fronts every workload. A service marked public gets
a router, a health check, and an automatic Let's Encrypt certificate for its
hostname. Services can use the wildcard zone you configure or claim any name
whose DNS points at the host.

**Observability that tells the truth.** CPU, memory, network, block I/O, PIDs,
and *CPU throttling* per service, plus host capacity, container logs, operation
logs, and an audit trail. Throttling matters more than it sounds: a
request-serving container bursts against its CPU quota in well under a second,
so it can average near-zero CPU while being stopped at the ceiling 40% of the
time. Asgard reports both.

**Logs that outlive a release.** The usual host-side shippers — Promtail, Alloy,
Fluent Bit — all read `/var/lib/docker/containers`, which the safe Compose
contract refuses to expose, so none of them can run on the workloads here.
Asgard is already the process holding a Docker client for the whole host, so it
tails every running managed container itself and pushes the frames to Loki;
Grafana queries them. See [Log collection](#log-collection).

**Networking.** Each project gets a private bridge by default. Managed shared
networks give explicit cross-project connectivity with stable DNS aliases, live
endpoint reconciliation, and topology views.

**Credentials that are checked.** Git tokens and SSH deploy keys are encrypted
with a host-local AES-256-GCM key and used only during a clone. Each one is
proven against its repository with `git ls-remote` when stored, when rotated,
and on a schedule — so a revoked key shows up in settings instead of in the
middle of a release. Secrets rotate in place, keeping their id, so projects
already using a credential pick up the replacement with no re-import.

**Backups.** Named-volume snapshot and restore, plus guarded deletion. Deleting
a project removes containers, source, routes, and project networks but retains
named volumes.

**Agent access.** OAuth 2.1 authorization-code flow with PKCE, dynamic client
registration, resource-bound tokens, exact scopes, and a stateless MCP
`2026-07-28` endpoint, with a plugin for Claude Code and Codex.

Private registries and signed Git redeploy webhooks are out of scope.

---

## Requirements

- A Linux host with Docker Engine 25+ and the Compose plugin.
- Ports 80 and 443 free and reachable from the internet (Let's Encrypt uses the
  HTTP-01 challenge).
- A domain you control, with a wildcard `A`/`AAAA` record pointing at the host.
- Go 1.24+ and Node 20+ only if you intend to build from source rather than let
  Compose build the image.

Asgard mounts the Docker socket. It can start, stop, and delete containers on
the host it runs on — run it on a machine you are willing to give it.

---

## Install

### 1. Point DNS at the host

Pick the zone Asgard will own — say `apps.example.com` — and create two records:

```
apps.example.com.     A   203.0.113.10
*.apps.example.com.   A   203.0.113.10
```

The wildcard is what lets a newly imported project get a working hostname
without another DNS change. Individual projects may also claim names outside
this zone later, as long as those names resolve to the same host.

### 2. Get the source onto the host

```sh
sudo git clone https://github.com/rousoftware/asgard /opt/asgard
cd /opt/asgard
```

### 3. Configure

```sh
cp deploy/.env.example deploy/.env
```

Edit `deploy/.env`:

| Variable | Meaning |
| --- | --- |
| `ASGARD_PUBLIC_URL` | Where you will reach the control plane, e.g. `https://asgard.apps.example.com` |
| `ASGARD_DOMAIN` | The wildcard zone workloads get hostnames under, e.g. `apps.example.com` |
| `ASGARD_ACME_EMAIL` | Contact address for Let's Encrypt expiry notices |
| `ASGARD_TIMEZONE` | IANA timezone used for displayed timestamps |
| `ASGARD_SECURE_COOKIES` | Leave `true` for any real deployment |
| `ASGARD_CREDENTIAL_VERIFY_HOURS` | How often stored Git credentials are re-proven (default `6`, `0` disables) |
| `ASGARD_KEEP_RELEASE_IMAGES` | Releases whose images are kept — also how far back a rollback can reach (default `3`) |
| `ASGARD_BUILD_CACHE_GB` | Build cache budget (default `10`, `0` disables cache pruning) |
| `ASGARD_RECLAIM_HOURS` | How often disk is reclaimed (default `12`, `0` disables) |
| `ASGARD_GRAFANA_HOSTNAME` | Hostname of the log collector's UI, e.g. `grafana.apps.example.com` |
| `ASGARD_GRAFANA_ADMIN_PASSWORD` | Grafana's admin password; also the only credential the collector exposes |
| `ASGARD_LOG_SINK_URL` | Where container logs are pushed (default `http://loki:3100`); empty collects nothing |
| `ASGARD_LOG_SHIP_INTERVAL_SECONDS` | How long output may sit uncollected (default `15`) |
| `ASGARD_LOG_SHIP_LOOKBACK_SECONDS` | First read of a container with no checkpoint (default `300`) |
| `ASGARD_LOG_SHIP_BATCH_KB` | Largest single push (default `1024`) |

`ASGARD_DOMAIN` also decides HSTS defaults — see [HTTPS and
HSTS](#https-and-hsts) below — so set it to the zone you actually own.

That is the only file you edit. Traefik's static configuration and the control
plane's own router are rendered from these values at startup, so no tracked file
carries a hostname or an email address.

### 4. Start it

```sh
docker compose -f deploy/compose.yaml config -q     # validate before building
docker compose -f deploy/compose.yaml up -d --build
```

### 5. Create the administrator

```sh
docker compose -f deploy/compose.yaml exec asgard asgard admin create --username you
```

The generated one-time password is printed, and also written to
`/root/asgard-initial-password` on the host. Sign in at your
`ASGARD_PUBLIC_URL`, reset the password, then delete that file.

---

## Local development

No Docker host is required to work on the UI; it is required for anything that
actually runs a container.

```sh
cp deploy/.env.example deploy/.env
npm --prefix web install
npm --prefix web run build

export ASGARD_DATA_DIR="$PWD/data"
export ASGARD_SECURE_COOKIES=false
export ASGARD_PUBLIC_URL=http://127.0.0.1:8080
export ASGARD_DOMAIN=apps.localhost

go run ./cmd/asgard admin create --username you
go run ./cmd/asgard serve
```

The control plane listens on `http://127.0.0.1:8080`.

Before opening a pull request:

```sh
go test ./...
npm --prefix web run typecheck
npm --prefix web audit --audit-level=high
docker compose -f deploy/compose.yaml config -q
```

---

## Deploying your first project

1. **Import.** *Projects → Import project.* Give it a name and a slug, then a
   repository URL, an archive, or an image reference. Asgard validates the
   Compose file against the safe contract and shows you the resulting service
   plan before anything runs.
2. **Configure.** Set CPU, memory, and PID limits per service, choose which
   service is public and on which port, and set a health path. Defaults are
   0.5 CPU and 512 MiB — deliberately small. Watch the throttling metric on the
   service page and raise the CPU limit if the service is spending real time at
   its ceiling.
3. **Deploy.** The release builds, starts, waits for the health check, and only
   then moves traffic. If the health gate fails, the previous release keeps
   serving.

### What the release gate waits for

A release is ordered by `depends_on`, and every service has to become ready
before the services that depend on it are created — or before the release is
declared live.

- **A healthcheck the Compose file declares is applied to the container**, so
  the file's own definition of healthy is the one the gate uses. Its
  `start_period` and `retries × interval` also set how long the release will
  wait before giving up, instead of a fixed ninety seconds that failed slow
  databases and first-boot migrations.
- **A service with `restart: "no"` is a job**, and its exit code is the verdict:
  exiting zero means the work it was there to do is done, which is exactly what
  `depends_on: {condition: service_completed_successfully}` waits for, and any
  other exit fails the release with the job's logs. A job that never finishes is
  reported as such. A service that declares a healthcheck is never treated as a
  job — it is asking to be probed.
- **A service with no healthcheck** is ready once it has stayed up for five
  seconds.
- **A public service** must also answer HTTP at its health path: any status
  below 500 counts, because a login page answering 401 or an API answering 404
  at `/` is a live server. A 5xx keeps the gate waiting.

**Unchanged services keep running.** When a release would create exactly the
container a service already runs — same image, same configuration — and that
container is up and not unhealthy, it carries over into the new release instead
of being recreated, the way `docker compose up` leaves an unchanged service
alone. A one-shot job always runs again, and a service that mounts files from
the project source is always recreated, because a re-sync replaces the source
tree a running container would still be holding.

### Git projects and the source snapshot

An import captures the repository **once**. Later deployments rebuild that
captured tree, which is what makes a release reproducible and a rollback
meaningful — but it also means pushing a commit does not change what deploys.

Re-sync first:

```
Project → Re-sync source     (or the project_source_resync MCP tool)
```

Re-syncing re-clones the tracked ref, records the commit, preserves the
project's `.env` and runtime overrides, and leaves the running project untouched
if the incoming Compose file fails validation.

You do not have to remember this. Every deployment reports the commit it is
building and warns when the tracked ref has moved ahead of it.

### Private repositories

Store a credential under *Settings → Git credentials*: a fine-grained HTTPS
token or an SSH deploy key. Give it a **repository to verify against** — token
scopes are per repository, so being able to reach `github.com` proves nothing
about whether the credential can read the repo you care about. Asgard runs
`git ls-remote` against it immediately and on a schedule.

To replace an expired or leaked secret, use **Rotate** rather than creating a
second credential. Rotation keeps the credential's id, so every project already
pointing at it picks up the new secret.

---

## HTTPS and HSTS

Every public route gets `X-Frame-Options`, `X-Content-Type-Options`, a referrer
policy, and an HTTPS redirect. `Strict-Transport-Security` is chosen per route:

| Mode | Header | When |
| --- | --- | --- |
| **Automatic** (default) | Strong inside `ASGARD_DOMAIN`, plain `max-age` elsewhere | Almost always |
| **Standard** | `max-age=31536000` | A custom domain you want pinned to HTTPS |
| **Strict** | adds `includeSubDomains; preload` | A domain you fully control |
| **Off** | no header | A name something else also serves over plain HTTP |

The distinction matters. `includeSubDomains` on `example.com` forces HTTPS on
*every* subdomain of that domain — including hosts Asgard has never heard of and
does not serve — and `preload` asks browser vendors to bake that in. Removal
from the preload list takes months. Inside your own wildcard zone that is fine,
because every name under it is Asgard's; on a custom domain it is a commitment
you should make deliberately. Set it per service under *Configuration →
Routing*.

---

## Log collection

`docker compose -f deploy/compose.yaml up -d` also starts a collector: **Loki**
holding the logs and **Grafana** querying them. Both are ordinary members of the
Compose project, on the management network.

**Why the shipping happens in the control plane.** Promtail, Grafana Alloy,
Fluent Bit and every similar agent read container logs from
`/var/lib/docker/containers` or from the Docker socket. Asgard's safe Compose
contract refuses both — a project may only mount its own source and declared
named volumes — so none of those agents can run against the workloads here, and
Compose has no `logging` driver field to redirect a container's stdout. The
control plane is the one process on the host that already holds a Docker client,
so it does the tailing: every `ASGARD_LOG_SHIP_INTERVAL_SECONDS` it reads what
each running managed container wrote since its last checkpoint, and pushes the
frames to `ASGARD_LOG_SINK_URL`.

**What arrives.** Each line is labelled `project`, `service` and `stream`
(`stdout` or `stderr`), taken from the labels Asgard puts on the container. The
container name, image and release are deliberately not labels: they change on
every deployment, and a label that changes with each release is how a log store
ends up with millions of streams.

Only the containers of *projects* are collected. Helper containers — the
migration and bootstrap scripts a deployment runs — are skipped, because their
output is already in the operation log, and so are the services in
`deploy/compose.yaml` itself: the control plane, Traefik, Loki and Grafana. Had
the last group been included, Loki's own output would be stored inside Loki,
Traefik's scanner traffic would swamp the store, and the control plane's stdout
would be filed under a project that does not exist. Read those with
`docker compose -f deploy/compose.yaml logs`.

**What that costs.** A checkpoint per container is stored in the control plane's
SQLite database, holding the timestamp and the last line shipped, and is pruned
when the container it belongs to disappears. A burst larger than
`ASGARD_LOG_SHIP_BATCH_KB` is sent in several pushes, and the checkpoint only
moves once a push is accepted — so a Loki that is down, restarting, or refusing
writes costs a delay, not data, as long as Docker still holds the container's
log file.

**Querying.** Open `https://$ASGARD_GRAFANA_HOSTNAME`, sign in as `admin` with
`ASGARD_GRAFANA_ADMIN_PASSWORD`, and use *Explore* or the provisioned **Asgard
container logs** dashboard. The dashboard filters by project and service; the
underlying query is ordinary LogQL, e.g. `{service="web"} |= "error"`.

**Exposure.** Grafana is the only part of the collector with a route, and it
authenticates; `GF_USERS_ALLOW_SIGN_UP`, anonymous access, the update check and
the usage report are all off in `deploy/compose.yaml`. Loki has no router and no
published port, and its configuration disables auth because nothing outside the
management network can reach port 3100 — the control plane pushes to it and
Grafana reads from it. If you would rather not have the UI on the internet at
all, drop the `grafana` router from `deploy/dynamic/control-plane.yml.template`
and reach it over an SSH tunnel instead.

**Retention.** Two weeks (`retention_period: 336h` in
`deploy/loki/loki-config.yaml`), enforced by Loki's compactor. Changing it means
changing that file and restarting Loki; the data lives in `asgard-loki-data`.

**Turning it off.** Set `ASGARD_LOG_SINK_URL=` (empty) in `deploy/.env` and
recreate the control plane. Shipping stops, the checkpoints stop being written,
and the collector keeps serving whatever it already has.

---

## Agent access

The MCP endpoint is `<ASGARD_PUBLIC_URL>/mcp`. Clients discover OAuth metadata
at `/.well-known/oauth-protected-resource/mcp`, register dynamically, and ask
the administrator to approve explicit scopes in the browser. Scopes are
`read`, `operate`, `deploy`, `configure`, `backup`, and `delete`; every deletion
additionally requires a short-lived confirmation token from a preview call.

Useful tools:

- `project_source_get` / `project_source_update` — inspect and revision-safely
  edit a project's Compose, Dockerfile, and `.env`.
- `project_source_resync` — refresh a Git project's tree to its branch head.
- `deployment_create` — queue a health-gated release; the response names the
  commit being built and flags a tree that has fallen behind.
- `service_config_update` — runtime environment, limits, routing, HSTS mode.
- `service_stats_get` — including CPU throttling, which is what actually
  explains a slow service.
- `git_credentials_list`, `git_credential_create`, `git_credential_update`,
  `git_credential_verify`, `project_credential_set` — store, rotate, prove, and
  attach source credentials.

### Installing the plugin

The plugin lives under `plugins/asgard` and carries both manifests:
`.claude-plugin/plugin.json` for Claude Code and `.codex-plugin/plugin.json` for
Codex. Marketplace entries are in `.claude-plugin/marketplace.json` and
`.agents/plugins/marketplace.json`.

```sh
claude plugin marketplace add /path/to/Asgard
claude plugin install asgard@rousoftware
```

Both hosts share the `asgard-deploy` and `asgard-develop` skills. Claude Code
additionally gets the `/asgard-status`, `/asgard-deploy`, and `/asgard-rollback`
slash commands and a read-only release-monitor subagent. Codex-specific per-skill
metadata lives in `skills/*/agents/openai.yaml`.

---

## Operating

```sh
cd /opt/asgard
docker compose -f deploy/compose.yaml ps
docker compose -f deploy/compose.yaml logs -f --tail=200 asgard traefik
docker compose -f deploy/compose.yaml exec asgard asgard doctor
docker compose -f deploy/compose.yaml exec asgard asgard admin reset-password --username you
```

### Upgrading

```sh
cd /opt/asgard
git pull
docker compose -f deploy/compose.yaml up -d --build
```

Schema migrations run at startup. Workload containers are not restarted by an
upgrade of the control plane.

### Disk

Asgard builds one image per service per release (`asgard/<slug>/<service>:r<n>`)
and Docker caches every build layer, so a host that only ever deploys will fill
up. Asgard reclaims what it created, and only what it created:

- Images from releases past `ASGARD_KEEP_RELEASE_IMAGES`.
- Every image of a project that has been deleted.
- Untagged layers left behind by rebuilds.
- Build cache over `ASGARD_BUILD_CACHE_GB`.

Images backing the most recent releases, and any image a container still
references, are never removed — retention is what bounds how far back a rollback
can reach, which is why the floor is one release rather than zero. Upstream
images a project pulls are shared and never touched.

This runs after every successful deployment, on the `ASGARD_RECLAIM_HOURS`
sweep, and when a project is deleted. To act immediately, use *Settings →
Disk reclamation* (which has a preview), `POST /api/v1/system/reclaim`, or the
`storage_reclaim` MCP tool with `dryRun` first.

The log collector is bounded the same way, by retention rather than by a sweep:
Loki drops chunks older than `retention_period` (two weeks by default) from
`asgard-loki-data`, so a chatty service costs disk for a fortnight rather than
forever.

### Where state lives

Everything is in the `asgard-data` Docker volume: the SQLite database, project
source trees, encryption keys, generated Traefik configuration, and backups.
Certificates are in `asgard-traefik-data`. The collected logs are in
`asgard-loki-data` and Grafana's users, dashboards and API keys are in
`asgard-grafana-data`; both are rebuildable, in the sense that losing them costs
history rather than anything Asgard needs to run. Back up the first two.

The AES key in `keys/secrets.key` decrypts every stored Git credential. Losing
it means re-entering those secrets; leaking it means rotating them.

---

## Security posture

- Argon2id password hashing, Ed25519 session JWTs, rotating refresh sessions.
- SameSite=Strict, Secure, HttpOnly cookies; CSRF protection; login throttling.
- Encrypted, write-only Git credentials — never returned by the API, never
  written into a project's source, never passed through argv or a remote URL.
- Containers run with `no-new-privileges` and enforced CPU, memory, and PID
  limits.
- Deletions require a preview followed by a confirmation token.
- Every mutation is recorded in the audit log with actor, IP, and user agent.

Asgard has a single administrator account and no authorization model beyond it.
Anyone who can sign in can do anything, including deleting projects. Treat the
control plane hostname as sensitive and keep it off shared machines.
