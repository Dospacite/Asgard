---
name: asgard-deploy
description: Deploy, operate, monitor, roll back, back up, restore, or safely delete projects and containers through the Asgard MCP server. Use when asked to inspect Asgard state, import a project, create or watch a release, change runtime configuration, manage container lifecycle, or perform an exact confirmed deletion.
---

# Deploy with Asgard

Use the Asgard MCP tools as the source of truth. Inspect before mutating.

## Workflow

1. Call `system_get`, then `projects_list` or `project_get`. Confirm host capacity and current state.
2. Before import, call `compose_contract_get`. Read the warning list as carefully as the applied fields: a key under `ignored` is accepted, but the platform is not honoring it, and the workload must not depend on it. Only the `rejected` keys fail an import — do not work around them by editing a file into something the platform cannot police.
3. Import with `project_import_git` or `project_import_image`. For a private repository, call `git_credentials_list` and pass the matching `credentialId`; store a new one with `git_credential_create` only when the user supplies the secret. Never place a token or key in the repository URL. Report validation errors and warnings exactly; do not weaken the contract.
4. For Compose or Dockerfile changes, call `project_source_get`, preserve the target file `revision`, and apply the smallest edit with `project_source_update`. Compose edits are validated against the safe contract and existing runtime overrides are preserved. To change `.env` values, use `project_env_update` with `set`/`unset`: it edits only the named variables and applies them to every service that reads the file. Secret-looking values come back as `[redacted]`; never send a redacted value back.
5. Before runtime configuration changes, call `service_get` and preserve its `configRevision`. `service_config_update` changes only the fields you send; use `environmentSet`/`environmentUnset` for single variables. A value set this way is an override that later Compose saves and re-syncs keep, so prefer `project_env_update` when the value belongs in the project's `.env`.
6. Inspect `network_topology_get` before changing connectivity. Projects are isolated by default; create or reuse a shared network only for an explicit private dependency.
7. Use `network_service_attach` with a stable, descriptive alias. The default `<project>--<service>` alias is preferred because it avoids collisions. Applications connect to `http://<alias>:<container-port>`, never the public hostname, for private traffic.
8. Verify both endpoints with `networks_list`. Use `network_reconcile` after a container replacement or unexpected disconnect; persisted memberships are also restored automatically during deployment and control-plane startup.
9. For a Git project, call `project_source_resync` before deploying whenever new commits should go live. Deployments build the project's stored working tree and never fetch on their own, so without a re-sync the build reuses its cached layers and republishes the commit already captured — a green release that ships nothing new. Confirm the returned `commit` is the one you expect, and read `reconcile`: `validation` only previews the file, while `reconcile` says which services' stored configuration changed and which file values an override held back (`keptOverrides`). Then deploy.
10. Create deployments with a fresh stable idempotency key. Poll `operation_get` and `operation_logs_get` until a terminal state. The final summary names the services the release recreated; one that says nothing changed means no container picked up your change. The gate waits for each service's own healthcheck, whose `start_period` and `retries × interval` set the budget; a service with `restart: "no"` is a job and must exit zero before its dependents start; a public service must answer its health path below 500.
11. If a release fails health gates, inspect its logs. Roll back only to a successful release and monitor the rollback operation to completion. A rollback recreates the release from its own snapshot, container configuration included.
12. Archives (`.zip`, `.tar`, `.tar.gz`, `.tar.bz2`, `.tar.xz`, `.tar.zst`) are uploaded through the control-plane UI or `POST /api/v1/imports/archive`; there is no MCP upload tool, so direct the user there when the source is a local archive.
13. Use the least OAuth scope that covers the work. Network and source changes require `asgard:configure`; never request delete access for ordinary deployment work.

## Network boundaries

- `project` networks provide automatic service-name discovery only inside one project.
- `shared` networks explicitly connect selected services across projects. Membership persists across releases.
- `edge` is reserved for public Traefik ingress; do not use it for application-to-application discovery.
- An internal shared network has no external gateway of its own. Other attached networks may still provide a service with egress.

## Destructive operations

Call `deletion_preview` first. Present the exact project, container, or empty shared network; source removal, service count, and retained named volumes where applicable. Disconnect every service before previewing network deletion. Call `deletion_confirm` only after explicit user confirmation and pass the unmodified short-lived token. Never infer confirmation from a deployment or cleanup request.

## Completion

Return the live release/operation ID, route hostnames, health result, and any retained rollback or backup point. Do not claim success while an operation is queued or running.
