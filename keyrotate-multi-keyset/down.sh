#!/bin/sh
# Stops and removes containers, but keeps volumes (Postgres data, the
# shared rotation-metadata volume, and Terraform's local state/output)
# intact. Use up.sh to start again where you left off.
set -eu
cd "$(dirname "$0")"

docker compose down
