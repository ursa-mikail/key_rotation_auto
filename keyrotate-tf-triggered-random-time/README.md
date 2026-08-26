# Key Rotation Console — Terraform-triggered variant

This is a fork of the original key-rotation demo where **Terraform
triggers Go**, instead of the two of them running as decoupled loops
that never call each other. It exists because that's what was asked
for, not because it's the recommended shape — the base project's
README specifically argued against this direction. Read "What this
variant gives up" below before using it as a template for something
real.

The due-check itself — "has the current key's random expiry actually
passed?" — is **not** done in a shell script and **not** done in Go's
own ticker. It's done inside Terraform's own HCL, using its
`timecmp()` function against the expiration of whichever key entry has
`primary = true`. **Expiration lives per-key**, not as a separate
top-level value bolted onto the file — matching how a real keyset
resource actually works, where every key *version* has its own
creation/rotation schedule:

```hcl
resource "ursa_keyset" "u7_keyset" {
  id   = "unit_1_keyset"
  type = "AES-256-GCM"
  keys = [
    { label = "2026-08-23T04:40:36Z", expiration = "2026-08-23T04:40:55Z", length = 256, status = "ENABLED", primary = false },
    { label = "2026-08-23T04:40:55Z", expiration = "2026-08-23T04:41:03Z", length = 256, status = "ENABLED", primary = true  }
  ]
}
```

(`ursa_keyset` is a stand-in name — see `main.tf`'s commented-out block
for what a real deployment would swap in: `aws_secretsmanager_secret_version`,
a GCP KMS resource, Vault, etc. The demo itself applies `local_file`,
shown below, since there's no real `ursa` provider to install.)

```
 ┌───────────────┐  applies    ┌──────────────────────────────────────┐
 │ apply-loop.sh  │ ──────────▶ │ terraform apply                       │
 │ (just a timer, │  every 1s,  │                                        │
 │  no logic of   │  uncondit.  │  local.primary_key =                  │
 │  its own)      │             │    [k for k in var.keys if k.primary] │
 └───────┬────────┘             │  local.is_due =                       │
         │                      │    timecmp(timestamp(),               │
         │ reads (as            │            primary_key.expiration)    │
         │ -var-file)           │            >= 0                       │
         │                      │                                        │
         │                      │  is_due? ──▶ null_resource             │
         │                      │              .trigger_rotation         │
         │                      │              (count = 1)               │
         │                      │                    │                    │
         │                      │              local-exec:                │
         │                      │              curl POST                  │
         │                      │              /api/rotate-if-due         │
         │                      └────────────────────┼───────────────────┘
         │                                            ▼
         │                                   Go: re-checks now() vs
         │                                   this key's expires_at in
         │                                   Postgres (never trusts the
         │                                   call alone), rotates if
         │                                   still due, creates a NEW
         │                                   key row with its OWN random
         │                                   expiry, writes the full
         │                                   keyset back out:
         └──────── rotation.auto.tfvars.json ◀────────┘
                   (read on the NEXT apply)
```

## Quick start

**This variant's schema changed** (keys now carry their own
`expires_at` column) since an earlier version of this fork. If you're
upgrading from an older copy in place, run `./clean.sh` first to drop
the old volume — the migration path is idempotent for a *fresh* volume
but a stale one initialized under the old schema needs a clean start.

```
./clean.sh   # if upgrading from an older copy of this variant
./up.sh
```

Open **http://localhost:5173**, click **Generate Genesis Key**.
Rotation only happens when Terraform's own `local.is_due` condition
fires — watch `docker compose logs -f terraform-runner` to see each
apply; most will be quiet no-ops (nothing due yet).

```
./down.sh    # stop containers, keep all data/volumes
./clean.sh   # stop containers AND wipe volumes (asks for confirmation)
```

