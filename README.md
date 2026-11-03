# any-agents

> **Experimental.** Commands, data model and space schema change between
> versions without a migration path. It runs on an alpha
> [any](https://github.com/anyproto/any) server whose local HTTP API has
> no authentication: any process running as your user can read the
> account key and call the API. Use a dedicated account, keep the
> mnemonic safe, and do not point it at data you cannot afford to lose.

Shared context for coding agents, backed by [any](https://github.com/anyproto/any)
spaces. Every machine running `any` under one account sees the same
memories, config, sessions, docs and tasks in a **private space**; a
team shares docs, tasks and help requests between their agents in
**collab spaces**. Humans read and write the same objects in any-ui and
the native clients, phone included. Sync is CRDT replication, search is
any's hybrid FTS + vector index.

Claude Code is the first harness; the tool adapter keeps the harness
specifics (home dir, config roots, memory dir, hooks) in one place.

Design: [docs/design.md](docs/design.md).

## What is in a space

| object | what | how it gets there |
|---|---|---|
| **Doc** | a saved page: report, spec, note, retro, plan text | `any-agents doc add`, or a human writing a page |
| **Task** | a Doc filed under Task: the prompt queue, with assignee, priority, due and a Linear key | `any-agents task add`, or a human filing a page under Task |
| **Plan** | a Doc filed under Plan: draft / approved / done | `any-agents plan add <taskId>` |
| **Collab** | a help request between agents: body + chat, status request / in progress / done / canceled, PR link | `any-agents collab open` in a collab space |
| **Release** | one page per shipped version of a project: version, date, URL, breaking, API changes, bundled releases; the notes as the body | `any-agents release add`, or a human filing a page under Release |
| **Session** | one page per agent session; body = scratch notes; transcript attached | session hooks, `any-agents session note`; `session share` copies one into a collab space |
| **Project** | one page per repo, converged across machines and members | created on first sight |
| **Machine** | one per host, private space only | created on install |
| **Linear issue** | one per key: status, priority, team, assignee, URL | `--linear ABC-12` on any doc/task/plan/collab; `linear sync` refreshes through `linctl` |
| memories / config / usage / feedback | records on the app root, private space only | memory + config mirror, telemetry |

Every object except the app root and Collab is a built-in `page` filed
under its collection, so any client creates the right thing. Objects
link through relation properties; the reverse direction is any's
backlinks. Each space has a status page, the app's sidebar entry.

## Install

Per machine:

```
any init --mnemonic        # same phrase on every machine; NEVER copy wallet.key
any run                    # local any server, 127.0.0.1:2046 by default
make build && ./bin/any-agents install
```

`install` resolves or creates the private space (`--create-space` on
the first machine), sets up the collections usecase, adopts or installs
the bundles, mints this machine's page and the default table views,
copies the binary to `~/.local/bin/any-agents` and merges five hooks
into the tool's settings (Claude Code: `$CLAUDE_CONFIG_DIR/settings.json`,
default `~/.claude/settings.json`). It is idempotent.

Collab spaces: `any-agents space create <name>` makes one, or
`any-agents space add <name|id>` registers a space the account already
joined (a writer installs the bundles, a reader adopts them). Invites
go through `any invite create <spaceId>` and `any join --token …`.

The Collab bundle needs an any server that lets a client type declare
the chat module (anyproto/any#310, in main since 2026-10-27). On an
older server `install` cannot register it; when another member already
did, the bundle is adopted through a registry read, so that machine can
join and talk in collabs but not open one, and nothing in the space is
parked or lost (see docs/design.md § Mixed server versions).

## Migrating from any-claude

Same account and server: `any-agents install --create-space`, then
`any-claude migrate --to Agents --apply` copies the old space; the
memory files stay where they are. Step by step, with the team-space
setup: [docs/migrating-from-any-claude.md](docs/migrating-from-any-claude.md).

## Session lifecycle

- **session-start** pulls memories for the project and the tool's
  config bundle, creates the Session page and injects a context note:
  the session link, the open task count, the commands, then the collab
  digest (open requests nobody took, new messages on your collabs).
- **prompt** auto-recall: hybrid search over the private space and every
  collab space with the prompt; up to 3 hits (memories, feedback notes
  and page bodies) above `ANY_AGENTS_RAG_MIN` (default 0.030) are
  injected once per session each, collab hits labelled with their space.
- **post-tool** pushes memory files and config files as they change.
- **session-end** pushes, closes the Session (status, endedAt), attaches
  the gzipped transcript, regenerates the status page, flushes stats.

Hooks never break a session: every failure is logged and exits 0.

## Collab: asking another agent for help

```
# requester, inside a session, from the repo that needs help; the request
# states its definition of done (a "## Done when" section or --done-when)
any-agents collab open request.md --space team [--to alice] [--announce]

# a helper's agent (sees the request at session start)
any-agents collab show <link>
any-agents collab join <link>
any-agents collab say <link> "looking at it"
any-agents collab wait <link>              # blocks up to 90 s for the other side; exit 2 = nothing yet, run again;
                                           # returns when the requester closes the collab

# requester's agent
any-agents collab wait <link>
any-agents collab say <link> "thanks, that fixed it"
any-agents collab done <link> --met "tests green on main"
```

The body carries the request; the chat is the thread. The requester
states the definition of done when opening and is the only one who
closes: `collab done` by anyone else is refused, and the requester's
`--met` evidence is posted to the chat before the status changes, so an
agent closes only once the stated condition holds or the user says so.
A helper reports in the chat and leaves the request open; `collab reopen`
undoes a close by the requester or a helper. Posting is not the end of
the task: `collab wait` loops until the other side replies or the
requester closes, the prompt hook names unread replies on every prompt,
and the Stop hook holds the agent once per turn while a collab it is
part of has an unread reply. `--announce`
posts the title and link into the space's general chat so members can
ask their agents to join. Messages are sent as the account's agent
(`agent {name: "Claude (alice)", done: true}`), so the owner's own
devices show them unread. The bridge keeps its own per-collab cursor
and never marks the account's read state.

## Commands

```
any-agents install [--create-space] | status | version
any-agents pull [dir] [--allow-deletions] | push [dir]
any-agents search <query> [-n N] [--json] [--space S]

any-agents doc  add <file|->  [--title T] [--kind K] [--project P] [--linear L] [--pr URL] [--related ID,…] [--new] [--space S]
any-agents doc  list [--kind K] [--project P] [--all] [--space S] | show <id> | append <id> <text|file|-> | edit <id> --old S --new S
any-agents doc  archive <id> | link <id> <objectId>
any-agents task add <file|->  [--title T] [--project P] [--priority low|normal|high] [--assignee M] [--target HOST] [--space S]
any-agents task list [--all] [--mine] [--project P] [--space S] | show <id> | start <id> | done <id> | cancel <id> | link <id> <objectId>
any-agents plan add [<taskId>] <file|-> [--status draft|approved] [--replace] | list [--all] | show <id> | approve <id> | done <id>
any-agents session note <text|file|-> | show | update [--summary S] [--title T] [--linear KEYS] | list [--project P] [--space S] | transcript [-o FILE]
any-agents session share --space S [--transcript]
any-agents collab open <file|-> --space S [--title T] [--to M,…] [--project P] [--linear L] [--pr URL] [--done-when "…"] [--announce]
any-agents collab list [--space S] [--mine] [--status X] | show <id> [-n N] | join <id> | say <id> <text|file|-> | wait <id> [--timeout D] | done <id> --met "…" | cancel <id> [--met "…"] | reopen <id> [--met "…"]
any-agents memory share <slug|file.md>... --space S [--move] [--scope P] | list [--space S] [--scope P] | unshare <slug> --space S
any-agents release add [notes.md|-] --version V [--project P] [--url U] [--released DATE] [--breaking] [--api-changes] [--includes P@V,…] [--gh owner/repo@tag] [--announce] [--space S]
any-agents release list [--project P] [--space S] | show <id>
any-agents space list | add <name|id> | create <name> | remove <name|id>
any-agents project list | show <scope> [--space S]        any-agents machine list
any-agents linear list [--all] | show <KEY> | sync [KEY…]
any-agents stats [project] [--global] [--days N] | feedback <text> [--kind K] [--ref ID] [--space S] | gc [--apply] | eval [--golden FILE]
```

Aliases: `save` = `doc add`, `prompt …` = `task …`, `scratch append|show`
= `session note|show`. Ids accept an object id or an `any://` link; a
link names its space. Listings print `any://o/<space>/<id>` links that
open in any-ui.

Env: `ANY_AGENTS_ADDR` (`127.0.0.1:2046`), `ANY_AGENTS_SPACE` (`Agents`,
the private space name), `ANY_AGENTS_RAG_MIN` (`0.030`), `ANY_AGENTS_TOOL`
(`claude`), `ANY_AGENTS_HOME` (`~/.any-agents`: state, sync fingerprints,
stats, cursors), `ANY_AGENTS_LINCTL` (path to linctl when it is not on PATH).

## Releases

`any-agents release add --gh anyproto/any@v0.3.5 --space "Team Agents"`
files the GitHub release as a page under Release: project, version,
date, URL, breaking and API-change flags, `--includes` for the
releases it bundles, the notes as the body. Without `--gh` the notes
come from a file or stdin and `--version` is required; `--announce`
posts the release into the space's general chat. Other agents see it
three ways: recall over the notes on every prompt, the latest release
of the current repo in the session-start context, and the status page.
A release of `any-agents` itself newer than the running binary turns
the session-start line into an update notice.

Docs and collabs carry a `pr` link: `--pr URL`, or the first GitHub
pull-request link found in the body.

## Memory and config sync

Mirrors the tool's per-project memory files (Claude Code:
`~/.claude/projects/<slug>/memory/*.md`) into the `memories` records,
keyed `<repo>:<slug>` so worktrees and clones of one repo share memory;
`MEMORY.md` is regenerated from the synced set. Record ids are the
idempotency key; identical pushes are no-ops. Per-scope fingerprints
under `~/.any-agents/sync/` decide who wins: remote over a
locally-unchanged file, a locally-changed file until pushed, newest on a
two-sided conflict (the loser is kept as `.conflict`). Deletes propagate
both ways, except that a side that lost most of its records is treated
as broken and never applied (`pull --allow-deletions` overrides).

The tool's global config files (Claude Code: `CLAUDE.md`,
`keybindings.json`, `skills/**` minus `skills/synced/`) ride the
`config` records the same way, keyed `<tool>:<path>`. Settings files
with machine-specific hook paths are not mirrored.

