terraform {
  required_version = ">= 1.5.0"
  required_providers {
    local = {
      source  = "hashicorp/local"
      version = "~> 2.4"
    }
    # null_resource.trigger_rotation below needs this -- easy to miss
    # since only "local" was needed before this variant introduced a
    # resource whose entire purpose is running a provisioner.
    null = {
      source  = "hashicorp/null"
      version = "~> 3.2"
    }
  }
  # State lives on its own volume (mounted at /work/state), not on the
  # /shared volume the tfvars/output files use -- so the backend's
  # artifact viewer only ever shows the two files people actually care
  # about, and state (which can contain sensitive plan data in a real
  # deployment) stays out of the shared, browsable directory.
  backend "local" {
    path = "state/terraform.tfstate"
  }
}

variable "id" {
  description = "Stable logical id for the keyset itself -- NOT a per-key id, and it does not change on rotation. Written by the Go rotation service, but it identifies 'this application's keyset', the same way an aws_secretsmanager secret name doesn't change when you add a new version to it."
  type        = string
  default     = "unit_1_keyset"
}

variable "type" {
  description = "Key algorithm shared by every entry in the keyset. Fixed for this demo (AES-256-GCM) since crypto.go never varies it."
  type        = string
  default     = "AES-256-GCM"
}

variable "keys" {
  description = <<-EOT
    The FULL current keyset, one entry per key generation that still
    exists in Postgres -- which, per the "old keys are retired, never
    deleted" invariant, is every key that has ever been created. Go
    rewrites this whole list on every commit (see writeTerraformVars
    in rotation.go); it is not incrementally appended to by Terraform
    or by this file. Exactly one entry should have primary = true at
    any given time -- Postgres enforces that invariant (one_primary_key
    unique index), this variable just carries the resulting shape.

    Each entry carries its OWN `expiration` -- there is deliberately no
    separate top-level expiry variable on this file. This matches how
    a real keyset resource actually works: every key VERSION has its
    own creation/rotation schedule, not the whole keyset resource as
    one blob. local.is_due below finds the entry with primary = true
    and reads its expiration; nothing here needs a value that lives
    outside the keys list.
  EOT
  type = list(object({
    label      = string # RFC3339Nano timestamp the key was created, used as its version label
    expiration = string # RFC3339 instant THIS key is scheduled to be superseded
    length     = number # key length in bits (256 for AES-256)
    status     = string # "ENABLED" for every key that still exists -- nothing is ever soft-disabled in this demo, retired just means primary = false
    primary    = bool   # true for exactly one entry: the currently active key
  }))
  default = []
}

variable "output_path" {
  description = "Where to write the reactive output file. Defaults to the same shared volume the backend reads tfvars.json from, so both files are visible from one place."
  type        = string
  default     = "/shared/terraform-output/current-key-reference.json"
}

variable "trigger_url" {
  description = "Base URL of the Go backend's rotate-if-due endpoint."
  type        = string
  default     = "http://backend:8080"
}

variable "trigger_response_path" {
  description = "Where to save the raw response from the last rotate-if-due call, for debugging."
  type        = string
  default     = "/shared/last-trigger-response.json"
}

variable "now" {
  description = <<-EOT
    Current UTC instant, RFC3339, supplied by the caller (apply-loop.sh,
    via `date -u`) instead of Terraform's own timestamp() function.

    timestamp() is deliberately "impure" -- Terraform treats its result
    as unknown-until-apply specifically so it can't silently make plans
    non-reproducible. That's fine for an output, but local.is_due below
    feeds null_resource.trigger_rotation's `count`, and count/for_each
    arguments MUST be fully known at plan time. Using timestamp() there
    fails every single apply with "Invalid count argument" -- which is
    exactly why nothing was ever rotating. var.now is a plain string
    variable, so it's known at plan time like everything else in
    var.keys, and timecmp() against it works identically to before.
  EOT
  type        = string
}

# THIS is the actual due-check, done in Terraform's own language, not
# in the shell wrapper -- and now derived entirely from var.keys, with
# no separate top-level expiry variable. First find the entry with
# primary = true (there should be exactly one, or zero before the very
# first genesis key exists); then compare ITS expiration to the
# current instant.
#
# var.now carries the current UTC instant in, supplied by apply-loop.sh
# on every apply (see the var.now description above for why this isn't
# timestamp() directly). `timecmp()` (Terraform >= 1.5) compares two
# RFC3339 timestamps, returning -1, 0, or 1. The whole expression is
# wrapped in `try()` so an empty keys list (true only before genesis)
# evaluates to "not due" instead of erroring out of an out-of-bounds
# index or an invalid timecmp() call.
locals {
  primary_key = try([for k in var.keys : k if k.primary][0], null)
  is_due      = try(local.primary_key != null && timecmp(var.now, local.primary_key.expiration) >= 0, false)
}