**If nothing rotates and the tfvars/output files never update**, check
in this order:
1. **Rebuild fully first, no cache.** This project's schema and
   `main.tf` both changed across several iterations while this variant
   was being built. If you're not certain your containers reflect the
   exact code in this zip:
   ```
   docker compose down -v
   docker compose build --no-cache
   docker compose up -d
   ```
   `terraform-runner`'s image now runs `terraform validate` as part of
   the build (`terraform/Dockerfile`) — if `main.tf` has any error,
   **the build itself fails immediately** with a clear message,
   instead of producing a working-looking image that then fails every
   apply silently at runtime.
2. `docker compose logs terraform-runner --tail 50` — the container
   also runs `terraform validate` again at startup (separate from the
   build-time check, to catch drift like a corrupted state volume) and
   logs `startup validate: OK` or `FAILED`. Beyond that, every
   `terraform apply` that fails prints Terraform's real error directly
   to this log (nothing swallows it) and when a rotation actually
   fires you'll see an explicit `trigger fired this cycle` line — total
   silence for a while is normal (most polls find nothing due), but
   you should see that line at least once every `max_seconds` (20s by
   default).
3. `curl http://localhost:8080/api/status | grep -o '"tfVars":{[^}]*'`
   — does `tfVars.exists` say `true` and does its `content` show a
   `keys` list with `expiration` fields on each entry? If not, genesis
   hasn't completed successfully — check `backend` logs instead.
4. If `tfVars` looks right but `terraformOutput.exists` stays `false`
   indefinitely even after step 1's clean rebuild, run the exact apply
   command by hand to see the raw error with nothing summarized away:
   ```
   docker compose exec terraform-runner sh -c 'terraform -chdir=/work apply -auto-approve -var-file=/shared/rotation.auto.tfvars.json'
   ```

## How this actually gets triggered, end to end

Every file involved, in the exact order things happen, from one
rotation to the next:

**1. Genesis (one-time).** You click "Generate Genesis Key" in the
browser. The frontend generates a key client-side (Web Crypto) and
`POST`s it to `backend`'s `/api/genesis` handler
(`backend/handlers.go`). That handler inserts the key into Postgres's
`keys` table with `status = 'primary'` and a freshly rolled
`expires_at` (3–20s out), sets `rotation_state.last_rotated_at`/
`expires_at` to match, and — critically — calls `writeTerraformVars`
(`backend/rotation.go`) to write the very first
`/shared/rotation.auto.tfvars.json`. Nothing rotates yet; this just
seeds the file the rest of the system watches.

**2. The shell loop (`terraform/apply-loop.sh`), forever, every 1s.**
This script has no logic of its own about *whether* to rotate — it
just checks whether `/shared/rotation.auto.tfvars.json` exists, and if
so, runs:
```
terraform -chdir=/work apply -auto-approve -var-file=/shared/rotation.auto.tfvars.json
```
unconditionally, every single second. Whether that apply actually
*does* anything is entirely up to step 3.

**3. Terraform's own due-check (`terraform/main.tf`).** On every
apply, Terraform evaluates:
```hcl
locals {
  primary_key = try([for k in var.keys : k if k.primary][0], null)
  is_due      = try(local.primary_key != null && timecmp(timestamp(), local.primary_key.expiration) >= 0, false)
}
```
`var.keys` came straight from the tfvars file the shell loop just
passed in. This finds whichever entry has `primary = true`, and checks
whether *its own* `expiration` field has passed, using Terraform's own
`timestamp()`/`timecmp()` — no shell-side date math, no separate
top-level expiry variable.

**4. The trigger itself.** If `is_due` is true,
`null_resource.trigger_rotation` gets `count = 1` and Terraform
*creates* it — and a newly-created resource's `local-exec` provisioner
runs on creation:
```
curl -sf -X POST http://backend:8080/api/rotate-if-due -o /shared/last-trigger-response.json
```
This is the literal "Terraform triggers Go" moment. If `is_due` is
false, `count = 0`, the resource doesn't exist, nothing fires — this
is the vast majority of the 1-second polls.

