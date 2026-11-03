# Migrating from any-claude to any-agents

any-agents replaces any-claude. It runs on the same any account and
server, keeps the same commands and object kinds, and adds collab
spaces: a team shares docs, tasks, memories and help requests between
their agents in a space every member joined. This is the migration
recipe for a member who runs any-claude today, plus the setup for a
member who starts fresh.

## What changes

| | any-claude | any-agents |
|---|---|---|
| binary | `any-claude` | `any-agents` (`~/.local/bin`) |
| private space | "Claude" | "Agents", created by `install --create-space` |
| team spaces | none | collab spaces, e.g. "Team Agents" |
| state dir | `~/.claude/any-claude/` | `~/.any-agents/` (`ANY_AGENTS_HOME`) |
| env prefix | `ANY_CLAUDE_*` | `ANY_AGENTS_*` |
| server address | `127.0.0.1:7001` | found from `server.addr` (the desktop app or `any run`); `install --addr` pins one |
| hooks | four any-claude hooks | four any-agents hooks; `install` drops the any-claude ones |
| commands | `doc`, `task`, `plan`, `session`, `search`, … | the same, plus `--space S` on most of them, `memory share`, `collab`, `space` |

The data model is also the same in spirit: memories, config, stats and
feedback are records on the app root; sessions, docs, tasks, plans,
projects, machines and Linear issues are pages filed under collections.
Humans read and write them in any-ui and the native clients.

## Prerequisites

- An any server built from main after 2026-10-27 (PR any#310, chat
  module open to client types). An older server can still join a team
  space and talk in collabs, but cannot open one; `install` says so and
  adopts the Collab bundle from the registry instead of registering it.
- A running any server. On a Mac that is the Any desktop app, which
  runs the server for you: do not start `any run` next to it. Without
  the app, `any run`. Either way the server records its address in the
  data dir's `server.addr` and any-agents finds it; only when the app
  listens somewhere any-agents cannot see, pin it with
  `any-agents install --addr host:port` (ask for the port, or read
  `~/.any/*/server.addr`).
- Go, to build both binaries.
- A clone of anyproto/any-agents and the any-claude checkout that has
  `migrate --to` (branch `feat/migrate-to-any-agents` of
  anyproto/any-claude until it is merged).

## Step 1: flush any-claude

In every repo you worked in recently, push what is still local so the
old space is complete before it is copied:

```
any-claude push
```

Closing the open Claude Code sessions does the same through the
session-end hook.

## Step 2: install any-agents and create the private space

On the first machine, in the any-agents clone:

```
make build && bin/any-agents install --create-space
```

`install` creates the "Agents" space, sets up the collections usecase,
installs the bundles, mints the machine page and the table views, copies
the binary to `~/.local/bin/any-agents` and rewrites the four hooks in
`~/.claude/settings.json`, dropping the any-claude entries. It is
idempotent; run it again after every `git pull`.

Check with `any-agents status`: it prints the identity, the private
space id and the bundles.

## Step 3: copy the old space

```
any-claude migrate --to Agents            # dry run: counts only
any-claude migrate --to Agents --apply    # the copy
```

The copy is a live read of the "Claude" space on the same server:

- records keep their ids: memories, config (minus the file blobs, which
  the next `any-agents push` re-attaches from your local files), stats,
  feedback;
- machines, projects and Linear issues are re-minted from their seeds;
- sessions and docs become pages under the any-agents collections, with
  relations, body links rewritten to the new ids, and transcripts
  re-attached;
- the app root, the table views and foreign objects are skipped.

The run is idempotent: an id map under the any-claude state dir records
what landed, so a second `--apply` only picks up what the first one
missed. The "Claude" space is only read. Keep it as the archive; nothing
needs it afterwards, but nothing is lost by leaving it.

Verify: `any-agents doc list --all`, `any-agents session list`,
`any-agents project list`. The counts should match the dry run.

## Step 4: reconnect the memory files

Memory files stay where Claude Code keeps them
(`~/.claude/projects/<slug>/memory/`), and their record ids
(`<repo>:<slug>`) did not change, so nothing moves on disk. In each
repo you use, run once:

```
any-agents pull
```

It adopts the files that match their records and regenerates
`MEMORY.md`. From then on the session-start hook pulls and the post-tool
hook pushes, as before.

## Step 5: join the team space

A member who is already in "Team Agents" mints an invite for you:

```
any invite create <spaceId>
```

You join with the token and register the space:

```
any join --token <token>
any-agents space add "Team Agents"
```

`space add` waits for the space metadata and the registry to sync, then
adopts the bundles (a writer installs what is missing). Afterwards:

- `any-agents space list` shows both spaces;
- the next session start mirrors the team's memories for the current
  repo into `memory/shared/<space>/` and lists them in `MEMORY.md`;
- auto-recall and `any-agents search` cover both spaces;
- `any-agents collab list --space "Team Agents"` shows the open help
  requests, and the session-start context says which ones are addressed
  to you.

## Step 6: other machines

Same account, same mnemonic (`any init --mnemonic`, never copy
`wallet.key`; with the desktop app, sign in there), then:

```
git pull --ff-only && make build && bin/any-agents install   # in the any-agents clone
any-agents space add "Team Agents"
```

No `--create-space`: `install` finds the existing "Agents" space. Run
the migration only once, from one machine; the other machines get the
copy through sync. Before the first `pull` on a new machine, let the space
finish syncing: `GET /v1/spaces/<spaceId>/sync-status` on the local any
server should report every object synced.

## What is shared and what stays private

Private by default. Nothing reaches a team space unless a command names
`--space "Team Agents"` or you reply on a collab there.

- **Memories.** Project knowledge for the repos the team works on lives
  in the team space. `any-agents memory share <slug>... --space "Team
  Agents" --move` publishes a private memory and retracts the private
  copy; without `--move` it publishes a copy. Your own shared entries are
  editable files under `memory/shared/<space>/` and sync on push; a new
  file written there becomes a new shared memory; other members' entries
  are read-only. Personal workflow notes and machine-specific findings
  stay private.
- **Docs, tasks, plans.** `--space "Team Agents"` on `doc add`, `task
  add` and friends files them in the team space; a doc there is a page
  every member reads in any-ui.
- **Collabs.** `any-agents collab open <file> --space "Team Agents"
  [--to member]` asks another member's agent for help; the other side
  sees it at session start, joins, answers with `collab say`, and closes
  with `collab done`. `--announce` posts the request into the space's
  general chat.
- **Feedback.** `any-agents feedback "…" --kind memory-wrong --ref <id>
  --space "Team Agents"` flags a stale shared memory for everyone.

## Fresh start without any-claude

Skip steps 1, 3 and 4: `install --create-space` on the first machine,
`install` on the others, then step 5. Memories accumulate from the
first session.

## Cleanup

After the copy is verified: remove the any-claude binary from
`~/.local/bin`, leave `~/.claude/any-claude/` until you no longer need
the migration id map, and keep the "Claude" space as the archive. The
any-claude hooks are already gone from `settings.json`.

## Known limits

- Shared memories are author-mutable: a teammate cannot edit your entry,
  only read it, publish their own slug, or file feedback.
- A server older than any#310 can join and talk in collabs but not open
  them; upgrade the server on the same data dir and everything works
  without a repair step.
- `migrate --to` copies one space into one space; run it once per
  account, not per machine.
