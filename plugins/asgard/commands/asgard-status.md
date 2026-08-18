---
name: asgard-status
description: Survey the Asgard control plane and report host capacity, project health, and any in-flight operations.
---

Report the current state of the Asgard control plane. Read only — do not mutate anything.

If `$ARGUMENTS` names a project, scope the report to that project. Otherwise cover every project.

1. Call `system_get` for host capacity and control-plane version.
2. Call `projects_list`, then `project_get` for each project in scope.
3. For every service, note its role, live release, health result, and public hostname.
4. Surface any operation that is still queued or running, with its ID and current stage.
5. Call `networks_list` and flag any service whose persisted shared-network membership is not currently connected.

Present a compact table of project, service, release, health, and hostname, then list problems worth acting on — unhealthy services, stalled operations, drifted network memberships, or capacity headroom below roughly 20%. State plainly when nothing needs attention.

Do not import, deploy, reconfigure, or delete anything from this command. If the state suggests an action, recommend it and stop.
