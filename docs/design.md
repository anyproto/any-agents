# any-agents — design

Shared context for Claude Code and Codex through a common tool adapter, stored in any spaces. One private space per
account holds what only the owner's agents need; any number of collab
spaces hold what a team shares. Humans read and write the same objects
in any-ui and the native clients, from a phone included.

## Spaces

| space | kind | holds | who |
|---|---|---|---|
| "Agents" (one per account) | private | memories, config files, usage stats, feedback, sessions, machines, private docs, tasks, plans, project pages, the status page | the owner's agents on every machine |
| any shared space the account is a member of | collab | collabs, shared docs, tasks, plans, project pages (converged per repo), sessions shared on purpose, Linear issue pages | every member's agents and the members themselves |

Rules

- Config, stats and machines never leave the private space. Memories are
  private unless published with `memory share`; sessions are private
  unless copied with `session share`.
- Every space gets the same bundle set, so ids resolve the same way
  everywhere; what differs is which objects a command creates there.
- Relations never cross spaces. A reference into another space is a
  global link (`any://o/<space>/<id>`) in a body or in a url property.
- The private space is found by name, and only adopted when this account
  owns it and it is empty or already ours; otherwise `ANY_AGENTS_SPACE`
  names another.
- `space add <name|id>` registers a space the account already joined
  (writer installs, reader adopts); `space create <name>` makes a new
  one; invites go through `any invite create` / `any join`. 1-1 spaces
  work once both sides are online.
- Every command takes `--space`; a link names its own space, so
  `show <link>` needs no flag.

## Types

A type exists only where an object needs more than a body and
properties.

| type | xKey | parts | role |
|---|---|---|---|
| app root `agents/v1` | `agents` (hidden) | body (status page) + records datasets `memories`, `config`, `stats`, `feedback` | the app's sidebar entry; host of the deterministic children (projects, machines, Linear issues) |
| Collab `agents-collab/v1` | `agent_collab` | body (editor) + chat (chat module) | a help request with its thread |
| `page` (any's built-in) | — | body | every other object |

Collab properties: `status` request · in_progress · done · canceled,
`requester` member, `assignees` member (multiple), `project`, `linear`,
`related`, `date`, `resolvedAt`, `originSession` url, `pr` url,
`doneWhen` longtext: the definition of done, stated at `open`; `agreed`
member (multiple): who agreed to close. A close is a vote: `collab
done|cancel --met` posts the evidence to the chat and records the
caller's agreement, and the status changes once the requester and every
assignee agreed; `--user "<their words>"` closes at once on a human's
decision. `collab say` by a non-participant joins first, so everyone who
talked has a say; `reopen` clears `agreed`. Two agents agreeing in the
same moment can overwrite each other's entry (last writer wins): the
chat keeps both messages and the next `collab done` re-records the lost
one. A helper's agent investigates and verifies on its own; features,
architecture and binding decisions are escalated to its user.

Rule: the chat part needs an any server that lets a client type declare
the chat module (branch `cheggaaa/chat-part-user-types`). On an older
server the Collab bundle is skipped and `collab` commands say so;
everything else works.

## Collections

Objects are `page`s filed under collections. A collection is columns
only; its `meta.defaultType` is `page`, so a human creating a row in a
client gets a page with a body.

| collection | xKey | properties | object created by |
|---|---|---|---|
| Project | `agent_project` | key, repo, status, memories, lastActivity | deterministic child `project:<scope>` of the app root — one page per repo, converged across machines and members |
| Machine | `agent_machine` | hostname, os, user, bridgeVersion, tools, lastSeen, status | child `machine:<host>`, private space only |
| Session | `agent_session` | status, tool, project, machine, owner (member), startedAt, endedAt, summary, linear, transcript, host, toolVersion, sessionId, cwd, sharedTo, sharedFrom | the session-start hook; `session share` in a collab space |
| Doc | `agent_doc` | kind, project, originSession, sessions, related, linear, author (member), date, archived, pr | `doc add`, or a human writing a page |
| Release | `agent_release` | project, version, released, url, breaking, apiChanges, includes (releases), author (member) | `release add`, or a human filing a page under Release |
| Task | `agent_task` | status, priority, assignee (member), linearKey, target, handledBy, handledIn, due | `task add`, or a human filing a Doc under Task (a phone works) |
| Plan | `agent_plan` | status, approvedAt, executedBy, executedIn | `plan add <taskId>` |
| Linear issue | `agent_linear_issue` | key, status, priority, team, assignee, url, dueDate, branch, syncedAt | child `linear:<KEY>`; `--linear` on any doc/task/plan/collab; `linear sync` through linctl |

Rules

- A task is a Doc filed under Task; a plan is a Doc filed under Plan.
  One object, one body, one history.
- Reverse directions (project → sessions, task → plan) are backlinks,
  never stored.
- `assignee` is a space member (`any://u/<identity>`). Pending-task
  selection is "untargeted or this machine" and "unassigned or me";
  starting an unassigned task assigns it to me. Shared tasks are the
  seed of a team tracker; for now Linear stays the tracker and a task
  links to its issue.
- Machine-bound properties (target, handledBy, handledIn, executedBy)
  are set only in the private space; in a collab space the member
  properties say who.

## Pipelines

Session (private space). session-start: setup → machine online → pull
memories for the repo and the tool's config → Session page (tool,
host, cwd, project) → context note: project, session link, open task
count, the CLI, then the collab digest (open requests nobody took, new
messages on my collabs). post-tool: push memory and config files as
they change. session-end: session ended → transcript attached → push →
status page → stats flushed. Hooks never fail a session.

