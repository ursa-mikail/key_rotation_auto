terraform {
  required_version = ">= 1.5.0"
  required_providers {
    local = {
      source  = "hashicorp/local"
      version = "~> 2.4"
    }
    null = {
      source  = "hashicorp/null"
      version = "~> 3.2"
    }
  }
  backend "local" {
    path = "state/terraform.tfstate"
  }
}

variable "type" {
  description = "Key algorithm shared by every entry in every keyset. Fixed for this demo (AES-256-GCM)."
  type        = string
  default     = "AES-256-GCM"
}

variable "keysets" {
  description = <<-EOT
    The FULL current state of every keyset Go manages. Every keyset
    rotates on its own random interval in this variant -- there is no
    static tier. Independently of that timer, a background process in
    Go can REVOKE any still-active keyset at any moment; what happens
    next depends on the live REVOKE_AUTO_ROTATE toggle:
      - true  : the keyset is immediately, emergency-rotated and
                resumes normal cycling (`terminated` stays false).
      - false : the keyset is permanently halted (`terminated` becomes
                true, and it never appears in `keys[].primary`'s
                expiration moving again -- its last key's entry is
                simply frozen as of the moment it was halted).

    `terminated`, `last_outcome`, and `last_trigger` are denormalized
    onto every snapshot Go writes, in real time, specifically so a
    reader of this file (or the live-rendered HCL in the dashboard) can
    see "revoked, then rotated" vs "revoked, then terminated" vs an
    ordinary scheduled "rotated" WITHOUT cross-referencing a separate
    event log.
  EOT
  type = list(object({
    id           = string
    type         = string
    terminated   = optional(bool, false)
    last_outcome = optional(string, "")  # "" | "rotated" | "revoked_rotated" | "revoked_terminated" | "failed"
    last_trigger = optional(string, "")  # "" | "timer" | "revoke"
    keys = list(object({
      label      = string
      expiration = string
      length     = number
      status     = string
      primary    = bool
    }))
  }))
  default = []
}

variable "output_path" {
  type    = string
  default = "/shared/terraform-output/current-key-reference.json"
}

variable "trigger_url" {
  type    = string
  default = "http://backend:8080"
}

variable "trigger_response_path" {
  type    = string
  default = "/shared/last-trigger-response.json"
}

variable "now" {
  description = <<-EOT
    Current UTC instant, RFC3339, supplied by apply-loop.sh (`date -u`)
    instead of Terraform's own timestamp() -- see the identical note in
    the N/M/L variant's main.tf for why timestamp() can't be used for a
    count/for_each argument.
  EOT
  type        = string
}

# For each keyset that is NOT terminated, find its primary key entry
# and compare its expiration to var.now. Terminated keysets are
# excluded here on purpose: once Go halts a keyset, its last primary
# key's `expiration` stays frozen in the past forever, and without this
# filter Terraform would believe that keyset is "due" on literally
# every future apply, firing a pointless rotate-if-due call forever.
# Excluding it here is what makes "halt" actually mean "Terraform stops
# asking about this keyset too," not just "Go stops acting on it."
locals {
  active_keysets = [for ks in var.keysets : ks if !ks.terminated]
  primary_of = {
    for ks in local.active_keysets : ks.id => try([for k in ks.keys : k if k.primary][0], null)
  }
  due_keyset_ids = [
    for ks in local.active_keysets : ks.id
    if try(local.primary_of[ks.id] != null && timecmp(var.now, local.primary_of[ks.id].expiration) >= 0, false)
  ]
  is_due = length(local.due_keyset_ids) > 0
}

# Fires rotate-if-due whenever at least one non-terminated keyset is
# due. Go itself re-derives exactly which keyset(s) to actually rotate
# from Postgres's own clock -- this resource's only job is "at least
# one might be due, go check."
resource "null_resource" "trigger_rotation" {
  count = local.is_due ? 1 : 0

  triggers = {
    due_ids    = join(",", local.due_keyset_ids)
    retry_tick = var.now
  }

  provisioner "local-exec" {
    command = "curl -sf -X POST ${var.trigger_url}/api/rotate-if-due -o ${var.trigger_response_path}"
  }
}

# ---------------------------------------------------------------------------
# What a real deployment would use instead of local_file below, ONE PER
# KEYSET via for_each -- see the identical note in the N/M/L variant's
# main.tf. "ursa" is a stand-in provider name for whatever real
# secrets/KMS platform is actually in use.
#
# resource "ursa_keyset" "keyset" {
#   for_each   = { for ks in var.keysets : ks.id => ks }
#   id         = each.value.id
#   type       = each.value.type
#   terminated = each.value.terminated
#   keys       = each.value.keys
# }
# ---------------------------------------------------------------------------

resource "local_file" "current_keyset" {
  filename = var.output_path
  content = jsonencode({
    keysets = var.keysets
  })
}

output "keyset_count" {
  value = length(var.keysets)
}

output "terminated_keyset_count" {
  value = length([for ks in var.keysets : ks.id if ks.terminated])
}

output "rotation_due_this_apply" {
  value = local.is_due
}

output "due_keyset_ids" {
  value = local.due_keyset_ids
}
