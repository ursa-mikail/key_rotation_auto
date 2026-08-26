# Key Rotation Console

A self-rotating key management demo: a TypeScript frontend generates a
genesis key, a Go backend rotates it every N seconds via an HKDF chain,
every rotation is gated by a live decrypt test across the whole key
history, Postgres is the single source of truth, and Terraform reacts
to (never drives) each rotation by consuming key *metadata* only.

```
[TS/Vite frontend] --REST/SSE--> [Go backend] --SQL--> [Postgres]
                                       |
                                       +--writes--> rotation.auto.tfvars.json
                                                          |
                                              [terraform-runner sidecar]
                                              (hash-gated `terraform apply`)
                                                          |
                                                          +--writes--> current-key-reference.json
```

This is **two independent loops that never call each other**, connected only by a
file. That separation is deliberate — see "Two loops, not one circular loop"
below for why a single loop where Terraform triggers Go (or vice versa) would
fight itself.

## Quick start

```
./up.sh
```

Then open **http://localhost:5173**, click **Generate Genesis Key**,
and watch the key chain rotate live. Useful endpoints:

- Frontend: http://localhost:5173
- Backend status API: http://localhost:8080/api/status
- Backend realtime stream: http://localhost:8080/api/events (SSE)
- Postgres: `localhost:5432`, user/db `keyrotate` / `keyrotate`

```
./down.sh    # stop containers, keep all data/volumes
./clean.sh   # stop containers AND wipe volumes (asks for confirmation)
```

## The simple version of the loop

If you just want "what actually happens after I click Generate," here
it is with the roles kept straight — the part most worth being precise
about is **who watches the clock and who watches the file**:

```
you click "Generate Genesis Key"
        │
        ▼
Go writes the FIRST key straight into Postgres (status = primary)
        │
        ▼
┌───────────────────────── every 1 second, forever ─────────────────────────┐
│  Go asks POSTGRES (not the json file) "is a rotation due yet?"            │
│  (due = now() - last_rotated_at >= interval_seconds, e.g. 10s)            │
│                                                                            │
│  not due yet?  → do nothing, ask again in 1s                              │
│  due?          → rotate INSIDE Postgres: derive next key (HKDF),          │
│                  live-decrypt-test every key, retire old primary,         │
│                  promote new one, commit                                  │
│                       │                                                   │
│                       ▼                                                   │
│         ONLY AFTER that commit succeeds, Go OVERWRITES                    │
│         rotation.auto.tfvars.json  with the new key's metadata            │
└────────────────────────────────────────────────────────────────────────┘
        │
        │ (a file on a shared volume — no function call, no HTTP,
        │  no signal; the terraform-runner sidecar just polls it)
        ▼
┌────────────────── every 2 seconds, forever, in a DIFFERENT process ───────┐
│  terraform-runner hashes rotation.auto.tfvars.json                        │
│  same hash as last time? → do nothing                                     │
│  different?              → terraform apply, write current-key-            │
│                             reference.json, remember the new hash         │
└────────────────────────────────────────────────────────────────────────┘
```

The one correction worth calling out explicitly, because it's an easy
mental model to reach for and it's backwards: **it is not** "the json
file expires, which updates the tf file, which triggers Go to rotate
the key in Postgres." Nothing ever watches the json file for
*expiry*, and nothing the sidecar does ever calls back into Go.
Expiry is checked exactly once, in Postgres, by Go, on its 1-second
tick. The json file is purely an *outbound receipt* of a rotation that
already happened — the sidecar's 2-second poll only asks "did the
receipt change," never "is it time." One clock (Postgres), one
consumer of its output (Terraform), one direction.

## Two loops, not one circular loop

A natural first instinct is to write this as one big loop: "check the
json for expiry → update it and trigger terraform → terraform triggers
Go to rotate the key → Go updates Postgres and the json → repeat."
**That's backwards, and the backwards part is specifically Terraform
triggering Go.** Two problems with it:

1. **Terraform has no clock and no lock of its own.** `terraform
   apply` runs once and exits — it isn't a long-running process that
   can "check expiry" every second. Making it do that means wrapping
   it in a `local-exec` provisioner on a timer, which re-implements
   the timing logic Go+Postgres already does correctly (a
   transaction plus an advisory lock), giving you two independent
   clocks that can drift apart.
