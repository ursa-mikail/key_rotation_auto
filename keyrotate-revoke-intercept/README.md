# Key Rotation Console — all-rotate + revoke interceptor variant

A demo stack that shows two independent loops — a Go backend and a Terraform
sidecar — keeping each other honest, PLUS a third, asynchronous process that
randomly revokes keysets out of band, deliberately racing against the timer.

```
 ┌─────────────┐        writes         ┌──────────────────────────────┐
 │   Postgres  │ ───────────────────▶  │ rotation.auto.tfvars.json    │
 │  (source of │  (Go, after any       │ (shared volume)               │
 │    truth)   │   keyset changes)     └──────────────────────────────┘
 └─────────────┘                                     │
    ▲        ▲                                       │ read by
    │        │ SELECT ... FOR UPDATE                  ▼
    │        │ (same row, so these two              ┌──────────────────────────────┐
    │        │  serialize per keyset)                │  Terraform apply-loop.sh     │
    │        │                                        │  (applies on a timer,        │
    │  ┌─────┴──────────┐        rotate-if-due        │   decides due-ness in HCL,   │
    │  │ Revoke          │◀──────────────────────────│   excludes terminated ones)  │
    │  │ interceptor     │       HTTP POST             └──────────────────────────────┘
    │  │ (async, random) │
    │  └─────────────────┘
    │
    └── /api/revoke (manual)
```

## What's new in this variant

The N/M/L variant split N keysets into a rotating subset and a static subset.
This variant goes further in a different direction: **every keyset rotates**,
all the time, on its own random 3–20s interval — and, independently,
a background process randomly **revokes** a keyset at any moment, with no
regard for that keyset's own timer.

| Concept | Meaning |
|---|---|
| **N** | Total keysets created at genesis (default `50`) — all of them rotate |
| **Revoke interceptor** | A background loop that, on a tick, with some probability, picks one uniformly random *still-active* keyset and revokes it |
| **Revoke mode** (`REVOKE_AUTO_ROTATE`) | Global, live-toggleable: what happens when a revoke lands |
| **Auto-rotate** | The revoked keyset is immediately, emergency-rotated, and resumes normal cycling |
| **Halt** | The revoked keyset is permanently stopped — no further rotation, ever |

Batch genesis is still a single, one-time, idempotent event: N keysets, each
with its own genesis key and its own random rotation interval. There is no
"revoked at birth" concept in this variant — every revocation happens live,
at runtime, from the interceptor (or a manual `/api/revoke` call).

## The collision, and how it's resolved

Because the revoke interceptor is completely asynchronous, it can fire for a
keyset **at the exact same moment** the timer-driven rotation loop is already
mid-rotation for that same keyset. Nothing in this stack tries to prevent
that race from happening — instead, both paths are built so the race is
always resolved safely, in a well-defined order, with nothing silently lost:

1. **Both paths lock the same row.** The timer loop's `rotateOneKeyset` and
   the revoke path's `rotateOneKeyset`/`runHaltKeyset` (see
   `backend/rotation.go` and `backend/revoke.go`) each begin their
   transaction with `SELECT ... FROM rotation_state WHERE keyset_id = $1
   FOR UPDATE`. Postgres itself serializes any two transactions racing for
   the same keyset's row — whichever one starts first runs to completion
   (commit or rollback) before the second one's `SELECT ... FOR UPDATE` even
   returns.
2. **The second transaction always sees the true, post-first-transaction
   state**, never a stale read. Concretely:
   - **Timer rotates first, then a revoke lands** (auto-rotate mode): the
     revoke's rotation proceeds against the *freshly rotated* primary key —
     i.e. the keyset gets emergency-rotated a second time, right on top of
     the timer's rotation. Nothing is skipped.
   - **Timer rotates first, then a revoke lands** (halt mode): the halt
     proceeds immediately after that rotation committed — "after you
     rotate, revoke anyway," exactly as specified. The keyset's very last
     (freshly rotated) key becomes its permanently frozen final state.
   - **A revoke lands first** (auto-rotate mode): it emergency-rotates and
     resets the keyset's timer to a fresh random interval. When the
     due-check loop later looks at this keyset, it's no longer due (the
     revoke already reset it) — no double rotation, no wasted work.
   - **A revoke lands first** (halt mode): it deletes the keyset's
     `rotation_state` row and marks it `terminated`. The timer loop's due
     query sources directly from `rotation_state`, so a terminated keyset
     simply, silently disappears from every future due-check — no error, no
     special-case skip logic needed.
