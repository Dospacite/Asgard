---
name: asgard-deploy
description: Create a health-gated Asgard release for a project and monitor it to a terminal state.
---

Deploy a project to Asgard. Use the `asgard-deploy` skill as the operating procedure.

Target project: `$ARGUMENTS`. If that is empty, call `projects_list` and ask which project to deploy rather than guessing.

1. Call `project_get` on the target and `system_get` for host capacity. Confirm there is headroom before starting.
2. Report the current live release ID and health so there is a known rollback point on record.
3. If source or runtime configuration must change first, call `project_source_get` or `service_get`, preserve the returned `revision` / `configRevision`, and apply the smallest edit that achieves the request.
4. Create the deployment with a fresh, stable idempotency key.
5. Poll `operation_get` and `operation_logs_get` until the operation reaches a terminal state. Do not stop polling early and do not report success while it is queued or running.
6. On success, report the new release ID, route hostnames, health-gate result, and the prior release retained for rollback.
7. On failure, pull the operation logs, explain the specific gate or step that failed, and recommend either a fix or a rollback to the last successful release. Do not roll back without asking.

Never weaken the Compose safety contract to make a deployment pass, and never request delete scopes for this workflow.