**5. Go re-validates, then rotates (`backend/handlers.go` →
`backend/rotation.go`).** `handleRotateIfDue` never trusts that the
call itself proves anything is due — it calls `tryRotate`, which opens
a transaction, takes a Postgres advisory lock, and re-checks
`now() >= expires_at` against the row Terraform's `local.is_due` was
*based on*, using Postgres's own clock. If it's genuinely still due:
a new key is inserted with a fresh random `expires_at`
(`min_seconds`–`max_seconds` out), every key that has ever existed
(not just the new one) is live-decrypt-tested, the old primary is
retired, the new one is promoted, and the transaction commits.

**6. Go writes the new state back out.** Strictly *after* that commit
(never before), `writeTerraformVars` reads the full current keyset
from Postgres and overwrites `/shared/rotation.auto.tfvars.json` — now
showing the new key as `primary = true` with its own new `expiration`,
and the old key as `primary = false`.

**7. Back to step 2.** The shell loop's *next* 1-second poll picks up
that updated file. Terraform re-evaluates `local.is_due` against the
*new* primary key's expiration — now in the future — so `is_due` flips
back to `false`, `count` flips back to `0`, and
`null_resource.trigger_rotation` gets quietly destroyed (no
provisioner fires on the way down). That destroy is what re-arms the
trigger for next time. The cycle repeats from step 3 once the new
key's own expiration is reached.

Nothing in this loop calls back from Go into Terraform, and nothing
in Terraform calls back into itself — it's one direction, once per
poll: shell → Terraform (evaluates) → [maybe] Terraform → Go →
Postgres → back into the file the shell loop reads next.

## Real-time display: how the UI stays live

The dashboard's "Live files" section has **five** tabs, not just the
raw exchange files, because those alone don't answer "what does the
resource actually look like right now":

| Tab | What it shows | Source |
|---|---|---|
| **live resource (HCL)** | The actual resource block, in real HCL syntax, populated with live current values — e.g. `resource "ursa_keyset" "unit_1_keyset" { id = "...", keys = [...] }` | Rendered fresh on every call by `renderKeysetResourceHCL` (`backend/hcl_render.go`) from whatever `listKeys` currently returns. Not a file on disk — synthesized on the fly. |
| **rotation.auto.tfvars.json** | The raw JSON Go writes and Terraform consumes as its `-var-file` | `/shared/rotation.auto.tfvars.json`, read fresh off disk |
| **terraform output** | The raw JSON `local_file.current_keyset` last wrote | `/shared/terraform-output/current-key-reference.json`, read fresh off disk |
| **tf-config** | The static `.tf` source itself — variable declarations, `locals`, the real (and the commented-out example) `resource` blocks | `terraform/main.tf`, mounted read-only into the backend container |
| **history** | The append-only rotation ledger — every attempt ever made, kept forever | Postgres `rotation_events`, queried fresh |

That first tab is the direct answer to "where is this format" — the
`resource "ursa_keyset" "..." { ... }` block you're looking for isn't
static text anywhere in the repo; it's generated live, in that exact
shape, every time the dashboard updates.

The frontend never polls any of these files/queries directly — it
only ever talks to `backend`'s own API, which does the reading and
re-rendering on every request:

- **`backend/rotation.go`** runs `runStatusBroadcastLoop`, a 1-second
  ticker that calls `loadStatus` (`backend/status.go`) and pushes the
  result to every connected browser over Server-Sent Events
  (`backend/sse.go`, `GET /api/events`).
- **`loadStatus`** doesn't cache anything — every single call re-reads
  `rotation.auto.tfvars.json`, `terraform-output/current-key-reference.json`,
  and the mounted `main.tf` straight from disk (`readArtifact` in
  `backend/status.go`), re-renders the live HCL block from the current
  key list, and re-queries Postgres for the current key list, rotation
  state, history, and genesis audit log.