Recall. Every prompt runs a hybrid search over the private space and
every collab space; memories, config, feedback notes and document
bodies above the threshold are injected once per session each, three
at most, collab hits labelled with their space. Another project's
memories travel only as workflow rules (kinds user and feedback); one
document-body excerpt per prompt. `search --space` runs one query in
one space.

Feedback. `feedback "<text>" --kind K [--ref id] [--space S]` files a
searchable note: private for memories and the bridge, in a collab
space for the whole team (a wrong shared doc, a convention). Recall
surfaces it next to what it refers to.

Docs, tasks, plans. `doc add`, `task add`, `plan add` file pages with
their properties in the chosen space; a human files the same from any
client. Long reports are docs; sharing one is `doc add --space`.

Collab.

1. Requester: `collab open <request.md> --space Team [--to alice]
   [--announce]` — a Collab with the request as body, status
   `request`, requester me, project page of the cwd repo, link to my
   private session. `--announce` posts the title and link into the
   space's general chat so people can ask their agents to join.
2. Helper's agent: sees the open request at session start, reads it
   (`collab show`), takes it (`collab join`: adds itself to assignees,
   status `in_progress`), then talks (`collab say`, `collab wait`).
   Replies on collabs addressed to the agent are autonomous.
3. Requester's agent: `collab wait` blocks up to 90 s for a message from
   someone else (exit 2 means nothing yet: run it again), answers with
   `collab say`, agrees to close with `collab done` or `collab cancel`;
   the collab closes once every participant agreed, or at once when a
   user decides (`--user`).

Rules for the chat: messages are sent with `agent {name: "<Tool>
(<member>)", done: true}`, so the owner's own devices badge them. The
bridge keeps its own per-collab cursor on `_ver.id` and never touches
the account's read state, so the humans' unread badges stay intact.
Chat text stays short; detail goes into the body or a linked doc.

Sharing a session. `session share --space Team [--transcript]` copies
the private Session page (name, body, summary, tool, host, project
page re-resolved in the target space) into the collab space, stamps
`sharedTo` on the original and `sharedFrom` on the copy. Objects cannot
move between spaces, so the private one stays.

Linear. `--linear ABC-12` on a doc, task, plan, collab or session
resolves the key to a deterministic issue page in that space; a
session's issues are recorded on the private Session. `linear sync`
refreshes through linctl when it is installed.

Status page. The app root's body in each space: open collabs, open
tasks, sessions, recent docs; in the private space also memory totals
per project and the machines.

Migration from any-claude. Same account and server: any-claude has
`migrate --to <space>` that copies objects, bodies, records and files
from its space into the any-agents private space, idempotent through
an id map. The old space stays as the archive.

## Naming and cleanup

Binary `any-agents`, state in `~/.any-agents/` (`ANY_AGENTS_HOME`),
env `ANY_AGENTS_ADDR`, `ANY_AGENTS_SPACE`, `ANY_AGENTS_RAG_MIN`,
`ANY_AGENTS_TOOL`. The tool adapter owns the harness specifics: home
dir (`CLAUDE_CONFIG_DIR` or `CODEX_HOME`), config roots, memory dir,
session env var, hook install and parsing. Config records are keyed
`<tool>:<path>`. No secrets, no home-path fallbacks, no team-specific
examples, no private eval set in the repo.

## Review notes (2026-10-27)

Gaps found on review, with the chosen answer.

- Feedback was private only. Now `--space` files it in a collab space.
- Recall searched the private space only. Now every collab space too,
  three hits in total, labelled by space.
- The private space was adopted by name alone; a shared or foreign
  space named "Agents" would have been written into. Now owner-only,
  and only when empty or already ours.
