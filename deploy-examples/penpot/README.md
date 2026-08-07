# Penpot on Asgard

This is an Asgard-safe adaptation of Penpot's official Docker Compose stack:

https://help.penpot.app/technical-guide/getting-started/docker/

The project-private network is supplied by Asgard, so upstream `networks` fields are
omitted. YAML extension anchors and `stop_signal` are expanded or omitted because they
are outside Asgard's safe Compose contract. The temporary mail catcher is replaced by
Penpot's `enable-log-emails` mode.

Before the first deployment, replace `PENPOT_SECRET_KEY` and every database password
through Asgard's service configuration. Do not deploy the placeholder values.