3. **A revoke against an already-terminated keyset is a safe no-op.** Both
   `rotateOneKeyset` (force=true) and `runHaltKeyset` treat a missing
   `rotation_state` row as "already terminated" and return a `skipped`
   result — this is also how the random interceptor never fires twice on a
   keyset that's already been halted (it draws only from
   `rotation_state`, so terminated keysets are never even offered as
   candidates).

## Quick start

```bash
./up.sh
```

Open the printed URL, click **"Generate 50 Keysets,"** and watch the
dashboard. Every keyset starts rotating within a few seconds to ~20 seconds.
Separately, roughly every ~8–9 seconds on average (the default
`REVOKE_INTERCEPT_INTERVAL_SECONDS=3` × `REVOKE_INTERCEPT_PROBABILITY=0.35`),
the revoke interceptor fires against one random keyset. Watch the **Revoke
mode** toggle in the Overview card, flip it between Auto-rotate and Halt, and
watch new revocations behave differently in real time.

```bash
./down.sh    # stop everything, keep the Postgres volume
./clean.sh   # stop everything AND wipe the Postgres volume (start completely fresh)
```

## The dashboard

- **Batch Genesis** — one button, one event, for the whole stack.
- **Overview** — live N / active / terminated counts, the soonest any
  keyset is due, the kill switch (stops the timer loop AND the revoke
  interceptor at once), the **revoke mode toggle** (Auto-rotate ⚡ / Halt
  ⛔), and a **"Revoke a random keyset now"** button for demoing the
  collision case on demand.
