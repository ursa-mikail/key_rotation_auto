terraform {
  required_version = ">= 1.5.0"
  required_providers {
    local = {
      source  = "hashicorp/local"
      version = "~> 2.4"
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
  EOT
  type = list(object({
    label   = string # RFC3339Nano timestamp the key was created, used as its version label
    length  = number # key length in bits (256 for AES-256)
    status  = string # "ENABLED" for every key that still exists -- nothing is ever soft-disabled in this demo, retired just means primary = false
    primary = bool   # true for exactly one entry: the currently active key
  }))
  default = []
}

variable "output_path" {
  description = "Where to write the reactive output file. Defaults to the same shared volume the backend reads tfvars.json from, so both files are visible from one place."
  type        = string
  default     = "/shared/terraform-output/current-key-reference.json"
}

# ---------------------------------------------------------------------------
# What a real deployment would use instead of local_file below. This block
# is intentionally commented out -- there is no real "ursa" provider, it's a
# stand-in name for "whatever secrets/KMS platform you actually deploy to"
# (AWS Secrets Manager, GCP KMS, Vault, etc. all have some equivalent of a
# named keyset with multiple enabled versions and one marked primary/active).
# The point of writing var.keys in exactly this shape is that swapping this
# comment in for the local_file resource below is close to a drop-in change
# -- same variables, same three arguments, no change to Go or to
# apply-loop.sh required.
#
# resource "ursa_keyset" "unit_1_keyset" {
#   id   = var.id
#   type = var.type
#   keys = var.keys
# }
# ---------------------------------------------------------------------------

# Demo stand-in for the real resource above. Represents whatever real
# infrastructure needs to know about the current keyset. Terraform only
# ever reacts to key *metadata* (id/type/label/length/status/primary),
# never to raw key material, which always stays in Postgres.
#
# Deliberately does NOT include any timestamp()/uuid() call in the
# content -- the file's content is a pure function of the input
# variables, so identical variables always produce an identical file
# and `terraform plan` correctly reports "no changes" on a re-apply.
# That determinism is what makes apply-loop.sh's hash-compare gate a
# true idempotency check rather than a rough approximation, and it's
# unaffected by var.keys being a list now instead of three scalars --
# see the README's "Idempotency with a list-shaped variable" section
# for why a growing list doesn't break that guarantee.
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

# Convenience output: the label of whichever entry is currently primary,
# or null if the keyset is still empty (before the first genesis key).
output "primary_key_label" {
  value = try([for k in var.keys : k.label if k.primary][0], null)
}