2. **"Terraform triggers Go" closes a circle with no exit
   condition.** Go writes the json → that triggers Terraform →
   Terraform triggers Go → Go writes the json again → forever.
   Terraform's plan/apply model has no native way to say "stop,
   nothing changed" about a system it doesn't own.

The fix is to **not close the loop** — run two loops that never call
each other, connected only by a file on disk:

```
 LOOP A — owns time, lives in Go + Postgres           LOOP B — reactive, lives in the terraform-runner sidecar
 ┌──────────────────────────────────────┐             ┌──────────────────────────────────────────┐
 │ every 1s:                            │             │ every 2s:                                 │
 │  1. read last_rotated_at from        │             │  1. hash rotation.auto.tfvars.json         │
 │     Postgres                         │             │  2. compare to the last APPLIED hash       │
 │  2. due? if not, do nothing          │   writes    │  3. same? do nothing                       │
 │  3. lock, rotate, run the live       │──────────►  │  4. different? terraform apply, then       │
 │     decrypt test, all in one tx      │  (file on   │     write the new hash + output file        │
 │  4. commit, THEN write               │   a shared  │                                            │
 │     rotation.auto.tfvars.json        │   volume)   │  (never calls back into Go or Postgres)     │
 └──────────────────────────────────────┘             └──────────────────────────────────────────┘
```

Go never knows or cares whether Terraform has applied anything.
Terraform never knows or cares why the file changed. Each loop is
independently idempotent (see below) using only tools that loop
actually has — Postgres transactions on the Go side, a content hash on
the Terraform side — instead of trying to coordinate across the
boundary.

Terraform is a **consumer of a fact** ("here is the current keyset"),
never a **decider of when to rotate**. The demo's `terraform/main.tf`
writes that fact with a `local_file` resource as a stand-in; a real
cloud deployment would swap in something like a real `ursa_keyset`-
shaped resource (or `aws_secretsmanager_secret_version` /
`aws_kms_alias`, whatever your actual key-management platform exposes)
keyed off the same three variables (`id`, `type`, `keys`) — the loop
shape doesn't change, and neither does the direction: **a bigger,
list-shaped variable is still just data Terraform reads, not a hook
that calls back into Go.** See "Does the tf-config shape trigger Go?"
below for the direct version of that answer. Raw key material is
never written into `.tf` files, tfvars, or Terraform state; it only
ever lives in Postgres.

## Does the tf-config shape trigger Go?

No — and the point of this section is to be precise about *why not*,
since it's a reasonable thing to wonder once `var.keys` grew from
three scalars into a list that looks a lot more like "real state."

