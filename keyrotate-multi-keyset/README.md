# Key Rotation Console — multi-keyset (N / M / L) variant

A demo stack that shows two independent loops — a Go backend and a Terraform
sidecar — keeping each other honest while **N** independent keysets live their
own random rotation schedules at once, instead of just one.

```
 ┌─────────────┐        writes         ┌──────────────────────────────┐
 │   Postgres  │ ───────────────────▶  │ rotation.auto.tfvars.json    │
 │  (source of │   (Go, after any      │ (shared volume)               │
 │    truth)   │    keyset rotates)    └──────────────────────────────┘
 └─────────────┘                                     │
        ▲                                            │ read by
        │ rotate-if-due                               ▼
 ┌─────────────┐        HTTP POST      ┌──────────────────────────────┐
 │  Go backend │ ◀───────────────────  │  Terraform apply-loop.sh     │
 │  (rotates   │   "at least one       │  (applies on a timer,        │
 │  N keysets) │    keyset is due")    │   decides due-ness in HCL)   │
 └─────────────┘                       └──────────────────────────────┘
                                                       │
                                                       ▼
                                        current-key-reference.json
                                              (shared volume)
```

## What's new in this variant

The original version of this stack managed **one** keyset with **one**
rotating primary key. This variant manages **N** of them at once:

| Symbol | Meaning | Default |
|---|---|---|
| **N** | Total keysets created at genesis | `50` |
| **M** | Of the N, how many are randomly selected to actually rotate | `20` |
| **N − M** | The rest: created once, never scheduled to rotate ("static") | `30` |
| **L** | Of the M rotating keysets, how many are *also* randomly flagged **revoked** — i.e. their very first key is born already-expired and gets renewed immediately on the first apply cycle, instead of waiting for a normal random interval | random, `0..M`, rolled fresh every batch genesis |

Batch genesis is still a **single, one-time, idempotent event** for the whole
stack — click "Generate 50 Keysets" once, and every subsequent click (or
every restart of the frontend) just reports what already exists. Nothing
about "one genesis, ever" changed; it just now creates N keysets in one
transaction instead of one keyset.

