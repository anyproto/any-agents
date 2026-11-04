package main

// The space schema: nine client bundles (ids are permanent per space; a
// shape change takes a new /vN id). Two declare a type — the app root,
// which hosts the records datasets, and Collab, which needs a chat next
// to its body; seven declare a collection: their objects are built-in
// `page`s filed under them, so a human creating a row in any client gets
// the right thing. Descriptors follow any's docs/27-descriptors.md; every
// property carries xFormat.pos so columns render in declaration order.

const (
	version      = "0.4.2"
	setupVersion = 4 // bump to force a full re-ensure on every machine

	bundleApp     = "agents/v1"
	bundleProject = "agents-project/v1"
	bundleMachine = "agents-machine/v1"
	bundleSession = "agents-session/v1"
	bundleDoc     = "agents-doc/v1"
	bundleTask    = "agents-task/v1"
	bundlePlan    = "agents-plan/v1"
	bundleLinear  = "agents-linear/v1"
	bundleCollab  = "agents-collab/v1"
	bundleRelease = "agents-release/v1"

	// pageType is any's built-in document type every collection member has.
	pageType = "page"

	// Records datasets on the app root (part key = dataset key).
	dsMemories = "memories"
	dsConfig   = "config"
	dsStats    = "stats"
	dsFeedback = "feedback"
)

// bundleNames are the state keys, in install order (targets before the
// definitions that reference them by xKey).
var bundleNames = []string{"app", "project", "machine", "session", "linear", "release", "doc", "task", "plan", "collab"}

var bundleIds = map[string]string{
	"app": bundleApp, "project": bundleProject, "machine": bundleMachine, "session": bundleSession,
	"doc": bundleDoc, "task": bundleTask, "plan": bundlePlan, "linear": bundleLinear, "collab": bundleCollab, "release": bundleRelease,
}

// collectionBundles declare a collection instead of a type: their
// objects are pages filed under them.
var collectionBundles = map[string]bool{"release": true,
	"project": true, "machine": true, "session": true, "linear": true, "doc": true, "task": true, "plan": true,
}

// optionalBundles may be absent in a space: collab needs a server that
// lets a client type declare the chat module.
var optionalBundles = map[string]bool{"collab": true}

var defaultSpaceName = "Agents"

func spaceName() string {
	if v := getenv("ANY_AGENTS_SPACE"); v != "" {
		return v
	}
	return defaultSpaceName
}

// --- descriptor helpers ---

type opt struct{ key, name, color string }

func prop(xkey, name, kind, slug, pos string) PropertyDraft {
	return PropertyDraft{XKey: xkey, Name: name, Kind: kind, XFormat: map[string]any{"type": slug, "pos": pos}}
}

func text(xkey, name, pos string) PropertyDraft { return prop(xkey, name, "string", "text", pos) }
func longtext(xkey, name, pos string) PropertyDraft {
	return prop(xkey, name, "string", "longtext", pos)
}
func urlProp(xkey, name, pos string) PropertyDraft { return prop(xkey, name, "string", "url", pos) }
func datetime(xkey, name, pos string) PropertyDraft {
	return prop(xkey, name, "datetime", "datetime", pos)
}
func date(xkey, name, pos string) PropertyDraft { return prop(xkey, name, "datetime", "date", pos) }
func checkbox(xkey, name, pos string) PropertyDraft {
	return prop(xkey, name, "boolean", "checkbox", pos)
}
func number(xkey, name, pos string) PropertyDraft { return prop(xkey, name, "number", "number", pos) }

func choice(xkey, name, pos string, opts ...opt) PropertyDraft {
	options := map[string]any{}
	for i, o := range opts {
		options[o.key] = map[string]any{"name": o.name, "color": o.color, "pos": lexPos(i)}
	}
	return PropertyDraft{XKey: xkey, Name: name, Kind: "array", XFormat: map[string]any{
		"type": "choice", "pos": pos, "config": map[string]any{"multiple": false}, "options": options}}
}

func relation(xkey, name, pos string, multiple bool, targets ...string) PropertyDraft {
	xf := map[string]any{"type": "relation", "pos": pos, "config": map[string]any{"multiple": multiple}}
	if len(targets) > 0 {
		xf["relation"] = map[string]any{"targetTypes": targets}
	}
	// Relation values are object ids: kept out of the text index (the
	// link index still reports the edges).
	return PropertyDraft{XKey: xkey, Name: name, Kind: "array", XFormat: xf, Meta: map[string]any{"index": "none"}}
}

// member points at space members by identity (`any://u/<identity>`).
func member(xkey, name, pos string, multiple bool) PropertyDraft {
	return PropertyDraft{XKey: xkey, Name: name, Kind: "array", XFormat: map[string]any{
		"type": "member", "pos": pos, "config": map[string]any{"multiple": multiple}},
		Meta: map[string]any{"index": "none"}}
}