# THIS is "Terraform triggers Go": a null_resource whose only job is
# to run a local-exec provisioner that calls the backend's
# rotate-if-due endpoint -- and it only exists at all when
# local.is_due is true.
#
# count = local.is_due ? 1 : 0 does two things at once: it's the gate
# (nothing fires when not due), and it's what makes this
# self-resetting. When a rotation succeeds, Go writes a NEW primary
# entry with a NEW, future expiration; the next `terraform apply` reads
# that new value, local.is_due flips back to false, and Terraform
# DESTROYS this resource (quietly -- no destroy-time provisioner is
# defined). That destroy is what re-arms the trigger: the next time the
# new primary's expiration is reached, is_due flips true again, count
# flips 0 -> 1, and creating the resource fires local-exec again. No
# external nonce is needed to force a re-run -- the create/destroy
# lifecycle itself is the pulse.
#
# triggers keys off the primary key's label (its creation timestamp,
# effectively its identity) rather than its expiration directly: once
# a NEW key becomes primary, its label always differs from the
# previous primary's label, which is a strictly more reliable signal
# than "expiration changed" (an edge case: two different primaries
# could theoretically compute the same expiration under extreme
# scheduling coincidence; they can never share a label).
#
# One honest caveat: if local-exec succeeds (curl gets an HTTP 200)
# but Go's response body says rotated: false for a reason that ISN'T
# reflected by the primary key changing -- e.g. tryRotate lost the
# advisory-lock race to another replica -- this resource is now
# considered "successfully created" with no diff to force a retry, so
# nothing retries automatically until the primary key next changes.
# With the single backend replica this compose stack runs, that's a
# near-impossible scenario (nothing else is contending for the lock);
# it would matter more if you scaled the backend service up.
#
# Ordering note, unchanged from before: this resource and
# local_file.current_keyset below have no dependency on each other, so
# local_file in THIS apply always reflects the vars passed in at
# invocation time -- the PREVIOUS cycle's tfvars.json -- never the
# result of the local-exec call that just ran in the same apply. See
# README, "The one-cycle lag this shape can't avoid."
resource "null_resource" "trigger_rotation" {
  count = local.is_due ? 1 : 0

  triggers = {
    primary_key_label = try(local.primary_key.label, "")
    # Retry safety net: var.now changes every poll, so as long as
    # is_due stays true this resource gets destroyed and recreated
    # (re-firing local-exec) on EVERY apply, not just once. Previously
    # this only keyed off primary_key_label, so a single call that
    # returned a truthful "rotated: false" (not-due-yet precision
    # race, lost advisory-lock, etc.) with no actual key change was
    # "successfully created" with nothing left to force a retry --
    # permanently wedging the pipeline until the primary key changed
    # by some other means, which it never would. See README/git log
    # for the incident this was written in response to.
    retry_tick        = var.now
  }

  provisioner "local-exec" {
    # -f: fail (nonzero exit) on HTTP error status, which fails this
    # provisioner, which taints this resource -- so a backend that's
    # down or erroring gets destroyed-and-recreated (i.e. retried) on
    # the very next apply, rather than silently swallowing the error.
    command = "curl -sf -X POST ${var.trigger_url}/api/rotate-if-due -o ${var.trigger_response_path}"
  }
}

# ---------------------------------------------------------------------------
# What a real deployment would use instead of local_file below. This block
# is intentionally commented out -- there is no real "ursa" provider, it's a
# stand-in name for "whatever secrets/KMS platform you actually deploy to"
# (AWS Secrets Manager, GCP KMS, Vault, etc. all have some equivalent of a
# named keyset with multiple enabled versions and one marked primary/active,
# each version carrying its own expiration). The point of writing var.keys
# in exactly this shape is that swapping this comment in for the local_file
# resource below is close to a drop-in change -- same variables, same three
# arguments, no change to Go or to apply-loop.sh required.
#
# resource "ursa_keyset" "unit_1_keyset" {
#   id   = var.id
#   type = var.type
#   keys = var.keys
# }
# ---------------------------------------------------------------------------

# Demo stand-in for the real resource above. Represents whatever real
# infrastructure needs to know about the current keyset. Terraform only
# ever reacts to key *metadata* (id/type/label/expiration/length/
# status/primary), never to raw key material, which always stays in
# Postgres.
#
# Deliberately does NOT include any timestamp()/uuid() call in its own
# content, unlike local.is_due above (which NEEDS a fresh timestamp()
# call every evaluation to do its job). This resource's job is just to
# mirror whatever var.keys currently is, so it should only show a diff
# when that actually changed.
resource "local_file" "current_keyset" {
  filename = var.output_path
  content = jsonencode({
    id   = var.id
    type = var.type
    keys = var.keys
  })
}

output "keyset_id" {
  value = var.id
}

output "key_count" {
  value = length(var.keys)
}

# Purely for visibility when running `terraform apply` / `terraform
# output` by hand -- shows whether THIS apply considered a rotation
# due, i.e. whether null_resource.trigger_rotation exists this round.
output "rotation_due_this_apply" {
  value = local.is_due
}

# Convenience output: the label and expiration of whichever entry is
# currently primary, or null if the keyset is still empty (before the
# first genesis key).
output "primary_key_label" {
  value = try(local.primary_key.label, null)
}

output "primary_key_expiration" {
  value = try(local.primary_key.expiration, null)
}