Everything downstream — the due-check, the rotation transaction, the
liveness test, the tfvars/output files, the audit trail — was generalized
from "the one keyset" to "whichever of the N keysets are relevant," but the
underlying *mechanics* of a single rotation (verify-then-promote, the
advisory lock, the idempotency ledger, the kill switch) are unchanged from
the base design. See [How one rotation actually works](#how-one-rotation-actually-works)
below.

## Quick start

```bash
./up.sh
```

This starts Postgres, the Go backend, the Terraform sidecar (which polls
`terraform apply` every second), and a small nginx container serving the
built frontend. Open the printed URL, click **"Generate 50 Keysets,"** and
watch the dashboard update in real time — some keysets will start rotating
within seconds (any of the L revoked ones almost immediately, the rest of
the M rotating ones over the next few seconds to ~20 seconds, per keyset).

```bash
./down.sh    # stop everything, keep the Postgres volume
./clean.sh   # stop everything AND wipe the Postgres volume (start completely fresh)
```

## The dashboard

- **Batch Genesis** — one button, one event, for the whole stack. Shows the
  resulting N/M/L/static counts once it's run.
- **Overview** — live N/M/L/static counts, the soonest any rotating keyset
  is due, and the global kill switch.
- **Keysets** — one row per keyset (N rows): its kind (static / rotating /
  rotating + revoked), a derived status badge, its current primary key,
  generation, and (for rotating keysets) a live countdown to its next
  rotation. Filter tabs above the table (**All / Rotating / Revoked /
  Static**) narrow both this table *and* the History tab below to the same
  subset at once. Below that is a **numbered index strip** — one small
  colored swatch per keyset (1..N) — click any number to jump straight to
  that keyset in both tables, functioning as an individual per-keyset "tab"
  without needing N real `<button>` tabs (which would just wrap into
  several unreadable rows at N=50). The same drill-down also works by
  clicking a chip anywhere else (Keysets rows, History rows); click it
  again, or use "clear," to go back. The color itself never changes no
  matter which filter or drill-down is active.
- **Live files & history** — five tabs:
  - *live resource (HCL)* — every keyset's `ursa_keyset` resource block,
    rendered fresh from the database on every update, with live values
    plugged in.
  - *rotation.auto.tfvars.json* — the actual file Go writes and Terraform
    reads: the full state of all N keysets.
  - *terraform output* — the actual file Terraform writes back after
    applying: what Terraform currently believes is true.
  - *tf-config* — the static `main.tf` source, for reference.
  - *history* — the append-only rotation ledger across every keyset, newest
    first, subject to the same filter tabs / drill-down as the Keysets
    table above (they're the same "which keysets am I looking at" state,
    shared across both tables). Each row's left edge and keyset chip use
    that same per-keyset color, so in a busy, interleaved feed of 20
    independently-rotating keysets you can either narrow to a category
    (e.g. just the L revoked ones) or click straight through to one
    keyset's own rotation history. Applied / skipped / failed results keep
    their own green/red badges on top of that.
- **Genesis audit trail** — every `/api/genesis` request the backend has
  ever received, including harmless no-op re-clicks after the real batch
  genesis already ran.

## How one rotation actually works

This part is unchanged from the single-keyset base design — it just now
runs independently, per keyset, for every one of the M rotating keysets:

1. **Terraform decides "is anything due," in HCL, not in shell.**
   `terraform/main.tf` takes the full list of N keysets (`var.keysets`) and,
   for each one, finds the entry with `primary = true` and compares its
   `expiration` against `var.now` (an RFC3339 string passed in by
   `apply-loop.sh`, *not* Terraform's own `timestamp()` — see the comment on
   `variable "now"` in `main.tf` for why `timestamp()` can't be used here).
   `local.due_keyset_ids` collects the IDs of every keyset that's currently
   due — zero, one, or several at once.
2. **A `null_resource` only exists when something is due.** If
   `local.is_due` is true, `null_resource.trigger_rotation`'s single
   `local-exec` provisioner fires a `curl -X POST` to the Go backend's
   `/api/rotate-if-due` endpoint. It doesn't tell Go *which* keyset(s) —
   "at least one is due" is the entire signal.
3. **Go re-derives the truth from Postgres, never trusts the caller.**
   `tryRotateAll` takes a global advisory lock (so only one backend replica
   ever acts on a given tick), then queries `rotation_state` for every
   keyset whose `expires_at <= now()` **according to the database's own
   clock** — completely independent of whatever the tfvars snapshot Terraform
   read a moment ago actually said.
4. **Each due keyset rotates in its own transaction**, scoped entirely to
   that `keyset_id`: derive a new key from the current primary, insert it as
   `pending`, decrypt a fixed test vector under **every key that has ever
   existed in that keyset's own lineage** (never across keysets), and only
   if every single one still decrypts correctly, retire the old primary and
   promote the new one. A failed liveness test rolls the whole transaction
   back and leaves the old key in place.
5. **Go writes the tfvars file back**, once, after the whole batch of due
   keysets has been processed — a full, fresh snapshot of every keyset's
   current key list, metadata only, never raw material.
6. **The idempotency ledger makes duplicate rotations impossible.** Each
   rotation's ID is `sha256(keyset_id + primary_key_id + expiry_instant)`.
   Calling `/api/rotate-if-due` early, late, or twice for the same keyset
   and the same expiry window always resolves to the same row in
   `rotation_events`, so a duplicate attempt is a harmless no-op, not a
   second rotation.

Because of the one-cycle lag inherent in "Terraform reads → Go rotates →
Terraform re-applies," the tfvars file and the terraform-output file will
*normally* disagree with each other for most of any given polling cycle —
the dashboard's "Sync status" badge reflects that honestly instead of
hiding it.

## Configuration

All via environment variables on the `backend` service in
`docker-compose.yml`:

| Variable | Default | Meaning |
|---|---|---|
| `KEYSET_COUNT` | `50` | N — total keysets created at batch genesis |
| `ROTATING_COUNT` | `20` | M — how many of the N are randomly selected to rotate |
| `ROTATION_MIN_SECONDS` | `3` | Lower bound of each rotating keyset's random rotation interval |
| `ROTATION_MAX_SECONDS` | `20` | Upper bound of each rotating keyset's random rotation interval |

There is deliberately **no env var for L** — the number of rotating keysets
that are also flagged `revoked` is rolled fresh, uniformly at random between
`0` and `M`, on every batch-genesis call (`rand.Intn(m + 1)` in
`backend/genesis.go`), so re-running genesis against a fresh database
produces a different L each time.

`/api/genesis` also accepts an optional JSON body to override N/M for that
one call without touching the environment:

```bash
curl -X POST localhost:8080/api/genesis \
  -H 'Content-Type: application/json' \
  -d '{"keysetCount": 10, "rotatingCount": 4}'
```

## Project layout

```
backend/
  main.go        — wiring: HTTP routes, env config, graceful shutdown
  genesis.go      — the N/M/L batch-genesis event (one-time, idempotent)
  rotation.go     — tryRotateAll / rotateOneKeyset, tfvars writer, jitter
  status.go       — /api/status aggregation, sync-check, artifact reading
  hcl_render.go   — live HCL rendering for the "live resource" tab
  handlers.go     — HTTP handlers
  controls.go     — kill switch + append-only audit log helpers
  crypto.go       — fixed AES-256-GCM material generation / test vectors
  db.go, sse.go, uuid.go — plumbing

db/
  init.sql        — keysets / keys / rotation_state / rotation_events /
                     system_state / genesis_attempts schema

terraform/
  main.tf         — var.keysets (list of N), local.due_keyset_ids, the
                     null_resource trigger, the demo local_file "resource"
  apply-loop.sh   — the timer loop that runs `terraform apply` every second
  Dockerfile

frontend/
  src/main.ts     — dashboard rendering, per-keyset coloring, tab switching
  src/api.ts      — fetch wrappers + SSE subscription
  src/types.ts    — TypeScript types mirroring the Go JSON shapes
  src/style.css   — styling, including the keyset-chip coloring

docker-compose.yml, up.sh, down.sh, clean.sh
```

## Notes on the crypto

Same as the base design — this is a **rotation-mechanics demo, not a real
KMS.** Key material is 32 random bytes (`crypto/rand`) treated as an
AES-256-GCM key; "deriving the next key" and the liveness test both live in
`backend/crypto.go`. Nothing here should be mistaken for a real key
management system's actual cryptography — the point of this stack is the
rotation *orchestration* (the due-check split across Terraform and Go, the
liveness test gating promotion, the idempotency ledger, the kill switch),
now exercised against N independent keysets instead of one.
