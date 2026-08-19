#!/bin/sh
# Renders Traefik's static and control-plane configuration from deploy/.env into
# the shared data volume.
#
# Nothing in the repository carries a particular deployment's domain or contact
# address: a self-hosted install sets deploy/.env and gets a correct Traefik
# configuration without editing tracked files.
set -eu

: "${ASGARD_PUBLIC_URL:?set ASGARD_PUBLIC_URL in deploy/.env}"
: "${ASGARD_DOMAIN:?set ASGARD_DOMAIN in deploy/.env}"
: "${ASGARD_ACME_EMAIL:?set ASGARD_ACME_EMAIL in deploy/.env - Let\'s Encrypt requires a contact address}"

# The control plane answers on the host of its own public URL unless told
# otherwise.
control_hostname="${ASGARD_CONTROL_HOSTNAME:-}"
if [ -z "$control_hostname" ]; then
  control_hostname=$(printf '%s' "$ASGARD_PUBLIC_URL" | sed -e 's|^[a-z][a-z0-9+.-]*://||' -e 's|[:/].*$||')
fi
if [ -z "$control_hostname" ]; then
  echo "could not derive a control-plane hostname from ASGARD_PUBLIC_URL=$ASGARD_PUBLIC_URL" >&2
  exit 1
fi

# includeSubDomains and preload are only safe on a name whose entire subtree
# belongs to this install. That is true when the control plane is the apex of
# its own wildcard zone, or sits inside it — every name underneath is Asgard's
# to commit. It is not true when the control plane has been put on some other
# domain, where those directives would reach subdomains Asgard never serves and
# preload-list removal takes months.
#
# This must stay in step with proxy.InControlPlaneZone, which decides the same
# question for workload routes.
hsts_subdomains=false
hsts_preload=false
case "$control_hostname" in
  "$ASGARD_DOMAIN"|*".$ASGARD_DOMAIN") hsts_subdomains=true; hsts_preload=true ;;
esac

mkdir -p /data/traefik/dynamic
sed -e "s|__CONTROL_HOSTNAME__|$control_hostname|g" \
    -e "s|__HSTS_SUBDOMAINS__|$hsts_subdomains|g" \
    -e "s|__HSTS_PRELOAD__|$hsts_preload|g" \
    /config/control-plane.yml.template > /data/traefik/dynamic/control-plane.yml
sed -e "s|__ACME_EMAIL__|$ASGARD_ACME_EMAIL|g" \
    /config/traefik.yaml.template > /data/traefik/traefik.yml
chmod 0644 /data/traefik/dynamic/control-plane.yml /data/traefik/traefik.yml

echo "control plane: $control_hostname (HSTS includeSubDomains=$hsts_subdomains, preload=$hsts_preload)"
echo "workload zone: *.$ASGARD_DOMAIN"
