---
name: asgard-deploy
description: Deploy, operate, monitor, roll back, back up, restore, or safely delete projects and containers through the Asgard MCP server. Use when Codex is asked to inspect Asgard state, import a project, create or watch a release, change runtime configuration, manage container lifecycle, or perform an exact confirmed deletion.
---

# Deploy with Asgard

Use the Asgard MCP tools as the source of truth. Inspect before mutating.

## Workflow

1. Call `system_get`, then `projects_list` or `project_get`. Confirm host capacity and current state.
2. Before import, call `compose_contract_get`. Validate that the app uses only the declared safe subset.
3. Import with `project_import_git` or `project_import_image`. Report validation errors exactly; do not weaken the contract.
4. For Compose, Dockerfile, or `.env` changes, call `project_source_get`, preserve the target file `revision`, and apply the smallest edit with `project_source_update`. Compose edits are validated against the safe contract and existing runtime overrides are preserved.
5. Before runtime configuration changes, call `service_get` and preserve its `configRevision`. Choose a complete environment map and apply it with `service_config_update`; values shown in `.env` are candidates until explicitly selected for a service.
6. Inspect `network_topology_get` before changing connectivity. Projects are isolated by default; create or reuse a shared network only for an explicit private dependency.
7. Use `network_service_attach` with a stable, descriptive alias. The default `<project>--<service>` alias is preferred because it avoids collisions. Applications connect to `http://<alias>:<container-port>`, never the public hostname, for private traffic.
8. Verify both endpoints with `networks_list`. Use `network_reconcile` after a container replacement or unexpected disconnect; persisted memberships are also restored automatically during deployment and control-plane startup.
9. Create deployments with a fresh stable idempotency key. Poll `operation_get` and `operation_logs_get` until a terminal state.
10. If a release fails health gates, inspect its logs. Roll back only to a successful release and monitor the rollback operation to completion.
11. Use the least OAuth scope that covers the work. Network and source changes require `asgard:configure`; never request delete access for ordinary deployment work.

## Network boundaries

- `project` networks provide automatic service-name discovery only inside one project.
- `shared` networks explicitly connect selected services across projects. Membership persists across releases.
- `edge` is reserved for public Traefik ingress; do not use it for application-to-application discovery.
- An internal shared network has no external gateway of its own. Other attached networks may still provide a service with egress.

## Destructive operations

Call `deletion_preview` first. Present the exact project, container, or empty shared network; source removal, service count, and retained named volumes where applicable. Disconnect every service before previewing network deletion. Call `deletion_confirm` only after explicit user confirmation and pass the unmodified short-lived token. Never infer confirmation from a deployment or cleanup request.

## Completion

Return the live release/operation ID, route hostnames, health result, and any retained rollback or backup point. Do not claim success while an operation is queued or running.
