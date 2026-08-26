#!/bin/sh
# Watches the tfvars file the Go backend writes on every successful key
# rotation and applies it with Terraform -- but only when its content
# actually changed. This is the idempotency boundary between the 1s
# rotation ticker and Terraform: rotation attempts that don't change
# anything (not due yet, lock lost to another replica, same time
# window already handled, liveness test failed) never even produce a
# new tfvars file, and rotations that DO produce one but with
# byte-identical content (shouldn't normally happen, but defence in
# depth) are also skipped here via the hash compare.
set -eu

SHARED_DIR="${SHARED_DIR:-/shared}"
VARS_FILE="$SHARED_DIR/rotation.auto.tfvars.json"
# The applied-hash marker lives on the SAME shared volume as the tfvars
# file (not in the container's own filesystem), so the backend can
# read it too and tell the UI whether the output file is in sync with
# the latest rotation, without any extra plumbing between containers.
HASH_FILE="$SHARED_DIR/.last-applied-hash"
POLL_SECONDS="${POLL_SECONDS:-2}"

mkdir -p "$SHARED_DIR"

log() {
  printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$1"
}

log "terraform-runner: watching $VARS_FILE every ${POLL_SECONDS}s"

while true; do
  if [ -f "$VARS_FILE" ]; then
    CURRENT_HASH=$(sha256sum "$VARS_FILE" | awk '{print $1}')
    PREV_HASH=""
    if [ -f "$HASH_FILE" ]; then
      PREV_HASH=$(cat "$HASH_FILE")
    fi

    if [ "$CURRENT_HASH" != "$PREV_HASH" ]; then
      log "rotation.auto.tfvars.json changed (hash $CURRENT_HASH) -- applying"
      if terraform -chdir=/work apply -auto-approve -var-file="$VARS_FILE"; then
        # Only record the hash as "applied" on success. A failed apply
        # leaves the old hash in place, so the very next poll retries
        # it automatically instead of silently drifting out of sync.
        echo "$CURRENT_HASH" > "$HASH_FILE"
        log "apply succeeded"
      else
        log "apply FAILED -- will retry next poll, hash not advanced"
      fi
    fi
  else
    log "waiting for $VARS_FILE to appear (no genesis key created yet?)"
  fi
  sleep "$POLL_SECONDS"
done