// indexed puts a property's text into the main search scope (the
// default scope for properties is FTS-only and never embedded).
func indexed(p PropertyDraft) PropertyDraft {
	p.Meta = map[string]any{"index": "basic"}
	return p
}

// hideEmpty asks any-ui to show the property row only when it has a value
// (its vendor key; other clients ignore it).
func hideEmpty(p PropertyDraft) PropertyDraft {
	p.XFormat["anyui"] = map[string]any{"headerVisibility": "hideWhenEmpty"}
	return p
}

// unindexed keeps a property out of the text index (ids, machine noise).
func unindexed(p PropertyDraft) PropertyDraft {
	p.Meta = map[string]any{"index": "none"}
	return p
}

// lexPos yields short byte-ordered position keys: a0 … a9, b0 … .
func lexPos(i int) string {
	return string(rune('a'+i/10)) + string(rune('0'+i%10))
}

// bodyPart is the editor module's canonical dataset: the same
// editor_blocks body a `page` has, so retyping keeps it.
func bodyPart() map[string]any {
	return map[string]any{"key": "body", "name": "Body", "pos": "a0",
		"ui":       map[string]any{"type": "document"},
		"datasets": []any{map[string]any{"module": "editor"}}}
}

// chatPart is one transcript per object (the chat module admits only
// its canonical dataset).
func chatPart() map[string]any {
	return map[string]any{"key": "chat", "name": "Chat", "pos": "a1",
		"ui":       map[string]any{"type": "chat"},
		"datasets": []any{map[string]any{"module": "chat"}}}
}

func field(key, kind string, extra map[string]any) map[string]any {
	f := map[string]any{"key": key, "kind": kind, "mutableBy": "author"}
	for k, v := range extra {
		f[k] = v
	}
	return f
}

func stamp(key, kind string) map[string]any {
	f := map[string]any{"key": key, "stamp": kind}
	if kind != "creator" {
		f["xFormat"] = map[string]any{"type": "datetime"}
	}
	return f
}

func recordsPart(key, name, pos string, ds map[string]any) map[string]any {
	ds["key"] = key
	ds["idRule"] = "user"
	ds["deleteBy"] = "author"
	return map[string]any{"key": key, "name": name, "pos": pos,
		"ui": map[string]any{"type": "table"}, "datasets": []any{ds}}
}

// --- the declarations ---

var activeOptions = []opt{{"active", "Active", "green"}, {"archived", "Archived", "grey"}}

var toolOptions = []opt{{"claude", "Claude Code", "orange"}, {"codex", "Codex", "green"}, {"gemini", "Gemini CLI", "blue"}, {"other", "Other", "grey"}}