- `collab wait` defaulted to 10 minutes, longer than a harness command
  timeout. Now 90 s with exit 2, so the agent loops.
- In a collab space a task records who (member) but not which session
  handled it, since relations cannot cross spaces; `session share` is
  the way to show the session. Left as is.
- Member names arrive a few seconds after a join; `--to <name>` fails
  until then and the agent name falls back to the short identity. Server
  side, left as is.
- Team memories: `memory share <slug> --space S` publishes a private
  memory into the collab space's `memories` records (same
  `<scope>:<slug>` ids); every member's session start mirrors the
  project's shared memories into `memory/shared/<space>/`, lists them
  in MEMORY.md and recall covers them. The publisher's own entries are
  writable files synced with the private three-way rule and pushed by
  `push` and the post-tool hook (a new file written into the mirror dir
  is a new shared memory); other members' entries are read-only and the
  space always wins. `share --move` retracts the private record and
  removes the private file, so the collab space becomes the memory's
  home; without `--move` the private copy wins and the mirror skips
  the slug. The record belongs to
  its publisher (the dataset's author rule); `memory unshare` retracts
  one's own (kind `retracted`, title and body cleared; readers and
  recall skip it, the mirror file goes) because a
  deleted record id can never be reused, while a retracted slug can be
  shared again. Verified on two accounts: mirror, MEMORY.md section,
  recall labelled with the space, refusal of another member's overwrite,
  retract and re-share.

## Whole-tree review (2026-10-27), what changed

Data safety
- Push sends only files changed on this machine since the last sync, or
  never synced. A tracked, unchanged file is never pushed, so it cannot
  overwrite a newer edit from another machine (memories and config).
- Nothing deletes a record any more: a deleted record id can never be
  reused on any's record datasets. A removed memory or config file
  retracts its record (kind `retracted`, empty content); readers treat
  a retracted record as absent, and a file that reappears revives it.
- A shared record's slug is another member's free text: the mirror
  file name is folded and kept inside the space's dir, never a path.
- Config sync skips a symlinked directory under the tool's config roots
  instead of aborting the whole scan on it.
- A locally changed config file that the space overwrites is kept as
  `.conflict` next to it, as the memory rules do.
- An install into a space this machine did not just create waits
  (bounded) for the bundle registry to sync, so two machines or members
  cannot each mint a root.

Semantics
- A doc re-saved by title updates only a doc of mine (or one without an
  author); another member's doc of the same name stays.
- Collab project pages are seeded from the private project's key, not
  its display name, so a renamed page still converges with the team's.
- Relations never cross spaces: `--related`, `doc link` and `task link`
  refuse a link into another space.
- Chat cursor: a message by this account's human (no agent envelope) is
  new to the agent; a repeated `join` keeps the cursor; `wait` on a
  machine without a cursor starts from now.
- `--space <name>` resolves the private space first, then one collab
  space; two collab spaces with one name need the id.
- `gc` deletes only unreferenced transcripts, never a human's attachment.
- A collab space's status page, shared memories and feedback carry no
  hostname, machine or private session id.
- `install` re-checks every space, so a server upgrade that enables the
  chat module turns Collab on without a new state file; a collab space
  whose setup failed is not registered.
- The prompt hook searches all spaces concurrently and every hook's
  HTTP client times out inside the harness's hook timeout.

Left as is
- Scopes are the origin repo's basename: two unrelated repos named the
  same share memories and project pages.
- A pulled memory is written to `<slug>.md` without checking that an
  unparseable file of that name exists.
- Concurrent `collab join` from two helpers can drop one assignee (a
  whole-array set); both see it and join again.
- Sharing a session to a second space, then the first again, makes a
  second copy there; only the last copy follows the original.
- Two machines with one hostname share a machine page and a stats row.

## Mixed server versions (verified 2026-11-02)

A member whose any server predates PR anyproto/any#310 (chat module
reserved to the server) in a space where another member installed the
Collab bundle:

- The type declaration with its chat part, the Collab objects and their
  chat messages all apply on inbound sync. Nothing is parked or
  rejected; the SDK checks the reservation only on local writes.
- The old peer can set properties on a collab and send messages into
  it; both reach the other members. It cannot create a collab (`400
  type.reserved_carrier`) and its `POST /bundles` refuses even to adopt
  the registered Collab bundle (`400 dataset.module_reserved`, checked
  before the adopt path). any-agents then adopts through a plain read
  of the registry, so `list`, `show`, `join`, `say` and `wait` work;
  `open` explains that the server must be updated.
- After the server is upgraded on the same data dir everything works
  with no repair step.