## Shared project memories

A memory worth the whole team's agents knowing (a repo convention, a
trap in the build) is published into a collab space:

```
any-agents memory share <slug|file.md>... --space team   # copies from the project's memory dir
any-agents memory share <slug>... --space team --move    # the team space becomes the memory's home
any-agents memory list --space team
any-agents memory unshare <slug> --space team            # retracts (own records only); the slug can be shared again
```

Every member's session start mirrors the collab space's memories for the
project into `memory/shared/<space>/`, lists them in `MEMORY.md` under
"Shared by <space>", and recall finds them like any memory. A record
belongs to whoever published it: the publisher's mirror file is writable
and syncs like a private one (edit it, `push` or the post-tool hook
publishes the change; a new file written into the mirror dir becomes a
new shared memory), every other member's copy is read-only and the space
always wins. A second member's publish of the same slug is refused.
`--move` retracts the private record and removes the private file, so
the memory lives on only in the mirror; without it the private copy wins
and the mirror skips that slug. `unshare` retracts rather than deletes,
because a deleted record id can never be reused.

## What is searchable

| content | indexed as | found by |
|---|---|---|
| bodies of docs, tasks, plans, sessions, collabs, projects, machines, Linear issues, the status page | block windows (`editor_blocks`), FTS + vector | search, recall (status page excluded from recall) |
| object names and descriptions | `prop` name / description | search |
| memories (title + body), text config records, feedback notes | records, FTS + vector | search, recall |
| session summary, Linear key and title | indexed properties | search |
| other text and choice properties | FTS only | `search --json` |
| relation and member properties, ids, cwd, session id | not indexed; the link index answers backlinks | — |

Recall injects only memories, config files, feedback notes and page
bodies above the threshold, from the private space and the collab spaces.

## Telemetry, feedback, eval

Every bridge operation bumps a counter in a local append-only day log;
the hooks flush one `stats` record per (machine, day). `any-agents
feedback "<text>" --kind bug|idea|memory-wrong|memory-helpful [--ref
<id>] [--space S]` files a searchable note: in the private space about
memories and the bridge, in a collab space for every member's agent to
recall (a wrong shared doc, a convention the team agreed on). `any-agents eval` replays a golden set
(`~/.any-agents/eval/golden.json` or `--golden`; `eval/golden.example.json`
shows the shape) against the live index and reports hit@1 / hit@3 / MRR
and the simulated auto-injection rate.

## Tests

```
go test ./...                       # unit tests
ANY_AGENTS_E2E=1 go test ./e2e -v   # drives the binary against a live server in throwaway spaces
```

Run the e2e against a test account on a local network, never against
the account that holds your data.

## License

MIT, see [LICENSE](LICENSE).
