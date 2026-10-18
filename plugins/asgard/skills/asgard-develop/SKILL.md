---
name: asgard-develop
description: Prepare an application for reliable deployment on Asgard. Use when creating or reviewing a Dockerfile, safe Docker Compose file, health endpoint, runtime limits, graceful shutdown, structured logs, persistent volumes, environment configuration, or x-asgard routing metadata.
---

# Develop for Asgard

Prepare the repository as a portable, health-gated container application.

## Inspect

Identify processes, listening ports, build commands, runtime commands, stateful paths, required environment values, shutdown behavior, and every service-to-service dependency. Call the Asgard `compose_contract_get` tool when available; its returned contract overrides assumptions.

## Build contract

- Provide a multi-stage Dockerfile with a pinned runtime base, non-root user when practical, `.dockerignore`, and no credentials in layers. Builds run on the host's BuildKit, and the build cache survives between releases, so order the Dockerfile for it: copy dependency manifests and install before copying source, and use `RUN --mount=type=cache` for package-manager and compiler caches (for example the pnpm store or `.next/cache`). A `# syntax=` directive works; base images must be pullable anonymously. `build.target` picks a stage and `build.args` are passed through. A string `command`/`entrypoint` is split into an argv the way Compose splits it — write a list when an argument contains spaces, and a shell explicitly (`["sh","-c","…"]`) when you want one.
- Listen on `0.0.0.0`, read the port from environment, and write logs to stdout/stderr.
- Implement a lightweight HTTP health endpoint that verifies readiness without mutating state, and declare the same check in `healthcheck:` — Asgard applies a declared healthcheck to the container, and its `start_period` and `retries × interval` also set how long the release waits before failing. A service with no healthcheck is only waited on for five seconds of stability.
- Handle SIGTERM, stop accepting work, and exit within the container grace period. `stop_grace_period`, `stop_signal`, `read_only`, `tmpfs`, `sysctls`, `ulimits`, and `shm_size` are applied when declared.
- Put durable data in declared named volumes. Never bind the Docker socket, host paths outside the project, devices, host networking, or privileged mode. A path the project itself ships, such as `./config/app.json`, may be mounted read-only; Asgard serves it from the project's own imported source, and a data *directory* the project does not ship — `./data:/data` — is created in that source tree at deploy. A missing *file* is rejected: ship it.
- Keep secret *values* out of Compose. A file the project ships can be declared as a Compose secret or config and is served where Compose mounts it (`/run/secrets/<name>`, `/<name>`); a secret from the host's store or from the shell that ran the container is refused, so keep the value in the project's `.env` file and interpolate it, or mount the file. Declare non-secret defaults explicitly and document every required value. `env_file` is supported and its values sit underneath any inline `environment` entry, matching Compose precedence. A bare `FOO` entry, which Compose fills from the shell that ran it, is reported and left unset — Asgard never exposes its own environment to a workload.
- Declare resource ceilings the way your tooling already writes them: `deploy.resources.limits.{cpus,memory,pids}` or `mem_limit`/`cpus`/`pids_limit`. Asgard applies them as the service's limits, so a compose file that carries its own budgets needs no separate configuration.
- Make dependency URLs configurable. For a cross-project dependency, accept an internal URL such as `http://<project>--<service>:<container-port>` through environment rather than hard-coding a public hostname.
- Do not publish a service merely to let another project call it. Use an Asgard shared network and private DNS alias at deployment time. A published host port is never used: Asgard routes to the container port from `ports` or `expose`.
- Write relative paths as Compose defines them: the build context, `env_file` entries, project-relative volume sources, and the `.env` file all resolve against the directory holding the Compose file, so `deploy/compose.yaml` mounting `./nginx.conf` means `deploy/nginx.conf`.

## Compose metadata

Use top-level `x-asgard.primary-service`. Under `x-asgard.services.<name>`, declare `role` (`web`, `worker`, or `stateful`), `public`, internal `port`, and `health-path`. Public primary services receive `<project>.<ASGARD_DOMAIN>`; additional public services receive `<service>--<project>.<ASGARD_DOMAIN>`, where `ASGARD_DOMAIN` is the wildcard zone the control plane is configured with. A service may also set an explicit `hostname` for any fully qualified name whose DNS points at the host. A misspelled key here is an error rather than a silent default.

A service whose job is to finish — a migration, a bucket bootstrap, a seed — is written the ordinary way: `restart: "no"` with `depends_on: {condition: service_completed_successfully}` on whatever needs it. Asgard waits for it to exit and treats a zero exit as success, so dependents are not started until it is done. Give it a healthcheck only if it is really a server.

Keys with no Asgard equivalent (`container_name`, `profiles`, `logging`, `init`, `tty`, `dns`, `links`, `pull_policy`, …) are accepted and reported as import warnings; read them, because the platform is not keeping that promise. Keys that would leave the sandbox (`privileged`, `network_mode`, `devices`, `cap_add`, `extra_hosts`, `volumes_from`, `extends`) are refused, as is a secret or config that is not a file the project ships. `x-*` extension keys and YAML anchors with `<<:` merge keys work everywhere.

Compose `depends_on` and service-name DNS cover dependencies inside one project. Cross-project networks are an Asgard deployment concern: document the source service, destination service, container port, preferred alias, and whether the shared bridge needs its own external gateway. Compose's own `networks` memberships in the project's networks are satisfied by the project network; a membership in an external network is reported and not attached. `aliases` are honored.

## Verify

Build locally, start the Compose project, exercise health and graceful shutdown, check logs for secrets, validate persistence across recreation, and test dependency calls using the same configurable internal URL shape. Then validate against Asgard's contract. Do not declare the app deployment-ready while any check is untested or failing.