- **Keysets** — one row per keyset: status (`rotating` / `⛔ terminated`), a
  **last-event badge** (⏱ rotated / 🎲 revoked → rotated / 🎲 revoked →
  terminated / failed), current primary key, generation, and (if still
  active) a live countdown. Filter tabs (**All / Active / Terminated**)
  narrow both this table and the History tab below at once. A numbered
  index strip (1..N) lets you jump straight to any specific keyset — click
  a number, or any colored chip anywhere, to drill down to just that one
  keyset in both tables (and optionally revoke just that one via "revoke
  this one" in the selection bar).
- **Live files & history** — five tabs, same as the other variants, plus:
  the tfvars/output json and the live-rendered HCL now carry `terminated`,
  `last_outcome`, and `last_trigger` on every keyset entry, updated in real
  time on every rotation AND every revoke — see
  [Real-time status in tf/json](#real-time-status-in-tfjson) below. History
  rows show a trigger icon (⏱ timer / 🎲 revoke) and the same outcome
  badges as the Keysets table.
- **Genesis audit trail** — every `/api/genesis` request ever received.

## Real-time status in tf/json

Both the tfvars file Go writes (`rotation.auto.tfvars.json`) and the output
file Terraform writes back (`current-key-reference.json`), plus the
dashboard's live-rendered HCL tab, carry three extra fields on every keyset
entry, refreshed on every state change (genesis, every timer rotation,
every revoke):

```json
{
  "keysets": [
    {
      "id": "unit_07_keyset",
      "type": "AES-256-GCM",
      "terminated": false,
      "lastOutcome": "revoked_rotated",
      "lastTrigger": "revoke",
      "lastEventAt": "2026-08-27T20:14:03.881Z",
      "keys": [ { "label": "...", "expiration": "...", "primary": true, ... } ]
    }
  ]
}
```

`lastOutcome` is one of: `rotated` (ordinary, on-schedule), `revoked_rotated`
(revoked, then emergency-rotated), `revoked_terminated` (revoked, then
halted), or `failed` (a rotation's post-promotion liveness test failed —
nothing was promoted). `terminated` is the permanent flag; once true, that
keyset's `keys[].expiration` is frozen forever, and
`terraform/main.tf`'s `local.active_keysets` excludes it from every future
due-check (see the comment on that local for why this matters — without it,
Terraform would believe a terminated keyset is perpetually "due" and keep
firing pointless `rotate-if-due` calls forever).

## Configuration

Environment variables on the `backend` service in `docker-compose.yml`:

| Variable | Default | Meaning |
|---|---|---|
| `KEYSET_COUNT` | `50` | N — total keysets created at batch genesis, all rotating |
| `ROTATION_MIN_SECONDS` | `3` | Lower bound of each keyset's random rotation interval |
| `ROTATION_MAX_SECONDS` | `20` | Upper bound of each keyset's random rotation interval |
| `REVOKE_INTERCEPT_INTERVAL_SECONDS` | `3` | How often the interceptor loop ticks |
| `REVOKE_INTERCEPT_PROBABILITY` | `0.35` | Probability of firing on any given tick (set to `0` to disable) |
| `REVOKE_AUTO_ROTATE` | `true` | Initial revoke mode at startup — live-toggleable afterward via `/api/revoke-mode` |

## API additions in this variant

```bash
# Manually revoke a specific keyset (or omit keysetId for a random active one)
curl -X POST localhost:8080/api/revoke \
  -H 'Content-Type: application/json' \
  -d '{"keysetId": "unit_07_keyset"}'

# Flip the global revoke-mode toggle
curl -X POST localhost:8080/api/revoke-mode \
  -H 'Content-Type: application/json' \
  -d '{"autoRotate": false}'
```

## Project layout

```
backend/
  main.go        — wiring: HTTP routes, env config, both background loops
  genesis.go      — the N-keyset, all-rotating batch-genesis event
  rotation.go     — timer-path rotation, shared rotateOneKeyset (trigger/force), tfvars writer
  revoke.go       — the revoke interceptor loop, processRevoke, runHaltKeyset
  status.go       — /api/status aggregation, last-event lookups, sync-check
  hcl_render.go   — live HCL rendering, including terminated/last_outcome
  handlers.go     — HTTP handlers, including /api/revoke, /api/revoke-mode
  controls.go     — kill switch + append-only audit log helpers
  crypto.go       — fixed AES-256-GCM material generation / test vectors
  db.go, sse.go, uuid.go — plumbing

db/
  init.sql        — keysets(terminated) / keys / rotation_state /
                     rotation_events(trigger, outcome) / system_state(revoke_auto_rotate) /
                     genesis_attempts schema

terraform/
  main.tf         — var.keysets (terminated/last_outcome/last_trigger),
                     local.active_keysets excludes terminated ones from due-check
  apply-loop.sh   — the timer loop that runs `terraform apply` every second
  Dockerfile

frontend/
  src/main.ts     — dashboard rendering, revoke-mode toggle, revoke-now button,
                     per-keyset coloring, filter tabs, index strip, drill-down
  src/api.ts      — fetch wrappers, revoke + revoke-mode calls, SSE subscription
  src/types.ts    — TypeScript types mirroring the Go JSON shapes
  src/style.css   — styling, including the segmented revoke-mode control

docker-compose.yml, up.sh, down.sh, clean.sh
```

## Notes on the crypto

Same as the other variants — this is a **rotation-mechanics demo, not a
real KMS.** Key material is 32 random bytes (`crypto/rand`) treated as an
AES-256-GCM key. The point of this variant specifically is demonstrating
that two independent, asynchronously-triggered state machines (a scheduled
timer and a random external revoke) can safely race for the same resource
as long as they agree on one lock, and that "the loser of the race always
acts on fresh state, never stale state" — not the actual cryptography.