A bigger variable doesn't create a new call path. `resource
"local_file" "current_keyset"` (or the commented-out real
`ursa_keyset` block above it) has no `provisioner`, no webhook, no
`local-exec`, nothing that reaches back into the Go process or
Postgres — it only ever *writes a file Terraform itself owns*. The
only thing that decides when `terraform apply` runs at all is
`apply-loop.sh`'s own 2-second poll of `rotation.auto.tfvars.json`'s
hash, a decision made entirely inside the sidecar container, using
only a hash it computed itself. Terraform never inspects
`rotation_state`, never opens a connection to Postgres, and has no way
to know a rotation is "due" even if it wanted to.

If `ursa_keyset` (or its real equivalent) *did* trigger Go — say, via
a provisioner that called `/api/rotation-hook` after every apply —
that would recreate exactly the forever-loop problem from "Two loops,
not one circular loop" above: Go writes the json → Terraform applies →
Terraform calls Go → Go writes the json again → forever, with no
native way for Terraform's plan/apply model to say "stop, nothing
changed" about a system it doesn't own. Growing `var.keys` into a list
doesn't change that risk calculus at all; it was never about how much
data crosses the boundary, only about which direction the *call*
goes. This design keeps that direction fixed at exactly one way.



Both loops exchange exactly two files, plus one small marker, all on
**one shared Docker volume** (`shared-data`, mounted at `/shared` in
both the `backend` and `terraform-runner` containers):

| File | Written by | Path inside containers | What it contains |
|---|---|---|---|
| `rotation.auto.tfvars.json` | Go, after every committed rotation (and after genesis) | `/shared/rotation.auto.tfvars.json` | `id`, `type`, and `keys` — the full current keyset (one entry per key that still exists: `label`, `length`, `status`, `primary`), metadata only, never key material |
| `.last-applied-hash` | `apply-loop.sh`, after every successful `terraform apply` | `/shared/.last-applied-hash` | a sha256 hex string, nothing else |
| `current-key-reference.json` | Terraform's `local_file` resource | `/shared/terraform-output/current-key-reference.json` | the same `id`/`type`/`keys` shape, echoed back out from Terraform state |

A fourth file is mounted **read-only**, separately from the shared
volume above, purely for display: `terraform/main.tf` itself, mounted
into the backend container at `/tf-config/main.tf` (see
`TF_CONFIG_PATH` below). It's the static source that produces the two
snapshot files — the backend never writes to it, and it isn't part of
either loop's runtime data flow.

**Why you couldn't see them in the original version:** those files
were real and being written correctly inside the containers, but
nothing ever read them back out to the browser — the frontend only
ever showed the Postgres-derived key chain, never the files
themselves. That's fixed now two ways:

1. **In the UI** — the "Live files" section on the dashboard has two
   tabs, `rotation.auto.tfvars.json` and `terraform output`, each
   showing the live file content and a "last updated" time. They
   refresh automatically over the same SSE stream that drives the key
   chain table — no manual refresh needed. A **sync status** badge
   tells you whether Terraform has caught up to the latest rotation
   yet (compares the current tfvars hash against `.last-applied-hash`
   under the hood).
2. **On the host machine**, if you want to look directly instead of
   through the UI:
   ```
   docker compose exec backend sh -c 'cat /shared/rotation.auto.tfvars.json'
   docker compose exec backend sh -c 'cat /shared/terraform-output/current-key-reference.json'
   ```
   (Either container works, since both mount the same volume.)

## How the json actually updates the tf file, step by step

1. Go's rotation transaction commits in Postgres (new key promoted to
   `primary`, old one `retired` — never deleted).
2. Only *after* that commit succeeds (or after genesis, for the very
   first key), Go re-reads the **full** key list from Postgres and
   writes `/shared/rotation.auto.tfvars.json` — a plain `-var-file` for
   Terraform containing `id`, `type`, and `keys`, where `keys` has one
   entry per key that currently exists (`label` = its creation
   timestamp, `length` = 256, `status` = `"ENABLED"`, `primary` = true
   for exactly one entry). This is a full overwrite each time (atomic,
   via a temp file + rename), not an append to the *file* — see
   "Idempotency with a list-shaped variable" below for why that's
   still correct even though the list itself grows every rotation.
3. The `terraform-runner` sidecar (`terraform/apply-loop.sh`) polls
   that file every 2 seconds, hashes it, and compares the hash to
   `/shared/.last-applied-hash`.
4. If the hash changed, it runs
   `terraform apply -auto-approve -var-file=/shared/rotation.auto.tfvars.json`
   against `terraform/main.tf`.
5. Terraform reads those three variables and updates its one resource,
   `local_file.current_keyset`, rewriting
   `/shared/terraform-output/current-key-reference.json` with the same
   `id`/`type`/`keys` shape. (This is the literal stand-in for
   "Terraform adds the key" — in a real deployment this resource would
   instead be something like the commented-out `ursa_keyset` block in
   `main.tf`, or `aws_secretsmanager_secret_version`, pointing a real
   secret's active version at the new key.)
6. On success, `apply-loop.sh` writes the just-applied hash into
   `/shared/.last-applied-hash`. That's what flips the UI's sync badge
   to "in sync."

## Idempotency with a list-shaped variable

Switching `var.keys` from three scalars to a growing list raises a
fair question: doesn't a file that gets bigger every rotation break
the "same input → same output → skip the apply" idempotency the
hash-compare loop depends on? It doesn't, for two separate reasons:

- **The file is still a pure, deterministic function of Postgres's
  current state.** `writeTerraformVars` builds `keys` from
  `listKeys()`, which orders by `generation ASC` — a stable, repeatable
  order — and only 'primary'/'retired' rows are ever durably committed
  (a failed liveness test rolls its whole transaction back, so no
  half-created 'pending' row ever survives to be read). Call it twice
  against the *same* committed DB state and you get byte-identical
  JSON both times, because `json.MarshalIndent` on a Go struct with
  fixed field order and a fixed slice order has no hidden
  nondeterminism (no map iteration, no timestamps generated at write
  time). That's the only property `apply-loop.sh`'s hash compare
  actually needs — it was never "the file must be small," only "the
  file must be a pure function of state."
- **Growing the list is exactly what makes each rotation produce a
  genuinely different hash**, which is correct, not a bug: a real
  rotation *did* change Postgres's state (one more key exists, the
  primary flag moved), so the tfvars file *should* change and *should*
  get applied. The hash-compare gate's job was always to skip applies
  when nothing changed (not due yet, lock lost, same window already
  handled) — not to skip applies when something legitimately did.

One real caveat worth stating plainly rather than glossing over: this
demo's rotation interval is 10 seconds and nothing prunes `var.keys`,
so a multi-hour run will genuinely accumulate hundreds of entries in
both the tfvars file and Terraform's state file. That's fine for a
demo meant to be watched rotate a few times, but it's not how you'd
want a production keyset represented — a real `ursa_keyset`-style
resource (or `aws_secretsmanager_secret_version`) would have its own
native versioning API on the provider side instead of asking Terraform
to carry the entire history in one attribute forever. Nothing in this
demo's *architecture* depends on the list staying small (idempotency
holds regardless of length), it's purely an operational sizing
concern you'd solve differently in a real deployment.

## Rotation kill switch

`rotation_state.paused` is a plain boolean, but it's read inside the
*same* `FOR UPDATE`-locked transaction that decides whether a rotation
is due (see `tryRotate` in `rotation.go`), right after the due-time
check's row lock is acquired and before any key material is derived.
That placement is what makes it safe rather than just "a flag Go
happens to check somewhere":

- It's authoritative in Postgres, not an in-process Go variable, so it
  survives a backend restart and applies identically to every replica
  if you ever run more than one — there's no separate "tell the other
  replicas" step.
- It can't race a rotation that's already past its due-check for this
  tick: the pause request either lands before that `SELECT ... FOR
  UPDATE` (this tick is skipped) or after (this tick already committed;
  the pause takes effect starting next tick). No tick can be
  "half-paused."
- It only stops Loop A from *starting new* rotations. It has no effect
  on Loop B — the terraform-runner sidecar keeps polling and would
  still apply any already-written tfvars file. There's nothing new for
  it to apply, since Loop A is what produces that file, so in practice
  it goes idle too, but that's a consequence, not something the pause
  flag reaches into Loop B to enforce.

API: `POST /api/rotation/pause` (optional `{"reason": "..."}"` body)
and `POST /api/rotation/resume`. The dashboard's "Stop auto-rotation"
button under Live Status calls these and reflects the resulting state
(`paused`, `pausedAt`, `pausedReason`) on the next status tick — same
SSE stream as everything else, no separate polling loop for it.

