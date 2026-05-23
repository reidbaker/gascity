# HQStore Test City — Setup & Usage

> **Status:** Running (2026-05-23). Part of R3.1 (ga-aec8q.13).
>
> **Safety:** This city is completely isolated from the live `gc-management` city.
> Separate directory, separate dolt instance, separate beads database. No shared state.
> All risky HQStore work (shadow mode, migration, cut-over) runs here first.

## Quick reference

| Property | Value |
|---|---|
| Root directory | `/home/jaword/gctest` |
| City name | `hqtest` |
| Dolt port | **28533** (deterministic hash of `/home/jaword/gctest`) |
| Dolt data dir | `/home/jaword/gctest/.beads/dolt` |
| Beads prefix | `gc-` |
| Controller | supervisor-managed (PID of machine `gc supervisor`) |
| Live city dolt port | 28232 (`gc-management`) — never use this |

## Start / stop

The test city is registered with the machine supervisor and auto-reconciles.

```bash
# Status
gc status --city /home/jaword/gctest

# Trigger immediate reconciliation (after config changes)
HOME=/home/jaword gc supervisor reload

# Stop all agents in the test city (city stays registered)
gc config --city /home/jaword/gctest       # validate config first
# To fully tear down: gc stop /home/jaword/gctest (unregisters from supervisor)

# Access the beads store directly
gc bd --city /home/jaword/gctest list
gc bd --city /home/jaword/gctest stats
```

## Reproduced setup from scratch

If the city is gone and needs to be recreated:

```bash
# 1. Create directory and write city.toml / pack.toml
mkdir -p /home/jaword/gctest

# Write city.toml (see /home/jaword/gctest/city.toml for current content)
# Write pack.toml (see /home/jaword/gctest/pack.toml for current content)

# 2. Initialize (non-interactive, preserves existing config files)
rm /home/jaword/gctest/city.toml   # gc init errors if city.toml exists
gc init --provider claude /home/jaword/gctest
# Then restore city.toml from git or re-write it

# 3. The city is auto-registered with the supervisor during init.
#    The supervisor picks it up on the next reconcile tick:
HOME=/home/jaword gc supervisor reload

# 4. Verify
gc status --city /home/jaword/gctest
gc bd --city /home/jaword/gctest stats
```

> **Note:** `gc init` requires `city.toml` to not exist (it considers an existing
> `city.toml` as "already initialized"). Remove it, run `gc init`, then write your
> custom `city.toml` and reload the supervisor.

## Dolt port pinning

The dolt port **28533** is determined deterministically by `cksum` of the city path:

```bash
printf '%s' "/home/jaword/gctest" | cksum   # → 4016068533
# port = 4016068533 % 50000 + 10000 = 28533
```

This is stable as long as the city path doesn't change. To override explicitly:

```bash
GC_DOLT_PORT=28240 gc start /home/jaword/gctest
# Once the state file records port=28240, subsequent starts reuse it.
```

Live city ports to avoid: **28231** (gascity main) · **28232** (gc-management). Both are
clear of 28533.

## Agents

All agents have `min_active_sessions = 0` — they do NOT auto-start. Wake on demand:

| Agent | Purpose | Wake |
|---|---|---|
| `dog` (×2) | Generate bead/formula/session load for HQStore benchmarking | `gc agent start dog --city /home/jaword/gctest` |
| `claude` (×1) | Interactive: create test data, inspect state, seed beads | `gc agent start claude --city /home/jaword/gctest` |
| `control-dispatcher` | Built-in graph.v2 workflow worker (always-on) | auto |
| `mayor` | Available on demand (named session) | `gc agent start mayor --city /home/jaword/gctest` |

## Seeded data (initial state)

At city creation, the following coordination-state entities were seeded to provide
a realistic baseline for migration testing:

```
gc-3aw  control-dispatcher session bead
gc-ah9  mayor session bead
gc-5yq  test: session entity load             [in_progress, label=coordination-test]
gc-16n  test: wisp/mail entity load           [open, label=coordination-test]
gc-bak  test: order/formula entity load       [closed]
gc-dzu  test: parent convoy bead              [open, label=coordination-test]
gc-dzu.1  test: child step 1                  [open, child of gc-dzu]
gc-dzu.2  test: child step 2                  [open, child of gc-dzu]
gc-wisp-aaf  wisp-test-1                      [open, type=message, label=coordination-test]
```

Entities covered: **session beads**, **open/in-progress/closed beads**, **parent-child
hierarchies** (molecule-like), **message wisps**, **label filtering**.

## Adding more seed data

```bash
# Open bead
gc bd --city /home/jaword/gctest create --title "test: <title>" --label coordination-test

# Closed bead
gc bd --city /home/jaword/gctest create --title "test: <title>" && \
  gc bd --city /home/jaword/gctest close <id>

# Dependency chain
PARENT=$(gc bd --city /home/jaword/gctest create --title "parent" --json | jq -r '.id')
gc bd --city /home/jaword/gctest create --title "child" --parent "$PARENT"

# Message wisp
gc bd --city /home/jaword/gctest create --type message --title "wisp" --body "test wisp"

# Convoy
gc convoy --city /home/jaword/gctest create --title "test convoy" <bead-ids>
```

## Gotchas

1. **Long paths → Unix socket limits.** The city root `/home/jaword/gctest` is intentionally
   short. If you move the city, verify the new path + `.gc/controller.sock` is < 108 chars
   (AF_UNIX `sun_path` limit on Linux).

2. **`/tmp` is tmpfs on this host.** Do NOT store durable artifacts under `/tmp` — it's
   RAM-backed and cleared on reboot. The city root, dolt data, and docs live on `/home/jaword/`
   (durable btrfs). `/tmp` is fine for high-churn transient files if the setup is scripted.

3. **HOME override.** This session uses `HOME=/home/jaword/james-claude`. Commands that
   interact with the machine supervisor (status, reload) need the real HOME:
   `HOME=/home/jaword gc supervisor reload`.

4. **Port 28533 is a hash, not pinned.** It stays stable as long as the city path is
   `/home/jaword/gctest`. Moving the city changes the hash. Pin explicitly with
   `GC_DOLT_PORT=<port>` on first start if stability across path changes matters.

5. **No public PR.** Operator directive 2026-05-23: all coordination-store work lives on
   `experiment/coordination-store`. Do NOT open public PRs for this city or any work in it
   until the backend approach is chosen and documented.

## Isolation checklist

Before running any migration or cut-over command, verify:

```bash
# Live city is untouched
gc status --city /home/jaword/projects/gc-management | grep "dolt\|port\|28232"

# Test city state
gc status --city /home/jaword/gctest
cat /home/jaword/gctest/.gc/runtime/packs/dolt/dolt-state.json
# Should show port=28533, NOT 28231 or 28232

# No cross-city contamination
gc bd --city /home/jaword/gctest stats
gc bd --city /home/jaword/projects/gc-management stats
# Prefixes: gc- (test) vs ga- (live) — they use different Dolt databases
```
