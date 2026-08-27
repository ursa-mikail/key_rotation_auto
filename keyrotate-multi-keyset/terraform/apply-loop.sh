#!/bin/sh
# This script's job shrank on purpose: it no longer reads expires_at
# out of the json or does any time comparison itself. It just runs
# `terraform apply` on a fixed cadence, forever, and Terraform's own
# HCL (local.is_due in main.tf, using timecmp() against timestamp())
# decides whether anything actually happens on any given apply -- that
# is the entire point of moving the due-check into Terraform.
#
# A `terraform apply` where nothing is due (local.is_due == false,
# and var.keys/var.expires_at are unchanged from last time) is cheap:
# the plan comes back empty and apply is a fast no-op. Polling every
# second is fine for a 3-20s expiry range; see README for the timing
# argument if you widen that range.
set -eu

SHARED_DIR="${SHARED_DIR:-/shared}"
VARS_FILE="$SHARED_DIR/rotation.auto.tfvars.json"
POLL_SECONDS="${POLL_SECONDS:-1}"

mkdir -p "$SHARED_DIR"

log() {
  printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$1"
}

# Runtime sanity check, separate from the `terraform validate` that
# already ran at image build time (see Dockerfile) -- this catches
# drift between the build environment and the running container (e.g.
# a volume mount problem) rather than an HCL error, which the build
# step already ruled out. Failing loudly here, once, at startup, beats
# discovering the same problem only after watching silent no-op applies
# for a while.
log "terraform-runner: startup validate..."
if terraform -chdir=/work validate; then
  log "startup validate: OK"
else
  log "startup validate: FAILED -- the container's terraform config is broken; every apply below will fail the same way. This should never happen if the image built successfully (build-time validate would have caught it) -- if you see this, something changed the mounted config after build, or the state volume is corrupted."
fi

log "terraform-runner: applying every ${POLL_SECONDS}s; Terraform itself decides whether a rotation is due"

while true; do
  if [ -f "$VARS_FILE" ]; then
    NOW="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    if terraform -chdir=/work apply -auto-approve -var-file="$VARS_FILE" -var="now=$NOW"; then
      # Quiet on a normal no-op apply (the common case -- most polls
      # find nothing due) but explicitly log the cycles where the
      # trigger actually fired, so `docker compose logs -f
      # terraform-runner` gives real confirmation of activity instead
      # of either total silence or noise on every single poll.
      #
      # `-json`, not `-raw`: rotation_due_this_apply is a bool output,
      # and `terraform output -raw` only supports string-typed outputs
      # -- it errors on bool/number/null. `-json` on a single named
      # output prints just that value's JSON representation (`true` or
      # `false`), which is safe to string-compare directly.
      DUE=$(terraform -chdir=/work output -json rotation_due_this_apply 2>/dev/null || echo "null")
      if [ "$DUE" = "true" ]; then
        IDS=$(terraform -chdir=/work output -json due_keyset_ids 2>/dev/null || echo "[]")
        log "trigger fired this cycle for keyset(s): $IDS -- see /shared/last-trigger-response.json for what Go returned"
      fi
    else
      log "apply FAILED -- will retry next poll (full Terraform error output is above this line)"
    fi
  else
    log "waiting for $VARS_FILE to appear (no genesis key created yet?)"
  fi
  sleep "$POLL_SECONDS"
done