## Closing the genesis re-click gap, with a server-side audit trail

The frontend already disabled the Generate button for the duration of
one request, but that alone doesn't *trace* anything — it only
prevents a second click from the same tab while the first request is
in flight, and it proves nothing about what the backend actually saw
(a second tab, a slow first response racing a fast second one, a retry
after a dropped connection, etc. are all still possible). Two things
now close that gap for real:

1. **Correctness was already handled at the data layer**, and stays
   there: `one_primary_key` is a partial unique index
   (`WHERE status = 'primary'`), so Postgres itself guarantees at most
   one genesis attempt can ever win, no matter how many concurrent
   requests arrive. `handleGenesis` checks for an existing primary key
   before inserting (fast path), and still handles a `23505` conflict
   from the insert itself (the actual race-safe path) by returning
   `409` rather than a generic error.
2. **What was missing was visibility**, not correctness — and that's
   what `genesis_attempts` adds. Every call into `handleGenesis` now
   writes one row via `logGenesisAttempt`, on *every* return path:
   invalid input, "already-initialized" no-op, the `23505` race loss,
   any DB error, and success. The row is written with the server's own
   `received_at` timestamp, so the ordering of concurrent attempts is
   whatever Postgres actually saw, not whatever order the browser
   *thinks* it sent them in. The dashboard's "Genesis audit trail"
   table reads this back, newest first, over the same SSE stream.

So: correctness doesn't come from the audit trail (it never did — it
came from the unique index), and the audit trail doesn't add
correctness. What it adds is that a re-click, a race, or a rejected
attempt now shows up as a row someone can look at, instead of as a
client-side error message that vanished the moment the tab closed.

