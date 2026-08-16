---
name: asgard-develop
description: Prepare an application for reliable deployment on Asgard. Use when creating or reviewing a Dockerfile, safe Docker Compose file, health endpoint, runtime limits, graceful shutdown, structured logs, persistent volumes, environment configuration, or x-asgard routing metadata.
---

# Develop for Asgard

Prepare the repository as a portable, health-gated container application.

## Inspect

Identify processes, listening ports, build commands, runtime commands, stateful paths, required environment values, shutdown behavior, and every service-to-service dependency. Call the Asgard `compose_contract_get` tool when available; its returned contract overrides assumptions.

## Build contract

- Provide a multi-stage Dockerfile with a pinned runtime base, non-root user when practical, `.dockerignore`, and no credentials in layers.
- Listen on `0.0.0.0`, read the port from environment, and write logs to stdout/stderr.
- Implement a lightweight HTTP health endpoint that verifies readiness without mutating state.
- Handle SIGTERM, stop accepting work, and exit within the container grace period.
- Put durable data in declared named volumes. Never bind the Docker socket, host paths outside the project, devices, host networking, or privileged mode. A path the project itself ships, such as `./config/app.json`, may be mounted read-only; Asgard serves it from the project's own imported source.
- Keep secrets out of Compose. Declare non-secret defaults explicitly and document every required value. `env_file` is supported and its values sit underneath any inline `environment` entry, matching Compose precedence.
- Make dependency URLs configurable. For a cross-project dependency, accept an internal URL such as `http://<project>--<service>:<container-port>` through environment rather than hard-coding a public hostname.
- Do not publish a service merely to let another project call it. Use an Asgard shared network and private DNS alias at deployment time.

## Compose metadata

Use top-level `x-asgard.primary-service`. Under `x-asgard.services.<name>`, declare `role` (`web`, `worker`, or `stateful`), `public`, internal `port`, and `health-path`. Public primary services receive `<project>.asgard.rousoftware.com`; additional public services receive `<service>--<project>.asgard.rousoftware.com`.

Compose `depends_on` and service-name DNS cover dependencies inside one project. Cross-project networks are an Asgard deployment concern: document the source service, destination service, container port, preferred alias, and whether the shared bridge needs its own external gateway. Do not add unsupported Compose network directives unless `compose_contract_get` explicitly permits them.

## Verify

Build locally, start the Compose project, exercise health and graceful shutdown, check logs for secrets, validate persistence across recreation, and test dependency calls using the same configurable internal URL shape. Then validate against Asgard's contract. Do not declare the app deployment-ready while any check is untested or failing.
