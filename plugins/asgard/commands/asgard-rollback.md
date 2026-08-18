---
name: asgard-rollback
description: Roll an Asgard project back to its last known-good release and monitor the rollback to completion.
---

Roll a project back to a previously successful release.

Target project: `$ARGUMENTS`. If that is empty, call `projects_list` and ask which project to roll back.

1. Call `project_get` and list the release history with each release's status and health result.
2. Identify the current live release and the most recent release that both succeeded and passed its health gate. Never target a failed release.
3. Show the user exactly what is changing — project, from-release, to-release, and the hostnames affected — and get explicit confirmation before proceeding.
4. Execute the rollback with a fresh idempotency key.
5. Poll `operation_get` and `operation_logs_get` until terminal. Report the final release, health result, and route state.

A rollback replaces running containers but does not restore volume contents. If the failed release ran a destructive data migration, say so before rolling back and recommend `backups` review first — a rollback alone will not undo schema or data changes.