## Snapshot vs. history — and why key versions aren't "appended" in the json

This is really the answer to "shouldn't key versions be updated by
appending, not overwritten" — the short version is: **they are
appended, just not in the json files, on purpose.**

There are two genuinely different kinds of state in this system, and
they're deliberately handled with opposite strategies:

| | `rotation.auto.tfvars.json` / `current-key-reference.json` | `keys` + `rotation_events` tables |
|---|---|---|
| What it represents | "What is true **right now**" — even though `keys` inside it now lists every key, the *file itself* is a full snapshot taken at write time, not a log | "Everything that has **ever happened**" |
| Write strategy | Full overwrite (temp file + rename) every time — the whole file, including its `keys` array, is rebuilt from scratch on each write, never edited in place | `INSERT` only — a retired key's row is updated to `status = 'retired'`, never deleted; `rotation_events` rows are inserted once and finalized once |
| Why | Terraform's `-var-file` contract wants Go to hand it one complete, current description of desired state per apply — there's no "append these 3 new list entries to my last apply," only "here is the whole list again, diff it yourself." A real backing resource (`aws_secretsmanager_secret_version`, a real `ursa_keyset`) works the same way: you declare the desired full state and the provider computes what changed | This is the actual key material history — every key ever derived stays decryptable forever (safety measure 5), and every rotation attempt (including skipped/failed ones) stays in `rotation_events` forever, as individual rows Postgres never rewrites wholesale |
| Exposed in the UI | The `rotation.auto.tfvars.json` / `terraform output` tabs (snapshots — now list-shaped, but still rewritten whole) | The new **history** tab (append-only, newest first) and the **Key Chain** table above it |


Put differently: appending was never missing from the *system* — the
`keys` table has always retired-not-deleted, and `rotation_events` has
always been insert-only (see safety measures 1 and 5, further down).
What was missing was a place in the UI to actually *see* the appended
history, since the only file-backed views were the two snapshots. The
new **history** tab fixes exactly that gap by reading
`rotation_events` directly — same data Postgres has always kept, now
visible without a `docker compose exec` + manual SQL query.

## Idempotency and safety measures

Measures 1–6 belong to Loop A (Go + Postgres); measure 7 belongs to
Loop B (the Terraform sidecar); measure 8 covers restarts of either.

1. **Deterministic rotation IDs.** Each rotation attempt computes
   `sha256(primary_key_id + rotation_time_window)`. A duplicate attempt
   for the same window collides on `rotation_events.rotation_id`
   (a primary key) and is rejected by Postgres itself — idempotency
   lives in the schema, not in "don't call this twice" discipline.

2. **Advisory lock per tick.** If more than one backend replica ever
   runs, `pg_try_advisory_lock` ensures only one of them evaluates and
   performs a rotation on any given tick; the rest no-op that tick.

3. **One transaction per rotation.** Insert the new key → run the live
   decrypt test → retire the old primary and promote the new one, all
   inside a single `BEGIN…COMMIT`. Any failure rolls back the whole
   thing — no half-rotated state is ever observable.

4. **Verify before promote, never the reverse.** The new key is
   inserted as `pending`. Every key that has ever existed (nothing is
   deleted on rotation) must decrypt its own stored test vector before
   the transaction is allowed to retire the old primary and promote
   the new one.

5. **Old keys are retired, never deleted.** "Rotating" adds a key and
   changes which one is primary; it never destroys key material.
   Anything encrypted under a retired key stays decryptable.

6. **DB clock, not wall clock.** "Is a rotation due" is computed as
   `now() - last_rotated_at >= interval` using Postgres's own `now()`
   inside the same transaction that reads `last_rotated_at`, so replica
   clock skew can't cause double or missed rotations.

7. **Content-addressed Terraform apply.** `apply-loop.sh` hashes
   `rotation.auto.tfvars.json` and only runs `terraform apply` when the
   hash changes from the last *successful* apply. A failed apply does
   not advance the stored hash, so the next poll retries automatically
   instead of drifting out of sync.

8. **Crash recovery is just a normal read.** On restart, the backend
   doesn't reconstruct state from memory — it reads the current
   primary key and `last_rotated_at` straight from Postgres, so a
   restart mid-rotation is invisible to correctness (worst case: that
   window's rotation is retried on the next tick, and the deterministic
   rotation ID makes that safe).

