#!/bin/sh
# Full teardown: stops containers AND deletes volumes, meaning all key
# history, rotation events, and Terraform state/output are wiped. Use
# this when you want a genuinely fresh start (e.g. before a demo), not
# for routine stop/start cycles -- that's what down.sh / up.sh are for.
set -eu
cd "$(dirname "$0")"

echo "This will permanently delete all Postgres data, rotation history,"
echo "and Terraform state for this stack."
printf "Type 'yes' to continue: "
read -r CONFIRM
if [ "$CONFIRM" != "yes" ]; then
  echo "Aborted."
  exit 1
fi

docker compose down -v --remove-orphans
docker image prune -f --filter "label=com.docker.compose.project=$(basename "$(pwd)")"

echo "Clean. Run ./up.sh for a fresh start."
