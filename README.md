# Asgard

Asgard is a private, single-host application cloud for small workloads. It imports safe Docker Compose projects, builds versioned releases, groups containers under projects, manages private cross-project networks, routes wildcard hostnames through Traefik, provisions TLS, exposes metrics and logs, snapshots named volumes, and provides an OAuth-protected MCP server for agents.

The production control plane is available at [asgard.rousoftware.com](https://asgard.rousoftware.com). Workloads receive hostnames beneath `*.asgard.rousoftware.com` and individual Let's Encrypt certificates automatically.

## Included in the MVP

- Single-administrator authentication with Argon2id passwords, Ed25519 JWTs, rotating refresh sessions, CSRF protection, and login throttling.
- Project imports from ZIP files, public HTTPS Git repositories, and public OCI/Docker image references.
- A constrained Compose contract with project/service configuration, CPU, memory, PID, restart, health, environment, volume, dependency, and routing settings.
- An in-browser source workspace for validated Compose, Dockerfile, and `.env` editing, plus per-service selection of reusable `.env` variables and explicit overrides.
- Durable, idempotent deployments; versioned releases; health-gated traffic switching; rollbacks; operation progress and logs.
- Live container CPU, memory, network, block I/O, PID, disk, host-capacity, log, and audit views.
- Automatic DNS routing and HTTPS through Traefik, using the configured wildcard DNS record.
- Project-isolated bridges by default, plus explicit managed shared networks with stable private DNS aliases, live endpoint reconciliation, and network-, project-, and endpoint-centric topology dashboards.
- Named-volume backups/restores and guarded project/container deletion. Project deletion retains named volumes but removes workload containers, source files, routes, and project networks.
- OAuth 2.1 authorization-code flow with PKCE, dynamic client registration, resource-bound tokens, exact scopes, and a stateless MCP `2026-07-28` endpoint.
- A Codex plugin with `asgard-deploy` and `asgard-develop` skills.

Private Git repositories, private registries, and signed Git redeploy webhooks are intentionally outside this single-user MVP.

## Local development

```sh
cp deploy/.env.example deploy/.env
npm --prefix web install
npm --prefix web run build
ASGARD_DATA_DIR="$PWD/data" ASGARD_SECURE_COOKIES=false go run ./cmd/asgard admin create --username ege
ASGARD_DATA_DIR="$PWD/data" ASGARD_SECURE_COOKIES=false go run ./cmd/asgard serve
```

The control plane listens on `http://127.0.0.1:8080`. Docker access is optional for UI development and required for workload operations.

## VPS deployment

Copy the repository to `/opt/asgard`, set `deploy/.env`, update the ACME email in `deploy/traefik.yaml`, then run:

```sh
docker compose -f deploy/compose.yaml config -q
docker compose -f deploy/compose.yaml up -d --build
docker compose -f deploy/compose.yaml exec asgard asgard admin create --username ege
```

Useful operator commands:

```sh
docker compose -f deploy/compose.yaml ps
docker compose -f deploy/compose.yaml logs -f --tail=200 asgard traefik
docker compose -f deploy/compose.yaml exec asgard asgard doctor
docker compose -f deploy/compose.yaml exec asgard asgard admin reset-password --username ege
```

On the deployed VPS, the generated one-time password is root-readable at `/root/asgard-initial-password`. Remove it after signing in and resetting the password.

## Agent access

The MCP endpoint is `https://asgard.rousoftware.com/mcp`. Clients discover OAuth metadata at `/.well-known/oauth-protected-resource/mcp`, register dynamically, and ask the administrator to approve explicit Asgard scopes in the browser. Project source can be inspected and revision-safely updated with `project_source_get` and `project_source_update`; runtime environment selection remains available through `service_config_update`. The plugin manifest is under `plugins/asgard`, with its local marketplace entry in `.agents/plugins/marketplace.json`.

## Validation

```sh
go test ./...
npm --prefix web run typecheck
npm --prefix web audit --audit-level=high
docker compose -f deploy/compose.yaml config -q
```