func bundleDrafts() map[string]*BundleEnsure {
	stamps := func() []any {
		return []any{stamp("author", "creator"), stamp("createdAt", "createTime"), stamp("updatedAt", "modifyTime")}
	}
	app := &BundleEnsure{
		Id: bundleApp, Name: "Agents", XKey: "agents", Hidden: true,
		RootCollections: []string{"miniapp"},
		RootProperties:  map[string]any{"miniapp": map[string]any{"bundle": bundleApp, "pos": "y0"}},
		Parts: []map[string]any{
			bodyPart(),
			recordsPart(dsMemories, "Memories", "a1", map[string]any{
				"displayName": "Agent memories",
				"search":      map[string]any{"title": "title", "text": "body"},
				"fields": append([]any{
					field("title", "string", map[string]any{"required": true}),
					field("kind", "string", map[string]any{"required": true}),
					field("scope", "string", map[string]any{"required": true}),
					field("slug", "string", map[string]any{"required": true}),
					field("body", "string", nil),
					field("links", "array", nil),
					field("origin", "string", nil),
				}, stamps()...),
			}),
			recordsPart(dsConfig, "Config files", "a2", map[string]any{
				"displayName": "Agent config files",
				"search":      map[string]any{"text": "body"},
				"fields": append([]any{
					field("path", "string", map[string]any{"required": true}),
					field("tool", "string", nil),
					field("kind", "string", nil),
					field("sha256", "string", nil),
					field("fileId", "string", nil),
					field("body", "string", nil),
				}, stamps()...),
			}),
			recordsPart(dsStats, "Usage", "a3", map[string]any{
				"displayName": "Bridge usage per machine and day", "skipHistory": true,
				"fields": append([]any{
					field("machine", "string", map[string]any{"required": true}),
					field("day", "string", map[string]any{"required": true}),
					field("sessions", "number", nil), field("pulls", "number", nil), field("pushes", "number", nil),
					field("memSynced", "number", nil), field("conflicts", "number", nil), field("memRejected", "number", nil),
					field("ragPrompts", "number", nil), field("ragInjected", "number", nil),
					field("searches", "number", nil), field("errors", "number", nil),
					field("lastErrors", "array", nil),
				}, stamps()...),
			}),
			recordsPart(dsFeedback, "Feedback", "a4", map[string]any{
				"displayName": "Agent feedback on memories and the bridge",
				"search":      map[string]any{"title": "kind", "text": "text"},
				"fields": append([]any{
					field("kind", "string", map[string]any{"required": true}),
					field("text", "string", map[string]any{"required": true}),
					field("machine", "string", nil),
					field("ref", "string", nil),
					field("session", "string", nil),
				}, stamps()...),
			}),
		},
	}
	project := &BundleEnsure{
		Id: bundleProject, Name: "Project", XKey: "agent_project", Collection: true,
		Properties: []PropertyDraft{
			unindexed(text("key", "Scope", "a0")),
			urlProp("repo", "Repository", "a1"),
			choice("status", "Status", "a2", activeOptions...),
			number("memories", "Memories", "a3"),
			datetime("lastActivity", "Last activity", "a4"),
		},
	}
	machine := &BundleEnsure{
		Id: bundleMachine, Name: "Machine", XKey: "agent_machine", Collection: true,
		Properties: []PropertyDraft{
			text("hostname", "Hostname", "a0"),
			text("os", "OS", "a1"),
			text("user", "User", "a2"),
			text("bridgeVersion", "any-agents version", "a3"),
			text("tools", "Tools", "a4"),
			datetime("lastSeen", "Last seen", "a5"),
			choice("status", "Status", "a6", opt{"online", "Online", "green"}, opt{"idle", "Idle", "grey"}),
		},
	}
	session := &BundleEnsure{
		Id: bundleSession, Name: "Session", XKey: "agent_session", Collection: true,
		Properties: []PropertyDraft{
			choice("status", "Status", "a0", opt{"active", "Active", "green"}, opt{"ended", "Ended", "grey"}),
			choice("tool", "Tool", "a1", toolOptions...),
			relation("project", "Project", "a2", false, "agent_project"),
			relation("machine", "Machine", "a3", false, "agent_machine"),
			member("owner", "Owner", "a4", false),
			datetime("startedAt", "Started", "a5"),
			datetime("endedAt", "Ended", "a6"),
			indexed(longtext("summary", "Summary", "a7")),
			relation("linear", "Linear issues", "a8", true, "agent_linear_issue"),
			hideEmpty(urlProp("transcript", "Transcript", "a9")),
			unindexed(hideEmpty(text("host", "Host", "b0"))),
			unindexed(hideEmpty(text("toolVersion", "Tool version", "b1"))),
			unindexed(text("sessionId", "Session id", "b2")),
			unindexed(hideEmpty(text("cwd", "Working directory", "b3"))),
			unindexed(hideEmpty(urlProp("sharedTo", "Shared to", "b4"))),
			unindexed(hideEmpty(urlProp("sharedFrom", "Shared from", "b5"))),
		},
	}
	doc := &BundleEnsure{
		Id: bundleDoc, Name: "Doc", XKey: "agent_doc", Collection: true,
		Properties: []PropertyDraft{
			choice("kind", "Kind", "a0",
				opt{"note", "Note", "grey"}, opt{"report", "Report", "blue"}, opt{"spec", "Spec", "purple"},
				opt{"retro", "Retro", "orange"}, opt{"prompt", "Prompt", "yellow"}, opt{"plan", "Plan", "teal"},
				opt{"other", "Other", "grey"}),
			relation("project", "Project", "a1", false, "agent_project"),
			relation("originSession", "Origin session", "a2", false, "agent_session"),
			relation("sessions", "Sessions", "a3", true, "agent_session"),
			relation("related", "Related", "a4", true),
			relation("linear", "Linear", "a5", true, "agent_linear_issue"),
			member("author", "Author", "a6", false),
			datetime("date", "Written", "a7"),
			checkbox("archived", "Archived", "a8"),
			hideEmpty(urlProp("pr", "PR", "a9")),
		},
	}
	task := &BundleEnsure{
		Id: bundleTask, Name: "Task", XKey: "agent_task", Collection: true,
		Properties: []PropertyDraft{
			choice("status", "Task status", "a0",
				opt{"todo", "To do", "yellow"}, opt{"in_progress", "In progress", "blue"},
				opt{"done", "Done", "green"}, opt{"cancelled", "Cancelled", "grey"}),
			choice("priority", "Priority", "a1",
				opt{"low", "Low", "grey"}, opt{"normal", "Normal", "blue"}, opt{"high", "High", "red"}),
			member("assignee", "Assignee", "a2", false),
			indexed(text("linearKey", "Linear issue", "a3")),
			relation("target", "Target machine", "a4", false, "agent_machine"),
			relation("handledBy", "Handled by", "a5", false, "agent_machine"),
			relation("handledIn", "Handled in", "a6", false, "agent_session"),
			hideEmpty(date("due", "Due", "a7")),
		},
	}
	plan := &BundleEnsure{
		Id: bundlePlan, Name: "Plan", XKey: "agent_plan", Collection: true,
		Properties: []PropertyDraft{
			choice("status", "Plan status", "a0",
				opt{"draft", "Draft", "yellow"}, opt{"approved", "Approved", "blue"}, opt{"done", "Done", "green"}),
			hideEmpty(datetime("approvedAt", "Approved at", "a1")),
			relation("executedBy", "Executed by", "a2", false, "agent_machine"),
			relation("executedIn", "Executed in", "a3", true, "agent_session"),
		},
	}
	linear := &BundleEnsure{
		Id: bundleLinear, Name: "Linear issue", XKey: "agent_linear_issue", Collection: true,
		Properties: []PropertyDraft{
			indexed(text("key", "Key", "a0")),
			choice("status", "Status", "a1",
				opt{"backlog", "Backlog", "grey"}, opt{"todo", "Todo", "yellow"}, opt{"in_progress", "In progress", "blue"},
				opt{"in_review", "In review", "purple"}, opt{"done", "Done", "green"}, opt{"canceled", "Canceled", "grey"}),
			choice("priority", "Priority", "a2",
				opt{"none", "None", "grey"}, opt{"urgent", "Urgent", "red"}, opt{"high", "High", "orange"},
				opt{"medium", "Medium", "yellow"}, opt{"low", "Low", "grey"}),
			text("team", "Team", "a3"),
			text("assignee", "Assignee", "a4"),
			urlProp("url", "URL", "a5"),
			hideEmpty(date("dueDate", "Due", "a6")),
			unindexed(hideEmpty(text("branch", "Branch", "a7"))),
			datetime("syncedAt", "Synced", "a8"),
		},
	}
	collab := &BundleEnsure{
		Id: bundleCollab, Name: "Collab", XKey: "agent_collab", Layout: map[string]any{"type": "page"},
		Parts: []map[string]any{bodyPart(), chatPart()},
		Properties: []PropertyDraft{
			choice("status", "Status", "a0",
				opt{"request", "Request", "yellow"}, opt{"in_progress", "In progress", "blue"},
				opt{"done", "Done", "green"}, opt{"canceled", "Canceled", "grey"}),
			member("requester", "Requester", "a1", false),
			member("assignees", "Assignees", "a2", true),
			relation("project", "Project", "a3", false, "agent_project"),
			relation("linear", "Linear", "a4", true, "agent_linear_issue"),
			relation("related", "Related", "a5", true),
			datetime("date", "Opened", "a6"),
			hideEmpty(datetime("resolvedAt", "Resolved", "a7")),
			unindexed(hideEmpty(urlProp("originSession", "Origin session", "a8"))),
			hideEmpty(urlProp("pr", "PR", "a9")),
			hideEmpty(longtext("doneWhen", "Done when", "a10")),
		},
	}
	release := &BundleEnsure{
		Id: bundleRelease, Name: "Release", XKey: "agent_release", Collection: true,
		Properties: []PropertyDraft{
			relation("project", "Project", "a0", false, "agent_project"),
			text("version", "Version", "a1"),
			date("released", "Released", "a2"),
			urlProp("url", "Release URL", "a3"),
			checkbox("breaking", "Breaking", "a4"),
			checkbox("apiChanges", "API changes", "a5"),
			relation("includes", "Includes", "a6", true),
			member("author", "Author", "a7", false),
		},
	}
	return map[string]*BundleEnsure{"app": app, "project": project, "machine": machine, "session": session,
		"doc": doc, "task": task, "plan": plan, "linear": linear, "collab": collab, "release": release}
}

// iconOf renders the any-ui icon:v2 value for a lucide glyph.
func iconOf(glyph, color string) string {
	return `icon:v2:{"assignment":{"kind":"pack","ref":{"packId":"io.anyproto.lucide","glyphId":"` + glyph +
		`"}},"color":"` + color + `"}`
}

var typeIcons = map[string][2]string{
	"app": {"bot", "purple"}, "project": {"folder-git-2", "blue"}, "machine": {"monitor", "teal"},
	"session": {"terminal", "grey"}, "doc": {"file-text", "blue"}, "task": {"list-checks", "yellow"},
	"plan": {"map", "teal"}, "linear": {"circle-dot", "purple"}, "collab": {"messages-square", "orange"}, "release": {"package", "green"},
}