## Is the 1s / 2s / 10s timing correct?

Yes, but the reason it's correct is the *ratios* between the three
numbers, not their absolute values — so it's worth understanding
before you change any of them:

- **Go's tick (1s, fixed in code) must be smaller than the rotation
  interval (10s, configurable).** The tick is just polling resolution
  — "how often do we check whether it's time yet" — not the rotation
  cadence itself. If you ever made the tick *larger* than the
  interval, you could sleep through a due rotation and rotate late but
  correctly; you'd lose precision but nothing would break. Don't go
  the other direction and make the interval *smaller* than the tick —
  at some point you'd want sub-second rotation, and this design
  doesn't support that without lowering the tick too.

- **The sidecar's poll (2s, configurable) should be smaller than the
  rotation interval (10s), with headroom.** If `POLL_SECONDS` were
  larger than `interval_seconds` (say, polling every 15s against a
  10s rotation), Terraform would still catch up eventually — the hash
  compare is exact regardless of timing — but the UI's "in sync" badge
  would lag visibly behind the actual key chain, and a fast demo would
  look broken even though nothing is. Keep the poll interval at
  roughly a quarter to a half of the rotation interval so the UI feels
  responsive.

- **Nothing here needs the two loops' timings to line up exactly**,
  which is the actual point of decoupling them. Loop A rotating every
  10s and Loop B polling every 2s just means Terraform typically
  notices a change on its first or second poll after it happens — nice
  for a demo, but not a correctness requirement. You could set
  `POLL_SECONDS=30` and the system would still be correct, just
  slower to visibly react.

If you tighten `interval_seconds` for a livelier demo, tighten
`POLL_SECONDS` proportionally too, and keep both well above 1s (Go's
fixed tick) so you don't lose the safety margin described above.

## Project layout

```
db/init.sql          Postgres schema (keys, rotation_events, test_vectors, rotation_state)
backend/              Go rotation service + REST/SSE API
frontend/             TypeScript/Vite console (genesis key generation, live dashboard)
terraform/            main.tf + apply-loop.sh sidecar, reacts to key metadata
docker-compose.yml    Wires it all together
up.sh / down.sh / clean.sh
```

## Configuration

| Variable | Where | Default | Purpose |
|---|---|---|---|
| `interval_seconds` (in `rotation_state` table) | Postgres | `10` | How often keys actually rotate. Change with `UPDATE rotation_state SET interval_seconds = 5;` |
| `SHARED_DIR` | backend, terraform-runner | `/shared` | Volume where `rotation.auto.tfvars.json` is exchanged |
| `POLL_SECONDS` | terraform-runner | `2` | How often the sidecar checks for a changed tfvars hash |
| `TF_CONFIG_PATH` | backend | `/tf-config/main.tf` | Read-only mount of `terraform/main.tf`, shown in the UI's tf-config tab |
| `rotation_state.paused` (kill switch) | Postgres | `false` | Set via `POST /api/rotation/pause` / `POST /api/rotation/resume`, or directly with `UPDATE rotation_state SET paused = true;` |

## API surface

| Endpoint | Method | Purpose |
|---|---|---|
| `/api/genesis` | POST | Create the generation-0 key (idempotent; logs to `genesis_attempts` on every outcome) |
| `/api/status` | GET | Full dashboard payload (keys, kill-switch state, both file snapshots, tf-config, history, genesis audit trail) |
| `/api/events` | GET (SSE) | Same payload as `/api/status`, pushed roughly once per second |
| `/api/keys` | GET | Just the key chain |
| `/api/rotation/pause` | POST | Engage the kill switch; optional `{"reason": "..."}"` body |
| `/api/rotation/resume` | POST | Disengage the kill switch |
| `/api/healthz` | GET | DB connectivity check |

The backend's own tick is fixed at 1 second (the "timecheck"), but it
only performs a rotation once `interval_seconds` have actually elapsed
— the 1s tick is polling resolution, not the rotation cadence.

## Notes on the demo crypto

Key "rotation by adding to it" is implemented as an HKDF-SHA256 step
over the previous key material, salted with the rotation timestamp —
a proper KDF chain, not literal byte concatenation (which would leak
structure and not increase effective entropy). Each key is AES-256,
and the "live test" is an AES-GCM encrypt/decrypt round-trip against a
fixed plaintext, one stored ciphertext per key generation.