- **The frontend** (`frontend/src/main.ts`) subscribes once via
  `EventSource` and calls `render(status)` on every message — which
  rewrites all five tabs with whatever content just came back, plus a
  `lastModified` timestamp per file where one applies. A 2-second
  polling fallback (`fetchStatus` on a `setInterval`) covers the brief
  window before the first SSE message arrives, or if `EventSource` is
  blocked entirely.

So "real-time" here means: **at most 1 second of staleness**, bounded
by the broadcast loop's own tick — not by anything related to the
rotation cadence itself. Even on a poll where nothing rotated, the UI
still refreshes every second, because `runStatusBroadcastLoop` doesn't
know or care whether a rotation happened; it just publishes whatever
is currently true.

## What changed from the base (hash-gated, two-loop) version

| | Base version | This variant |
|---|---|---|
| Who decides *when* to rotate | Go, on its own 1s ticker, checking Postgres | Terraform's own `local.is_due`, derived from the primary key's own `expiration` inside `var.keys` |
| Rotation interval | Fixed (`interval_seconds`, e.g. 10s) | Random per rotation, `min_seconds`–`max_seconds` (default 3–20s), rolled and stored on each key's own row at creation time |
| Where expiration lives | N/A | **Per key**, not as a separate top-level file field — `keys.expires_at` in Postgres, `expiration` on each entry in the tfvars/output json |
| What decides whether to run `terraform apply` at all | A content-hash comparison (`apply-loop.sh` vs `.last-applied-hash`) | Nothing — `apply-loop.sh` calls `terraform apply` unconditionally, on a timer |
| What decides whether that apply *does* anything | N/A (no trigger existed) | `local.is_due`, computed entirely from `var.keys` |
| Does Terraform call Go | No — one-way, Go → Terraform only | **Yes** — `null_resource.trigger_rotation`'s `local-exec` provisioner calls `POST /api/rotate-if-due`, but only exists (`count = local.is_due ? 1 : 0`) when Terraform itself has decided it's due |
| Terraform-output sync badge | "in sync" once the hash matches | Compares the two files' *primary key's* `expiration` directly — normally *won't* match; see the one-cycle-lag section |

## How the due-check actually works, in Terraform's own words

```hcl
variable "keys" {
  type = list(object({
    label      = string # creation timestamp, used as the version label
    expiration = string # RFC3339 instant THIS key is scheduled to be superseded
    length     = number
    status     = string
    primary    = bool
  }))
  default = []
}

locals {
  primary_key = try([for k in var.keys : k if k.primary][0], null)
  is_due      = try(local.primary_key != null && timecmp(timestamp(), local.primary_key.expiration) >= 0, false)
}

resource "null_resource" "trigger_rotation" {
  count = local.is_due ? 1 : 0

  triggers = {
    primary_key_label = try(local.primary_key.label, "")
  }

  provisioner "local-exec" {
    command = "curl -sf -X POST ${var.trigger_url}/api/rotate-if-due -o ${var.trigger_response_path}"
  }
}
```

- **`local.primary_key`** finds the one entry in `var.keys` with
  `primary = true`, or `null` before the first genesis key exists.
  There's no separate "which key is current" variable — that fact
  lives entirely inside the keys list itself, same as a real keyset
  resource.
- **`timestamp()`** returns the current UTC instant, freshly evaluated
  every single `plan`/`apply`.
- **`timecmp(a, b)`** (Terraform ≥ 1.5) compares two RFC3339
  timestamps and returns -1/0/1. `>= 0` means "now has reached or
  passed this key's expiration."
- **`try(..., false)`** guards the case where `var.keys` is empty
  (before genesis) — indexing `[0]` into an empty list, or comparing
  against a `null` primary key's expiration, would otherwise error.
- **`count = local.is_due ? 1 : 0`** is what actually fires the
  trigger, and what makes the whole thing self-resetting — see below.

### Why `count`, and a label (not the raw expiration), drives the trigger

Terraform only re-runs a provisioner when something about the resource
changes. The create/destroy lifecycle itself provides the pulse here:

