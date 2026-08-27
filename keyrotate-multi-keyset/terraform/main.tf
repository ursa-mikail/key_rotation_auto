terraform {
  required_version = ">= 1.5.0"
  required_providers {
    local = {
      source  = "hashicorp/local"
      version = "~> 2.4"
    }
    # null_resource.trigger_rotation below needs this -- the whole
    # point of this variant is a resource whose entire purpose is
    # running a provisioner.
    null = {
      source  = "hashicorp/null"
      version = "~> 3.2"
    }
  }
  # State lives on its own volume (mounted at /work/state), not on the
  # /shared volume the tfvars/output files use -- so the backend's
  # artifact viewer only ever shows the two files people actually care
  # about, and state stays out of the shared, browsable directory.
  backend "local" {
    path = "state/terraform.tfstate"
  }
}

variable "type" {
  description = "Key algorithm shared by every entry in every keyset. Fixed for this demo (AES-256-GCM) since crypto.go never varies it."
  type        = string
  default     = "AES-256-GCM"
}

variable "keysets" {
  description = <<-EOT
    The FULL current state of every keyset Go manages -- N of them by
    default (see KEYSET_COUNT), a random M of which (ROTATING_COUNT)
    are `rotating`, and a random L (0..M) of THOSE are additionally
    `revoked` (needed an immediate/emergency renewal at genesis).

    Go rewrites this whole list on every commit (see writeTerraformVars
    in rotation.go); it is not incrementally appended to by Terraform.
    Within each keyset, exactly one key entry should have primary =
    true at any given time -- Postgres enforces that invariant
    (one_primary_key_per_keyset unique index), this variable just
    carries the resulting shape.

    Each KEY entry carries its OWN `expiration` -- there is
    deliberately no top-level expiry variable anywhere on this file,
    matching how a real keyset resource works: every key VERSION has
    its own creation/rotation schedule. local.due_keyset_ids below
    finds, for EVERY keyset, the entry with primary = true and reads
    its expiration.
  EOT
  type = list(object({
    id       = string # stable logical id for this keyset, e.g. "unit_07_keyset" -- never changes across rotations
    type     = string
    rotating = optional(bool, false) # was this keyset one of the M randomly selected to actually rotate?
    revoked  = optional(bool, false) # was this keyset ALSO flagged for emergency renewal at genesis?
    keys = list(object({
      label      = string # RFC3339Nano timestamp the key was created, used as its version label
      expiration = string # RFC3339 instant THIS key is scheduled to be superseded
      length     = number # key length in bits (256 for AES-256)
      status     = string # "ENABLED" for every key that still exists
      primary    = bool   # true for exactly one entry per keyset: the currently active key
    }))
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
    fails every single apply with "Invalid count argument". var.now is
    a plain string variable, so it's known at plan time like everything
    else in var.keysets, and timecmp() against it works identically to
    before.
  EOT
  type        = string
}

# THIS is the actual due-check, done in Terraform's own language, not
# in the shell wrapper -- generalized across every keyset in
# var.keysets at once, instead of just one. For each keyset, find the
# entry with primary = true (there should be exactly one, or none
# before genesis has run); then compare ITS expiration to var.now.
# local.due_keyset_ids collects the ids of every keyset that's
# currently due -- could be zero, one, or many, since N independent
# keysets can each become due on their own random schedule.
locals {
  primary_of = {
    for ks in var.keysets : ks.id => try([for k in ks.keys : k if k.primary][0], null)
  }
  due_keyset_ids = [
    for ks in var.keysets : ks.id
    if try(local.primary_of[ks.id] != null && timecmp(var.now, local.primary_of[ks.id].expiration) >= 0, false)
  ]
  is_due = length(local.due_keyset_ids) > 0
}

# THIS is "Terraform triggers Go": a null_resource whose only job is
# to run a local-exec provisioner that calls the backend's
# rotate-if-due endpoint -- and it only exists at all when at least
# ONE keyset is due. Go itself decides (from Postgres's own clock)
# exactly WHICH of the (possibly many) due keysets to actually rotate
# in that one call -- this resource doesn't need to tell it which
# ones; "at least one is due" is the entire signal.
#
# count = local.is_due ? 1 : 0 is the gate. triggers include var.now
# (not just a joined due-id list) so this resource gets
# destroyed-and-recreated -- re-firing local-exec -- on EVERY apply
# where at least one keyset remains due, not just once. That matters
# more here than in a single-keyset version: with N independent random
# schedules, "still due" can easily mean "a DIFFERENT keyset became due
# in the meantime," and the retry_tick keeps every apply live instead
# of only the first one after due_ids changes.
resource "null_resource" "trigger_rotation" {
  count = local.is_due ? 1 : 0

  triggers = {
    due_ids    = join(",", local.due_keyset_ids)
    retry_tick = var.now
  }

  provisioner "local-exec" {
    # -f: fail (nonzero exit) on HTTP error status, which fails this
    # provisioner, which taints this resource -- so a backend that's
    # down or erroring gets destroyed-and-recreated (i.e. retried) on
    # the very next apply.
    command = "curl -sf -X POST ${var.trigger_url}/api/rotate-if-due -o ${var.trigger_response_path}"
  }
}

# ---------------------------------------------------------------------------
# What a real deployment would use instead of local_file below, ONE PER
# KEYSET via for_each. This block is intentionally commented out -- there
# is no real "ursa" provider, it's a stand-in name for "whatever
# secrets/KMS platform you actually deploy to" (AWS Secrets Manager, GCP
# KMS, Vault, etc. all have some equivalent of a named keyset with
# multiple enabled versions and one marked primary/active). Writing
# var.keysets in exactly this shape is what makes swapping this comment
# in for the local_file resource below close to a drop-in change.
#
# resource "ursa_keyset" "keyset" {
#   for_each = { for ks in var.keysets : ks.id => ks }
#   id       = each.value.id
#   type     = each.value.type
#   keys     = each.value.keys
# }
# ---------------------------------------------------------------------------

# Demo stand-in for the real resource above. Represents whatever real
# infrastructure needs to know about the current state of every
# keyset. Terraform only ever reacts to key *metadata*, never to raw
# key material, which always stays in Postgres.
resource "local_file" "current_keyset" {
  filename = var.output_path
  content = jsonencode({
    keysets = var.keysets
  })
}

output "keyset_count" {
  value = length(var.keysets)
}

output "rotating_keyset_count" {
  value = length([for ks in var.keysets : ks.id if ks.rotating])
}

# Purely for visibility when running `terraform apply` / `terraform
# output` by hand -- shows whether THIS apply considered any keyset
# due, i.e. whether null_resource.trigger_rotation exists this round.
output "rotation_due_this_apply" {
  value = local.is_due
}

# Which keyset id(s) this apply considered due. Usually empty (most
# polls find nothing due) or a single id; can be several at once when
# more than one of the N independent random schedules lines up.
output "due_keyset_ids" {
  value = local.due_keyset_ids
}