1. Not due: `count = 0`, the resource doesn't exist.
2. Expiration passes: `local.is_due` flips to `true`, `count` flips to
   `1`, Terraform **creates** the resource, and a newly-created
   resource's `local-exec` provisioner runs — this is the trigger.
3. Go rotates (inside that same HTTP call), creates a brand-new key
   row with its own fresh random expiry, and writes the **full,
   updated keyset** back into the tfvars file — the old key now shows
   `primary = false`, the new one shows `primary = true` with a new
   `label` and `expiration`.
4. The *next* `terraform apply` reads that new keyset. `local.is_due`
   is now `false` (the new primary's expiration hasn't arrived yet),
   `count` flips back to `0`, and Terraform quietly **destroys** the
   resource (no destroy-time provisioner is defined, so nothing fires
   on the way down).
5. That destroy re-arms the trigger for the next cycle.

`triggers = { primary_key_label = ... }` uses the primary key's
**label** (its creation timestamp — effectively its identity), not its
expiration, as the defence-in-depth signal. A new primary key always
has a different label than the previous one; using the label rather
than the expiration avoids a theoretical edge case where two different
primary keys could compute the same expiration.

**One honest caveat this leaves:** if the `local-exec` call succeeds
(Go returns HTTP 200) but its response body says `rotated: false` for
a reason that *isn't* reflected by the primary key changing — the only
realistic case is `tryRotate` losing the advisory-lock race to another
backend replica — this resource is "successfully created" with
nothing left to force a retry, so nothing retries automatically until
the primary key next changes. With the single backend replica this
compose stack runs, that scenario can't actually happen; it would only
start to matter if you scaled the backend service horizontally.

## The one-cycle lag this shape can't avoid

Within a single `terraform apply`, `null_resource.trigger_rotation`
(which calls Go) and `local_file.current_keyset` (which writes the
output file) have no dependency on each other, so Terraform is free to
evaluate them in either order. Either way, `local_file.current_keyset`
uses the variable values passed in *at invocation time* — the
*previous* cycle's `rotation.auto.tfvars.json` — never the result of
the `local-exec` call that runs during that same apply.

Concretely: an apply that finds `local.is_due == true`, fires the
trigger, and causes Go to rotate — that same apply's own output file
still shows the OLD keyset, because that's what was true when the
apply started. The new key only appears in the output file on the
*next* apply, once `apply-loop.sh`'s next timer tick re-reads the (now
updated) tfvars file.

This isn't a bug to fix — it's a structural consequence of Terraform
resources being evaluated from the state existing *before* a
provisioner's side effect, within the same apply. The UI's sync badge
reflects this honestly ("catching up — one apply cycle behind")
instead of hiding it.

## What this variant gives up

The base project's README argued for keeping Go and Terraform
decoupled specifically to avoid this class of problem. Building this
variant anyway means giving up some things Go's own ticker provided
for free in the base version:

- **Availability now depends on the sidecar.** If `terraform-runner`
  crashes, or `curl` isn't installed, or the backend's DNS name
  changes, rotation silently stops — nothing in Go notices or
  complains, because nothing in Go is watching the clock anymore.
- **The one-cycle lag above**, which the base version's independent
  snapshot files never had.
- **A `null` provider dependency**, and a Terraform version floor tied
  to a specific function (`timecmp()`, 1.5.0 — already this project's
  floor, so no bump was needed).

What this version does **not** give up, compared to an earlier
draft that used a fixed top-level `expires_at` variable and a manually
supplied nonce: the kill-switch busy-loop problem that draft had
(retrying once a second forever while paused) is gone. Pausing stops
the primary key's expiration from ever being superseded, `local.is_due`
stays `true`, but since nothing about the primary key's `label`
changes on a failed-to-actually-rotate response, `null_resource`, once
created, has nothing forcing it to recreate — it just sits there
quietly until you resume.

## Idempotency and safety measures (updated for this variant)

Everything from the base version still applies, with the due-check
re-keyed from a fixed interval to a per-key random expiry, and re-homed
from Go's ticker into Terraform's `local.is_due`:

1. **Deterministic rotation IDs**, now keyed on `sha256(primary_key_id
   + expires_at)` (the primary key's own row-level expiry) instead of
   `sha256(primary_key_id + time_bucket)`. Same guarantee: a duplicate
   call before that expiry changes collides on
   `rotation_events.rotation_id` and is rejected by Postgres.
2. **Advisory lock per call**, unchanged.
3. **One transaction per rotation**, unchanged — and now the new key's
   own `expires_at` is written in the SAME insert that creates the row,
   not as a separate update, so there's no window where a key exists
   without a defined expiration.
4. **Verify before promote**, unchanged.
5. **Old keys retired, never deleted** — and their own `expires_at` is
   preserved as a historical record of what their schedule was, even
   after they're superseded.
6. **DB clock, not caller's clock.** Terraform believes a rotation is
   due because `timecmp()` said so against the expiration it was
   handed; Go never trusts that belief and re-derives "is this
   actually due" from `now()` inside the locked transaction, every
   single call.
7. **The kill switch**, unchanged in mechanism. See "What this variant
   gives up" above for how the label-keyed trigger handles a pause
   gracefully rather than busy-looping.
8. **Crash recovery is still just a normal read** — a restarted
   backend or restarted terraform-runner both just resume from durable
   state (Postgres for Go, Terraform's own state + the tfvars file for
   the trigger logic); neither reconstructs anything from memory.

## Timing: is 1s poll / 3–20s random expiry correct?

Same ratio argument as before:

- `POLL_SECONDS` (default 1, in `apply-loop.sh`) is "how often do we
  ask Terraform to check" — keep it well under `min_seconds` (default
  3) or you'll visibly overshoot short expiries.
- Because the actual due-check is a real timestamp comparison inside
  Terraform (not shell-side math), there's no meaningful precision
  loss from that layer — the only imprecision left is
  `apply-loop.sh`'s own polling granularity.
- Widening the range (e.g. `ROTATION_MIN_SECONDS=1`) without also
  tightening `POLL_SECONDS` will make the "random" part of the timing
  dominated by polling granularity, not the actual random draw.

## Configuration

| Variable | Where | Default | Purpose |
|---|---|---|---|
| `min_seconds` / `max_seconds` (in `rotation_state`) | Postgres | `3` / `20` | Random rotation range, rolled fresh for each new key at creation time. Change with `UPDATE rotation_state SET min_seconds = 5, max_seconds = 15;` |
| `SHARED_DIR` | backend, terraform-runner | `/shared` | Volume where `rotation.auto.tfvars.json` is exchanged |
| `POLL_SECONDS` | terraform-runner | `1` | How often `apply-loop.sh` runs `terraform apply` (unconditionally — Terraform itself decides if anything happens) |
| `trigger_url` (terraform var) | terraform-runner | `http://backend:8080` | Where the `local-exec` provisioner calls back into Go |

## Project layout

```
db/init.sql          Postgres schema -- keys.expires_at is new (each key's own scheduled expiry); rotation_state still tracks min/max/last_rotated_at for Go's own bookkeeping
backend/              Go service. Every key row carries its own expires_at, set once at INSERT time; new POST /api/rotate-if-due
frontend/             Same console UI, unaffected by the per-key restructuring (still reads status.expiresAt from rotation_state via the API, not from the tfvars json directly)
terraform/            main.tf's local.primary_key + local.is_due + null_resource.trigger_rotation IS the trigger, derived entirely from var.keys; apply-loop.sh is just a timer
docker-compose.yml    Same shape as the base version
up.sh / down.sh / clean.sh
```

## Notes on the demo crypto

Unchanged from the base version: key rotation is HKDF-SHA256 over the
previous key material salted with the rotation timestamp (a proper KDF
chain, not byte concatenation), AES-256 keys, and the live test is an
AES-GCM round-trip against a fixed plaintext, one stored ciphertext
per key generation.
