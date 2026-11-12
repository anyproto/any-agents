// End-to-end suite: drives the built any-agents binary against a live any
// server in throwaway spaces, then verifies the server-side effects with
// direct HTTP calls.
//
// Run with:
//
//	ANY_AGENTS_E2E=1 go test ./e2e -v -count=1 -timeout 20m
//
// ANY_AGENTS_ADDR picks the server (default 127.0.0.1:2046).
// ANY_AGENTS_ADDR_B names a second server on the same network, logged into
// another account: it joins the collab space as a member for the shared
// memory checks, which are skipped when it is unset.
//
// Isolation: every run builds the binary into a temp dir and points HOME,
// ANY_AGENTS_HOME (bridge state, sync fingerprints, stats, the installed
// binary) and CLAUDE_CONFIG_DIR (settings.json, config files, memory dirs)
// at a sandbox. The private space and the collab space carry a name unique
// to the run (the member's private space too); cleanup deletes exactly
// those on each server, also when a test fails, panics, times out or the
// run is interrupted.
package e2e

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	// scope is what resolveScope() derives from the sandbox project dir:
	// the basename of its git origin URL.
	scope = "agents-e2e-proj"
	// phrase is unique to one doc body; the search and auto-recall tests
	// rely on it outranking everything else in the space.
	phrase = "zylophonic quarklebeam resonates at midnight"

	sidA = "e2e00001-1111-4111-8111-aaaaaaaaaaaa" // session used by the CLI commands
	sidB = "e2e00002-2222-4222-8222-bbbbbbbbbbbb" // session used for the pending-task count
	sidP = "e2e00003-3333-4333-8333-cccccccccccc" // session used for the prompt hook

	// fakeIdentity is a well-formed (base58) identity nobody in the space
	// holds: a task assigned to it belongs to someone else.
	fakeIdentity = "AE2eSomeoneAnother111111111111111111111111111111"
)

var (
	binPath   string
	addr      string
	spaceName string // the private space
	teamName  string // the collab space
	spaceId   string
	teamId    string
	identity  string
	homeA     string // primary sandbox HOME
	homeB     string // second machine, used by the pull test
	addrB     string // the second member's server ("" = no member checks)
	homeM     string // the second member's sandbox HOME
	projDir   string
	tmpRoot   string
	hostName  string
	keep      bool

	// ids produced by earlier subtests and read by later ones
	sessionObjA string
	projectObj  string
	docObj      string
	taskObj     string
	task2Obj    string

	memberIdentity string
	memberReady    bool // the member joined the collab space and set it up
)

func TestMain(m *testing.M) { os.Exit(mainRun(m)) }

func mainRun(m *testing.M) (code int) {
	if os.Getenv("ANY_AGENTS_E2E") != "1" {
		return m.Run() // TestE2E skips itself
	}
	flag.Parse()
	addr = os.Getenv("ANY_AGENTS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:2046"
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(os.Stderr, "e2e setup: "+format+"\n", a...)
		return 1
	}
	if err := health(); err != nil {
		return fail("any server not reachable at %s (%v) — start it with `any run`", addr, err)
	}
	if addrB = os.Getenv("ANY_AGENTS_ADDR_B"); addrB != "" {
		if addrB == addr {
			return fail("ANY_AGENTS_ADDR_B must name another server (another account) than ANY_AGENTS_ADDR")
		}
		if err := healthAt(addrB); err != nil {
			return fail("member server not reachable at %s (%v)", addrB, err)
		}
	}
	var err error
	if tmpRoot, err = os.MkdirTemp("", "any-agents-e2e-"); err != nil {
		return fail("temp dir: %v", err)
	}
	keep = os.Getenv("ANY_AGENTS_E2E_KEEP") == "1" // debugging: leave the spaces and sandbox behind
	if !keep {
		defer os.RemoveAll(tmpRoot)
	}
	if hostName, err = os.Hostname(); err != nil {
		return fail("hostname: %v", err)
	}

	spaceName = fmt.Sprintf("agents-e2e-%d", time.Now().Unix())
	teamName = spaceName + "-team"
	defer func() {
		if !cleanupSpaces() {
			code = 1
		}
	}()
	guardCleanup()

	binPath = filepath.Join(tmpRoot, "any-agents")
	if out, err := exec.Command("go", "build", "-o", binPath, "..").CombinedOutput(); err != nil {
		return fail("go build: %v\n%s", err, out)
	}

	homeA, homeB = filepath.Join(tmpRoot, "homeA"), filepath.Join(tmpRoot, "homeB")
	homeM = filepath.Join(tmpRoot, "homeM")
	projDir = filepath.Join(tmpRoot, "proj")
	for _, d := range []string{toolHome(homeA), toolHome(homeB), toolHome(homeM), projDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fail("mkdir %s: %v", d, err)
		}
	}
	for _, args := range [][]string{
		{"init", "-q", projDir},
		{"-C", projDir, "remote", "add", "origin", "https://example.com/" + scope + ".git"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			return fail("git %v: %v\n%s", args, err, out)
		}
	}
	// A settings.json with a foreign key, a foreign hook and a stale
	// any-claude hook: install must keep the first two and drop the third.
	if err := os.WriteFile(filepath.Join(toolHome(homeA), "settings.json"), []byte(preSettings), 0o644); err != nil {
		return fail("settings.json: %v", err)
	}

	out, errOut, installErr := execCLI(runOpts{}, "install", "--create-space")
	if st, err := loadState(homeA); err == nil {
		spaceId, identity = st.Private, st.Identity
	}
	if spaceId != "" {
		trackSpace(spaceId)
		fmt.Fprintf(os.Stderr, "e2e: server %s, space %s (%s), sandbox %s\n", addr, spaceName, spaceId, tmpRoot)
	}
	if installErr != nil {
		return fail("install: %v\n%s\n%s", installErr, out, errOut)
	}
	if spaceId == "" || identity == "" {
		return fail("install left no private space or identity in state.json")
	}
	return m.Run()
}

const preSettings = `{
  "theme": "e2e-keep",
  "hooks": {
    "SessionStart": [
      {"hooks": [{"type": "command", "command": "echo e2e-keep-me"}]},
      {"hooks": [{"type": "command", "command": "/old/bin/any-claude hook session-start"}]}
    ]
  }
}
`

func TestE2E(t *testing.T) {
	if os.Getenv("ANY_AGENTS_E2E") != "1" {
		t.Skip("set ANY_AGENTS_E2E=1 to run against a live any server")
	}
	// A panicking subtest skips TestMain's deferred cleanup but still runs
	// its parents' cleanups.
	t.Cleanup(func() { cleanupSpaces() })
	t.Run("install_is_idempotent_and_builds_the_space", testInstall)
	t.Run("hook_session_start_creates_the_session_page", testSessionStartHook)
	t.Run("memory_files_sync_up_and_down", testMemorySync)
	t.Run("config_sync_leaves_claude_code_synced_skills_alone", testConfigSync)
	t.Run("doc_add_list_append_edit_archive", testDocs)
	t.Run("task_add_start_done_and_pending_count", testTasks)
	t.Run("task_assignee_and_mine", testTaskAssignee)
	t.Run("task_update_and_link_checks", testTaskUpdate)
	t.Run("plan_collection_on_a_task_and_standalone", testPlans)
	t.Run("session_note_summary_end_and_transcript", testSessionLifecycle)
	t.Run("codex_joins_the_existing_install", testCodexInstall)
	t.Run("collab_space_create_share_and_remove", testCollabSpace)
	t.Run("collab_space_second_member_joins", testCollabMember)
	t.Run("shared_memory_move_edit_push_and_mirror", testSharedMemory)
	t.Run("search_finds_the_doc_body", testSearch)
	t.Run("feedback_and_stats", testFeedbackAndStats)
	t.Run("prompt_hook_recalls_once_per_session", testPromptHook)
	t.Run("eval_reads_the_golden_set", testEval)
	t.Run("release_add_list_show_and_update_notice", func(t *testing.T) {
		st := loadStateT(t, homeA)
		ps := st.priv()
		r := ps.owner("release")
		notes := "# any v9.9.9\n\nShipped with the e2e harness.\n\n## New\n\n- the release collection\n"
		out, errOut := runCLI(t, runOpts{sid: sidA, stdin: notes}, "release", "add", "-", "--project", "any", "--version", "v9.9.9",
			"--url", "https://github.com/anyproto/any/releases/tag/v9.9.9", "--released", "2026-01-02", "--breaking")
		mustNotLogErrors(t, errOut, "release add")
		relId := linkId(t, out)
		row := apiObject(t, relId)
		mustEqual(t, typeOf(row), "page", "release object type")
		mustBeFiled(t, row, r, "release")
		mustEqual(t, nameOf(row), "any v9.9.9", "release name")
		mustEqual(t, strOf(row, r, ps.prop("release", "version")), "v9.9.9", "release version")
		mustEqual(t, strOf(row, r, ps.prop("release", "url")), "https://github.com/anyproto/any/releases/tag/v9.9.9", "release url")
		if b, _ := group(row, r)[ps.prop("release", "breaking")].(bool); !b {
			t.Error("release add --breaking did not set the flag")
		}
		if firstLink(row, r, ps.prop("release", "project")) == "" {
			t.Error("release add did not link the project page")
		}
		mustEqualSlice(t, arrOf(row, r, ps.prop("release", "author")), []string{"any://u/" + identity}, "release author")
		mustOnlyDeclaredProps(t, ps, "release", row)
		mustContain(t, apiMarkdown(t, relId), "the release collection", "release notes body")

		out, _ = runCLI(t, runOpts{sid: sidA}, "release", "list")
		mustContain(t, out, "v9.9.9", "release list")
		mustContain(t, out, "breaking", "release list shows the flags")
		out, _ = runCLI(t, runOpts{sid: sidA}, "release", "show", relId)
		mustContain(t, out, "version=v9.9.9", "release show meta")
		mustContain(t, out, "the release collection", "release show body")

		// the same project and version re-saves the page in place
		out, errOut = runCLI(t, runOpts{sid: sidA, stdin: notes + "\nAmended.\n"}, "release", "add", "-", "--project", "any", "--version", "v9.9.9")
		mustContain(t, errOut, "updated existing", "release add twice")
		mustEqual(t, linkId(t, out), relId, "release add twice keeps the page")
		mustContain(t, apiMarkdown(t, relId), "Amended.", "release notes updated")

		// --includes resolves bundled releases by project@version
		out, _ = runCLI(t, runOpts{sid: sidA, stdin: "SDK notes.\n"}, "release", "add", "-", "--project", "any-sync-sdk", "--version", "v9.0.0")
		sdkId := linkId(t, out)
		runCLI(t, runOpts{sid: sidA, stdin: notes}, "release", "add", "-", "--project", "any", "--version", "v9.9.9", "--includes", "any-sync-sdk@v9.0.0")
		row = apiObject(t, relId)
		mustEqualSlice(t, arrOf(row, r, ps.prop("release", "includes")), []string{"any://" + sdkId}, "release includes")
		_, errOut = runCLI(t, runOpts{sid: sidA, stdin: "n\n", wantFail: true}, "release", "add", "-", "--project", "any", "--version", "v9.9.10", "--includes", "any-store@v0.0.0")
		mustContain(t, errOut, "no release any-store v0.0.0", "release add with an unknown include")

		// the current repo's latest release and a newer any-agents reach the
		// session-start context
		runCLI(t, runOpts{sid: sidA, stdin: "Project notes.\n"}, "release", "add", "-", "--version", "v1.2.3")
		runCLI(t, runOpts{sid: sidA, stdin: "Faster.\n"}, "release", "add", "-", "--project", "any-agents", "--version", "v99.0.0", "--url", "https://example.invalid/any-agents/v99.0.0")
		ctx := sessionStartHook(t, sidA, "resume")
		mustContain(t, ctx, "Latest release of "+scope+": v1.2.3", "latest release line")
		mustContain(t, ctx, "any-agents v99.0.0 is out", "update notice")
		mustContain(t, ctx, "https://example.invalid/any-agents/v99.0.0", "update notice links the release")
		mustContain(t, apiMarkdown(t, ps.owner("app")), "## Recent releases", "status page lists releases")
	})
	t.Run("status_page_reflects_the_space", testStatusPage)
}

// Both harnesses use the same account and bridge home. Run the Codex
// commands without ANY_AGENTS_ADDR to exercise desktop hook discovery
// of the installed server as well as the real Any lifecycle.
func testCodexInstall(t *testing.T) {
	const sid = "e2ec0de0-1234-4567-8901-abcdef123456"
	codexHome := filepath.Join(homeA, ".codex")
	o := runOpts{extraEnv: []string{"CODEX_HOME=" + codexHome, "CODEX_THREAD_ID=" + sid, "ANY_AGENTS_ADDR="}}
	ps := loadStateT(t, homeA).priv()
	claudeFile := filepath.Join(toolHome(homeA), "settings.json")
	before := readFile(t, claudeFile)
	out, _ := runCLI(t, o, "install", "--tool", "codex")
	mustContain(t, out, filepath.Join(codexHome, "hooks.json"), "Codex install destination")
	mustEqual(t, readFile(t, claudeFile), before, "Claude settings after installing Codex")
	mustEqual(t, loadStateT(t, homeA).Private, spaceId, "private space after installing Codex")
	mustEqual(t, strOf(apiObject(t, ps.MachineObjectId), ps.owner("machine"), ps.prop("machine", "tools")), "claude codex", "machine tools")

	hook := func(action string, fields map[string]any) string {
		t.Helper()
		fields["session_id"], fields["cwd"] = sid, projDir
		event := o
		event.stdin = mustJSON(fields)
		out, stderr := runCLI(t, event, "hook", "--tool", "codex", action)
		mustNotLogErrors(t, stderr, action)
		return out
	}
	start := hook("session-start", map[string]any{"source": "startup"})
	mustContain(t, start, "SessionStart", "Codex context envelope")
	row := sessionRow(t, ps, sid)
	mustEqual(t, choiceOf(row, ps.owner("session"), ps.prop("session", "tool")), "codex", "Codex session tool")

	file := filepath.Join(codexHome, "AGENTS.md")
	writeFile(t, file, "Codex project conventions.\n")
	hook("post-tool", map[string]any{"tool_name": "apply_patch", "tool_input": map[string]string{"command": "*** Update File: " + file}})
	rows := apiRecords(t, ps.owner("app"), ps.coll("config"), map[string]any{"tool": "codex"})
	if len(rows) != 1 || rows[0]["body"] != "Codex project conventions.\n" {
		t.Fatalf("Codex post-tool records: %v", rows)
	}
	transcript := filepath.Join(tmpRoot, "codex-transcript.jsonl")
	writeFile(t, transcript, "{\"type\":\"session_meta\"}\n")
	hook("session-end", map[string]any{"transcript_path": transcript})
	row = sessionRow(t, ps, sid)
	mustEqual(t, choiceOf(row, ps.owner("session"), ps.prop("session", "status")), "ended", "Codex session end")
	output := filepath.Join(tmpRoot, "restored-codex-transcript.jsonl")
	runCLI(t, o, "session", "transcript", "-o", output)
	mustEqual(t, readFile(t, output), readFile(t, transcript), "Codex transcript")
}

// --- cleanup ---

var cleanup struct {
	sync.Mutex
	ids  map[string]map[string]bool // server → space ids
	done bool
	ok   bool
}

// trackSpace records a space this run created, for cleanupSpaces.
func trackSpace(id string) { trackSpaceAt(addr, id) }

func trackSpaceAt(server, id string) {
	cleanup.Lock()
	defer cleanup.Unlock()
	if cleanup.ids == nil {
		cleanup.ids = map[string]map[string]bool{}
	}
	if cleanup.ids[server] == nil {
		cleanup.ids[server] = map[string]bool{}
	}
	cleanup.ids[server][id] = true
}

// ownSpaceName reports whether a space name carries this run's unique
// prefix: the only spaces cleanup may delete.
func ownSpaceName(name string) bool {
	return spaceName != "" && (name == spaceName || strings.HasPrefix(name, spaceName+"-"))
}

// cleanupSpaces deletes the spaces this run created, once, on every
// server of the run (the member's server last: its delete of the collab
// space only drops its own copy).
func cleanupSpaces() bool {
	cleanup.Lock()
	defer cleanup.Unlock()
	if cleanup.done {
		return cleanup.ok
	}
	cleanup.done, cleanup.ok = true, true
	if spaceName == "" {
		return true
	}
	if keep {
		fmt.Fprintf(os.Stderr, "e2e: ANY_AGENTS_E2E_KEEP=1 — spaces %s* and %s left behind\n", spaceName, tmpRoot)
		return true
	}
	servers := []string{addr}
	if addrB != "" {
		servers = append(servers, addrB)
	}
	for _, server := range servers {
		if !cleanupServer(server) {
			cleanup.ok = false
		}
	}
	return cleanup.ok
}

// cleanupServer deletes one server's share of the run's spaces: the
// tracked ids plus any active space named with the run's prefix (one a
// command created before its id was recorded). A tracked id whose name
// does not carry the prefix is refused, so no other space of the account
// is ever touched.
func cleanupServer(server string) bool {
	ok := true
	ids := map[string]bool{}
	for id := range cleanup.ids[server] {
		info, err := getSpaceAt(server, id)
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr, "e2e cleanup: read space %s on %s: %v\n", id, server, err)
			ok = false
		case info.Status == "deleted":
		case !ownSpaceName(info.Name):
			fmt.Fprintf(os.Stderr, "e2e cleanup: refusing to delete %s (%q is not a space of this run)\n", id, info.Name)
			ok = false
		default:
			ids[id] = true
		}
	}
	spaces, err := listSpacesAt(server)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e cleanup: list spaces on %s: %v\n", server, err)
		ok = false
	}
	for _, s := range spaces {
		if ownSpaceName(s.Name) {
			ids[s.Id] = true
		}
	}
	for id := range ids {
		if err := deleteSpaceAt(server, id); err != nil {
			fmt.Fprintf(os.Stderr, "e2e cleanup: delete space %s on %s: %v\n", id, server, err)
			ok = false
		} else {
			fmt.Fprintf(os.Stderr, "e2e cleanup: deleted space %s on %s\n", id, server)
		}
	}
	return ok
}

// guardCleanup deletes the run's spaces before `go test -timeout` kills
// the binary, and on SIGINT/SIGTERM: neither path reaches a defer.
func guardCleanup() {
	if f := flag.Lookup("test.timeout"); f != nil {
		if d, err := time.ParseDuration(f.Value.String()); err == nil && d > 2*time.Minute {
			time.AfterFunc(d-time.Minute, func() {
				fmt.Fprintln(os.Stderr, "e2e: the test timeout is near — deleting the run's spaces now")
				cleanupSpaces()
			})
		}
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		fmt.Fprintln(os.Stderr, "e2e: interrupted — deleting the run's spaces")
		cleanupSpaces()
		if !keep {
			os.RemoveAll(tmpRoot)
		}
		os.Exit(1)
	}()
}

// --- 1. install ---

func testInstall(t *testing.T) {
	before := loadStateT(t, homeA)
	out, errOut := runCLI(t, runOpts{}, "install")
	mustContain(t, out, "installed", "second install output")
	after := loadStateT(t, homeA)
	if !reflect.DeepEqual(before, after) {
		t.Errorf("install is not idempotent: state.json changed\nbefore: %s\nafter:  %s",
			mustJSON(before), mustJSON(after))
	}
	mustEqual(t, after.Version, 1, "state.json version")
	mustEqual(t, after.Private, spaceId, "state.json private space")
	if after.Identity == "" || after.TechSpaceId == "" {
		t.Errorf("state.json identity %q, techSpaceId %q: want both", after.Identity, after.TechSpaceId)
	}
	ps := after.priv()
	mustEqual(t, ps.Id, spaceId, "private space id")
	mustEqual(t, ps.Name, spaceName, "private space name")
	mustEqual(t, ps.Kind, "private", "private space kind")
	mustEqual(t, ps.SetupVersion, wantSetupVersion(t), "private space setupVersion")
	if ps.collabOn() {
		if strings.Contains(out, "collabs  off") {
			t.Errorf("install says collabs are off although the Collab bundle resolved:\n%s", out)
		}
	} else {
		mustContain(t, out, "collabs  off", "install output without the Collab bundle")
		mustContain(t, errOut, "collabs are off in "+spaceName, "install warning without the Collab bundle")
	}
	t.Logf("collabs on: %v", ps.collabOn())

	checkSpaceSetup(t, ps)

	// the Machine page for this host
	machine := apiObject(t, ps.MachineObjectId)
	m := ps.owner("machine")
	mustEqual(t, nameOf(machine), hostName, "machine page name")
	mustEqual(t, typeOf(machine), "page", "machine object type")
	mustBeFiled(t, machine, m, "machine page")
	mustEqual(t, strOf(machine, m, ps.prop("machine", "hostname")), hostName, "machine hostname property")
	mustEqual(t, strOf(machine, m, ps.prop("machine", "tools")), "claude", "machine tools property")
	mustEqual(t, choiceOf(machine, m, ps.prop("machine", "status")), "online", "machine status property")
	mustOnlyDeclaredProps(t, ps, "machine", machine)

	// hooks: merged into $CLAUDE_CONFIG_DIR/settings.json, pointing at the
	// installed binary under $ANY_AGENTS_HOME
	bin := filepath.Join(bridgeHome(homeA), "bin", "any-agents")
	mustContain(t, out, bin, "install output names the installed binary")
	if fi, err := os.Stat(bin); err != nil || fi.Mode()&0o111 == 0 {
		t.Errorf("installed binary %s missing or not executable (%v)", bin, err)
	}
	var settings map[string]any
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(toolHome(homeA), "settings.json"))), &settings); err != nil {
		t.Fatalf("settings.json: %v", err)
	}
	mustEqual(t, fmt.Sprint(settings["theme"]), "e2e-keep", "settings.json keeps unrelated keys")
	hooks, _ := settings["hooks"].(map[string]any)
	for event, name := range map[string]string{
		"SessionStart": "session-start", "SessionEnd": "session-end",
		"PostToolUse": "post-tool", "UserPromptSubmit": "prompt", "Stop": "stop",
	} {
		var ours []string
		for _, c := range hookCommands(hooks[event]) {
			if strings.Contains(c, "any-claude") {
				t.Errorf("%s keeps the stale any-claude hook %q", event, c)
			}
			if strings.Contains(c, "any-agents") {
				ours = append(ours, c)
			}
		}
		if want := bin + " hook --tool claude " + name; len(ours) != 1 || ours[0] != want {
			t.Errorf("%s hooks of any-agents = %q, want exactly [%q]", event, ours, want)
		}
	}
	if !contains(hookCommands(hooks["SessionStart"]), "echo e2e-keep-me") {
		t.Errorf("install dropped the foreign SessionStart hook: %s", mustJSON(hooks["SessionStart"]))
	}
	if _, err := os.Stat(filepath.Join(toolHome(homeA), "any-agents")); err == nil {
		t.Errorf("bridge state leaked into the tool home %s", toolHome(homeA))
	}
}

// expectedProps are the xKeys each bundle declares (schema.go).
var expectedProps = map[string][]string{
	"project": {"key", "repo", "status", "memories", "lastActivity"},
	"machine": {"hostname", "os", "user", "bridgeVersion", "tools", "lastSeen", "status"},
	"session": {"status", "tool", "project", "machine", "owner", "startedAt", "endedAt", "summary", "linear",
		"transcript", "host", "toolVersion", "sessionId", "cwd", "sharedTo", "sharedFrom"},
	"doc":    {"kind", "project", "originSession", "sessions", "related", "linear", "author", "date", "archived"},
	"task":   {"status", "priority", "assignee", "linearKey", "target", "handledBy", "handledIn", "due"},
	"plan":   {"status", "approvedAt", "executedBy", "executedIn"},
	"linear": {"key", "status", "priority", "team", "assignee", "url", "dueDate", "branch", "syncedAt"},
	"collab": {"status", "requester", "assignees", "project", "linear", "related", "date", "resolvedAt", "originSession", "agreed"},
}

var bundleIds = map[string]string{
	"app": "agents/v1", "project": "agents-project/v1", "machine": "agents-machine/v1",
	"session": "agents-session/v1", "doc": "agents-doc/v1", "task": "agents-task/v1",
	"plan": "agents-plan/v1", "linear": "agents-linear/v1", "collab": "agents-collab/v1",
}

var collectionXKeys = map[string]string{
	"project": "agent_project", "machine": "agent_machine", "session": "agent_session", "doc": "agent_doc",
	"task": "agent_task", "plan": "agent_plan", "linear": "agent_linear_issue",
}

// checkSpaceSetup verifies one space's bundles, collections and views
// against the cached state (the private space and the collab space alike).
func checkSpaceSetup(t *testing.T, sp *spaceState) {
	t.Helper()
	var bundles struct {
		Bundles []struct{ Id, RootId string } `json:"bundles"`
	}
	apiGet(t, "/v1/spaces/"+sp.Id+"/bundles", &bundles)
	byId := map[string]string{}
	for _, b := range bundles.Bundles {
		byId[b.Id] = b.RootId
	}
	names := []string{"app", "project", "machine", "session", "doc", "task", "plan", "linear"}
	if sp.collabOn() {
		names = append(names, "collab")
	}
	for _, name := range names {
		id := bundleIds[name]
		root, ok := byId[id]
		if !ok {
			t.Errorf("%s: bundle %s missing from GET /bundles (have %v)", sp.Name, id, keys(byId))
			continue
		}
		mustEqual(t, root, sp.owner(name), sp.Name+": bundle "+id+" rootId")
		for _, x := range expectedProps[name] {
			if sp.prop(name, x) == "" {
				t.Errorf("%s: state.json caches no property id for %s.%s", sp.Name, name, x)
			}
		}
	}
	if _, ok := byId["system:collections/v1"]; !ok {
		t.Errorf("%s: bundle system:collections/v1 missing from GET /bundles (have %v)", sp.Name, keys(byId))
	}
	if !sp.Usecases["collections"] {
		t.Errorf("%s: state.json usecases = %v, want collections:true", sp.Name, sp.Usecases)
	}

	// the collection bundles are collections of pages
	for name, xkey := range collectionXKeys {
		var raw map[string]any
		apiGet(t, "/v1/spaces/"+sp.Id+"/collections/"+sp.owner(name), &raw)
		if c, ok := raw["collection"].(map[string]any); ok {
			raw = c
		}
		mustEqual(t, fmt.Sprint(raw["xKey"]), xkey, sp.Name+": "+name+" collection xKey")
		meta, _ := raw["meta"].(map[string]any)
		mustEqual(t, fmt.Sprint(meta["defaultType"]), "page", sp.Name+": "+name+" collection meta.defaultType")
	}

	// the app root: a hidden type in the sidebar with the records datasets
	app := apiObject(t, sp.owner("app"), sp.Id)
	if !inCollection(app, "miniapp") {
		t.Errorf("%s: app root is not filed under miniapp: %v", sp.Name, group(app, "any")["collections"])
	}
	mustEqual(t, fmt.Sprint(group(app, "type")["xkey"]), "agents", sp.Name+": app root xKey")
	for _, ds := range []string{"memories", "config", "stats", "feedback"} {
		if sp.coll(ds) == "" {
			t.Errorf("%s: state.json caches no storage collection for the %s dataset", sp.Name, ds)
		}
	}
	if sp.collabOn() {
		mustEqual(t, sp.bundle("collab").Collections["chat"], "chat_messages", sp.Name+": collab chat part")
		mustEqual(t, sp.bundle("collab").Collections["body"], "editor_blocks", sp.Name+": collab body part")
	}

	// the well-known `default` dataview and its views, on one dataview
	// object per browsable collection or type
	wantViews := map[string][]struct{ id, name string }{
		"doc":     {{"default", "Table"}},
		"task":    {{"default", "Open"}, {"all", "All"}},
		"session": {{"default", "Table"}},
		"project": {{"default", "Table"}},
		"machine": {{"default", "Table"}},
		"linear":  {{"default", "Table"}},
	}
	if sp.collabOn() {
		wantViews["collab"] = []struct{ id, name string }{{"default", "Open"}, {"all", "All"}}
	}
	if sp.Kind == "collab" {
		wantViews["task"] = append(wantViews["task"], struct{ id, name string }{"mine", "For You"})
		if sp.collabOn() {
			wantViews["collab"] = append(wantViews["collab"], struct{ id, name string }{"mine", "For You"})
		}
	}
	for name, want := range wantViews {
		host := sp.dataview(name)
		if host == "" {
			t.Errorf("%s %s: state.json caches no dataview object", sp.Name, name)
			continue
		}
		dvObj := apiObject(t, host, sp.Id)
		mustEqual(t, typeOf(dvObj), "dataview", name+" dataview object type")
		mustEqual(t, strOf(dvObj, "dataview", "host"), sp.owner(name), name+" dataview host")
		dvs := apiRecordsIn(t, sp.Id, host, "dataviews", nil)
		if len(dvs) != 1 || dvs[0]["id"] != "default" {
			t.Errorf("%s %s: want one `default` dataview, got %s", sp.Name, name, mustJSON(dvs))
			continue
		}
		if got, _ := dvs[0]["setupVersion"].(float64); int(got) != wantSetupVersion(t) {
			t.Errorf("%s %s: dataview setupVersion %v, want %d", sp.Name, name, dvs[0]["setupVersion"], wantSetupVersion(t))
		}
		views := apiRecordsIn(t, sp.Id, host, "views", nil)
		byViewId := map[string]map[string]any{}
		for _, v := range views {
			id, _ := v["id"].(string)
			byViewId[id] = v
		}
		if len(views) != len(want) {
			t.Errorf("%s %s: want %d view(s) %v, got %v", sp.Name, name, len(want), want, keys(byViewId))
		}
		declared := map[string]bool{"name": true}
		for _, b := range sp.Bundles {
			for _, id := range b.Props {
				declared[id] = true
			}
		}
		for _, w := range want {
			v := byViewId[w.id]
			if v == nil {
				t.Errorf("%s %s: view %q missing (have %v)", sp.Name, name, w.id, keys(byViewId))
				continue
			}
			mustEqual(t, fmt.Sprint(v["name"]), w.name, name+" view "+w.id+" name")
			mustEqual(t, fmt.Sprint(v["layout"]), "table", name+" view "+w.id+" layout")
			mustEqual(t, fmt.Sprint(v["dataview"]), "default", name+" view "+w.id+" dataview")
			// members of a collection, objects of a type
			scopeKey := "any.collections"
			if name == "collab" {
				scopeKey = "any.type"
			}
			if q := mustJSON(v["query"]); !strings.Contains(q, `"`+scopeKey+`":"`+sp.owner(name)+`"`) {
				t.Errorf("%s %s view %s: query does not select %s = %s: %s", sp.Name, name, w.id, scopeKey, sp.owner(name), q)
			}
			// columns are bare property ids (any-ui's column ids)
			ls, _ := v["layoutSettings"].(map[string]any)
			if ls["clickToOpen"] != true || ls["openMode"] != "full-screen" {
				t.Errorf("%s view %s: layoutSettings lacks clickToOpen/openMode: %v", name, w.id, ls)
			}
			for _, key := range []string{"visible", "order"} {
				cols, _ := ls[key].([]any)
				if len(cols) < 2 {
					t.Errorf("%s view %s: layoutSettings.%s = %v, want name + columns", name, w.id, key, cols)
				}
				for _, c := range cols {
					col := fmt.Sprint(c)
					if strings.Contains(col, ".") {
						t.Errorf("%s view %s: layoutSettings.%s has qualified column %q, want a bare property id",
							name, w.id, key, col)
					} else if !declared[col] {
						t.Errorf("%s view %s: layoutSettings.%s column %q is not a property of the bundle",
							name, w.id, key, col)
					}
				}
			}
		}
	}
	// the task `default` view is the open queue, `all` is unfiltered
	statusPath := sp.propPath("task", "status")
	openFilter := mustJSON(viewOf(t, sp, "task", "default")["query"])
	if !strings.Contains(openFilter, statusPath) || !strings.Contains(openFilter, "in_progress") {
		t.Errorf("task `default` view query does not filter on open status: %s", openFilter)
	}
	if allFilter := mustJSON(viewOf(t, sp, "task", "all")["query"]); strings.Contains(allFilter, statusPath) {
		t.Errorf("task `all` view query filters on status: %s", allFilter)
	}
}

func viewOf(t *testing.T, sp *spaceState, bundle, viewId string) map[string]any {
	t.Helper()
	for _, v := range apiRecordsIn(t, sp.Id, sp.dataview(bundle), "views", nil) {
		if v["id"] == viewId {
			return v
		}
	}
	t.Fatalf("%s dataview has no view %q", bundle, viewId)
	return nil
}

// hookCommands lists the commands of one settings.json hook event.
func hookCommands(v any) []string {
	var out []string
	entries, _ := v.([]any)
	for _, e := range entries {
		em, _ := e.(map[string]any)
		hs, _ := em["hooks"].([]any)
		for _, h := range hs {
			hm, _ := h.(map[string]any)
			if c, ok := hm["command"].(string); ok {
				out = append(out, c)
			}
		}
	}
	return out
}

// --- 2. session start hook ---

func testSessionStartHook(t *testing.T) {
	st := loadStateT(t, homeA)
	ps := st.priv()
	ctx := sessionStartHook(t, sidA, "startup")
	mustContain(t, ctx, "shared context is on", "session-start context")
	mustContain(t, ctx, "space "+spaceName, "session-start context names the space")
	mustContain(t, ctx, fmt.Sprintf("project %q", scope), "session-start context")

	row := sessionRow(t, ps, sidA)
	sessionObjA, _ = row["id"].(string)
	mustContain(t, ctx, "any://o/"+spaceId+"/"+sessionObjA, "session-start context names the session page link")

	s := ps.owner("session")
	mustEqual(t, typeOf(row), "page", "session object type")
	mustBeFiled(t, row, s, "session page")
	if !strings.HasPrefix(nameOf(row), scope+" ") {
		t.Errorf("session page name = %q, want it to start with the scope", nameOf(row))
	}
	mustEqual(t, choiceOf(row, s, ps.prop("session", "status")), "active", "session status")
	mustEqual(t, choiceOf(row, s, ps.prop("session", "tool")), "claude", "session tool")
	mustEqual(t, strOf(row, s, ps.prop("session", "cwd")), projDir, "session cwd")
	mustEqual(t, strOf(row, s, ps.prop("session", "host")), hostName, "session host")
	mustEqual(t, strOf(row, s, ps.prop("session", "sessionId")), sidA, "session id property")
	mustEqualSlice(t, arrOf(row, s, ps.prop("session", "owner")), []string{"any://u/" + identity}, "session owner")
	if group(row, s)[ps.prop("session", "startedAt")] == nil {
		t.Error("session-start did not set startedAt")
	}
	mustContain(t, apiMarkdown(t, sessionObjA), sidA, "session body header")

	projectObj = firstLink(row, s, ps.prop("session", "project"))
	if projectObj == "" {
		t.Fatal("session has no project relation")
	}
	project := apiObject(t, projectObj)
	p := ps.owner("project")
	mustEqual(t, nameOf(project), scope, "project page name")
	mustEqual(t, typeOf(project), "page", "project object type")
	mustBeFiled(t, project, p, "project page")
	mustEqual(t, strOf(project, p, ps.prop("project", "key")), scope, "project key")
	mustEqual(t, choiceOf(project, p, ps.prop("project", "status")), "active", "project status")
	// the project page is the app root's deterministic child for the scope
	mustEqual(t, bundleChild(t, spaceId, "agents/v1", "project:"+scope, p), projectObj,
		"project page = deterministic child project:<scope>")
	mustEqual(t, firstLink(row, s, ps.prop("session", "machine")), ps.MachineObjectId, "session machine relation")
	mustOnlyDeclaredProps(t, ps, "session", row)
	mustEqual(t, loadStateT(t, homeA).LastSessionId, sidA, "state.json lastSessionId")

	// a resume keeps the one page
	sessionStartHook(t, sidA, "resume")
	mustEqual(t, sessionRow(t, ps, sidA)["id"], any(sessionObjA), "session page after resume")
	mustContain(t, apiMarkdown(t, sessionObjA), "Resumed ", "session body after resume")
}

// --- 3. memory sync ---

func testMemorySync(t *testing.T) {
	ps := loadStateT(t, homeA).priv()
	memDir := memoryDir(homeA, projDir)
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	alpha := memFile("e2e-alpha", "Alpha memory for the e2e suite", "project", "Alpha body, first revision.")
	beta := memFile("e2e-beta", "Beta memory for the e2e suite", "reference", "Beta body, only revision.")
	writeFile(t, filepath.Join(memDir, "alpha.md"), alpha)
	writeFile(t, filepath.Join(memDir, "beta.md"), beta)

	runCLI(t, runOpts{}, "push")
	recs := memRecords(t, ps)
	mustEqual(t, len(recs), 2, "memory records after push")
	if r := recs[scope+":e2e-alpha"]; r == nil {
		t.Errorf("no record %s:e2e-alpha (have %v)", scope, keys(recs))
	} else {
		mustEqual(t, fmt.Sprint(r["title"]), "Alpha memory for the e2e suite", "alpha title")
		mustEqual(t, fmt.Sprint(r["kind"]), "project", "alpha kind")
		mustEqual(t, fmt.Sprint(r["origin"]), hostName, "alpha origin")
		mustContain(t, fmt.Sprint(r["body"]), "first revision", "alpha body")
	}
	if r := recs[scope+":e2e-beta"]; r == nil {
		t.Errorf("no record %s:e2e-beta (have %v)", scope, keys(recs))
	} else {
		mustEqual(t, fmt.Sprint(r["kind"]), "reference", "beta kind")
	}

	// an edit is pushed as an update
	writeFile(t, filepath.Join(memDir, "alpha.md"),
		memFile("e2e-alpha", "Alpha memory for the e2e suite", "project", "Alpha body, second revision."))
	runCLI(t, runOpts{}, "push")
	recs = memRecords(t, ps)
	mustContain(t, fmt.Sprint(recs[scope+":e2e-alpha"]["body"]), "second revision", "alpha body after edit")

	// a local delete retracts the record (a deleted record id could never
	// be reused); readers treat a retracted record as absent
	if err := os.Remove(filepath.Join(memDir, "beta.md")); err != nil {
		t.Fatal(err)
	}
	runCLI(t, runOpts{}, "push")
	recs = memRecords(t, ps)
	if r, ok := recs[scope+":e2e-beta"]; !ok {
		t.Error("record e2e-beta vanished: a removed file retracts its record, never deletes it")
	} else {
		mustEqual(t, fmt.Sprint(r["kind"]), "retracted", "beta kind after delete")
		mustEqual(t, fmt.Sprint(r["body"]), "", "beta body after delete")
	}
	live := 0
	for _, r := range recs {
		if fmt.Sprint(r["kind"]) != "retracted" {
			live++
		}
	}
	mustEqual(t, live, 1, "live memory records after delete")

	// the project page tracks the count
	mustEqual(t, fmt.Sprint(numOf(apiObject(t, projectObj), ps.owner("project"), ps.prop("project", "memories"))),
		"1", "project memories count")

	// a second machine pulls the scope into its own memory dir
	raw, err := os.ReadFile(statePath(homeA))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, statePath(homeB), string(raw))

	out, _ := runCLI(t, runOpts{home: homeB}, "pull")
	mustContain(t, out, "1 memories in scope", "pull output")
	memDirB := memoryDir(homeB, projDir)
	pulled := readFile(t, filepath.Join(memDirB, "e2e-alpha.md"))
	mustContain(t, pulled, "second revision", "pulled memory body")
	mustContain(t, pulled, "name: e2e-alpha", "pulled memory frontmatter")
	index := readFile(t, filepath.Join(memDirB, "MEMORY.md"))
	mustContain(t, index, "# Memory index", "MEMORY.md")
	mustContain(t, index, "e2e-alpha", "MEMORY.md lists the memory")
	if strings.Contains(index, "e2e-beta") {
		t.Errorf("MEMORY.md still lists the deleted memory:\n%s", index)
	}
	if _, err := os.Stat(filepath.Join(memDirB, "e2e-beta.md")); err == nil {
		t.Error("pull recreated the deleted memory file")
	}
	// sync fingerprints live in the bridge home, not the tool home
	if _, err := os.Stat(filepath.Join(bridgeHome(homeB), "sync")); err != nil {
		t.Errorf("no sync state under %s: %v", bridgeHome(homeB), err)
	}
}

// --- 3b. config ---

func testConfigSync(t *testing.T) {
	ps := loadStateT(t, homeA).priv()
	confA, confB := toolHome(homeA), toolHome(homeB)
	syncedRel := "skills/synced/org_user/managed/SKILL.md"
	longRel := "skills/long/" + strings.Repeat("x", 120) + ".md"
	writeFile(t, filepath.Join(confA, "CLAUDE.md"), "# Global rules from the e2e suite\n")
	writeFile(t, filepath.Join(confA, "skills/mine/SKILL.md"), "Own skill from the e2e suite.\n")
	writeFile(t, filepath.Join(confA, syncedRel), "Claude Code's copy on machine A.\n")
	writeFile(t, filepath.Join(confA, longRel), "Too long to mirror.\n")
	writeFile(t, filepath.Join(confB, syncedRel), "Claude Code's copy on machine B.\n")

	// a synced-skill record left by an older binary
	staleId := "claude:" + strings.ReplaceAll(syncedRel, "/", ":")
	apiPost(t, "/v1/spaces/"+spaceId+"/upsert", map[string]any{
		"objectId": ps.owner("app"), "dataset": ps.coll("config"),
		"records": []map[string]any{{"id": staleId, "fields": map[string]any{
			"path": syncedRel, "sha256": "stale", "kind": "text", "body": "Stale remote copy.\n"}}},
	}, nil)

	runCLI(t, runOpts{home: homeB}, "pull")
	mustEqual(t, readFile(t, filepath.Join(confB, syncedRel)), "Claude Code's copy on machine B.\n",
		"synced skill on B after pulling the stale record")

	_, errOut := runCLI(t, runOpts{}, "push")
	mustContain(t, errOut, "skip "+longRel+" (path longer than a record id allows)", "push stderr")
	byPath := map[string]map[string]any{}
	for _, r := range apiRecords(t, ps.owner("app"), ps.coll("config"), nil) {
		byPath[fmt.Sprint(r["path"])] = r
	}
	for _, want := range []string{"CLAUDE.md", "skills/mine/SKILL.md"} {
		r := byPath[want]
		if r == nil {
			t.Errorf("config record %s missing after push (have %v)", want, keys(byPath))
			continue
		}
		mustEqual(t, fmt.Sprint(r["id"]), "claude:"+strings.ReplaceAll(want, "/", ":"), want+" record id")
		mustEqual(t, fmt.Sprint(r["tool"]), "claude", want+" record tool")
	}
	for p, r := range byPath {
		if (strings.HasPrefix(p, "skills/synced/") || strings.HasPrefix(p, "skills/long/")) && fmt.Sprint(r["kind"]) != "retracted" {
			t.Errorf("config record %s should not be mirrored (kind %v)", p, r["kind"])
		}
	}

	runCLI(t, runOpts{home: homeB}, "pull")
	mustEqual(t, readFile(t, filepath.Join(confB, "CLAUDE.md")), "# Global rules from the e2e suite\n",
		"CLAUDE.md pulled on B")
	mustEqual(t, readFile(t, filepath.Join(confB, "skills/mine/SKILL.md")), "Own skill from the e2e suite.\n",
		"own skill pulled on B")
	mustEqual(t, readFile(t, filepath.Join(confB, syncedRel)), "Claude Code's copy on machine B.\n",
		"synced skill on B after the stale record is gone")

	// later hooks push config too: keep their stderr free of the skip notice
	if err := os.RemoveAll(filepath.Join(confA, "skills/long")); err != nil {
		t.Fatal(err)
	}
}

// --- 4. docs ---

func testDocs(t *testing.T) {
	st := loadStateT(t, homeA)
	ps := st.priv()
	bodyText := "This report belongs to the any-agents end-to-end suite.\n\nThe " + phrase + "."
	body := "# E2E Report\n\n" + bodyText + "\n"
	out, errOut := runCLI(t, runOpts{sid: sidA, stdin: body}, "doc", "add", "-", "--kind", "report", "--linear", "SYN-1")
	docObj = linkId(t, out)
	mustContain(t, out, "any://o/"+spaceId+"/"+docObj, "doc add prints the doc's link")
	mustNotLogErrors(t, errOut, "doc add")

	row := apiObject(t, docObj)
	d := ps.owner("doc")
	mustEqual(t, typeOf(row), "page", "doc object type")
	mustBeFiled(t, row, d, "doc")
	if inCollection(row, ps.owner("task")) {
		t.Errorf("a plain doc is filed under Task: %v", group(row, "any")["collections"])
	}
	mustEqualSlice(t, arrOf(row, d, ps.prop("doc", "kind")), []string{"report"}, "doc kind")
	mustEqual(t, firstLink(row, d, ps.prop("doc", "linear")), linearIssueId(t, ps, "SYN-1"), "doc linear relation")
	mustEqual(t, firstLink(row, d, ps.prop("doc", "project")), projectObj, "doc project relation")
	mustEqual(t, firstLink(row, d, ps.prop("doc", "originSession")), sessionObjA, "doc originSession relation")
	mustEqualSlice(t, arrOf(row, d, ps.prop("doc", "author")), []string{"any://u/" + identity}, "doc author")
	if date := group(row, d)[ps.prop("doc", "date")]; date == nil {
		t.Errorf("doc add did not set the date property (doc group: %s)", mustJSON(group(row, d)))
	}
	mustOnlyDeclaredProps(t, ps, "doc", row)
	// the object name doubles as the title, so a leading H1 that repeats it
	// is dropped from the stored body
	mustNoTitleH1(t, apiMarkdown(t, docObj), "E2E Report", "doc body")
	mustEqual(t, strings.TrimSpace(apiMarkdown(t, docObj)), bodyText, "doc body round-trip without the H1")

	out, _ = runCLI(t, runOpts{sid: sidA}, "doc", "list")
	mustContain(t, out, "E2E Report", "doc list")
	mustContain(t, out, "report", "doc list kind column")
	mustContain(t, out, "SYN-1", "doc list linear column")

	// --linear minted the issue page and the listing knows it
	out, _ = runCLI(t, runOpts{sid: sidA}, "linear", "list")
	mustContain(t, out, "SYN-1", "linear list")
	out, _ = runCLI(t, runOpts{sid: sidA}, "linear", "show", "SYN-1")
	mustContain(t, out, "# SYN-1", "linear show")
	waitFor(t, 20*time.Second, "the doc to show up among the issue's backlinks", func() bool {
		out, _ = runCLI(t, runOpts{sid: sidA}, "linear", "show", "SYN-1")
		return strings.Contains(out, "← any://o/"+spaceId+"/"+docObj)
	})

	// re-adding the same title updates the object in place
	body2 := "# E2E Report\n\nSecond revision of the report.\n\nThe " + phrase + ".\n\nMARKER-ORIGINAL holds until the edit test.\n"
	out, _ = runCLI(t, runOpts{sid: sidA, stdin: body2}, "doc", "add", "-", "--kind", "report", "--pr", "https://github.com/anyproto/any-agents/pull/2")
	mustEqual(t, strOf(apiObject(t, linkId(t, out)), ps.owner("doc"), ps.prop("doc", "pr")), "https://github.com/anyproto/any-agents/pull/2", "doc pr")
	mustEqual(t, linkId(t, out), docObj, "re-add with the same title returns the same object")
	mustEqual(t, len(docsNamed(t, ps, "E2E Report")), 1, "objects named E2E Report after re-add")
	mustContain(t, apiMarkdown(t, docObj), "Second revision", "doc body after re-add")
	mustNoTitleH1(t, apiMarkdown(t, docObj), "E2E Report", "doc body after re-add")

	// --new forces a second object with the same title
	dup := "# E2E Dup Doc\n\nFirst copy.\n"
	runCLI(t, runOpts{sid: sidA, stdin: dup}, "doc", "add", "-")
	mustEqual(t, len(docsNamed(t, ps, "E2E Dup Doc")), 1, "objects named E2E Dup Doc after the first add")
	runCLI(t, runOpts{sid: sidA, stdin: "# E2E Dup Doc\n\nSecond copy.\n"}, "doc", "add", "-", "--new")
	mustEqual(t, len(docsNamed(t, ps, "E2E Dup Doc")), 2, "objects named E2E Dup Doc after --new")

	// append + edit change the body; ids accept the any://o link
	link := "any://o/" + spaceId + "/" + docObj
	runCLI(t, runOpts{sid: sidA}, "doc", "append", docObj, "APPENDED-LINE from the e2e suite.")
	mustContain(t, apiMarkdown(t, docObj), "APPENDED-LINE", "doc body after append")
	appendSrc := filepath.Join(tmpRoot, "append.md")
	writeFile(t, appendSrc, "FILE-APPENDED section from the e2e suite.\n")
	runCLI(t, runOpts{sid: sidA}, "doc", "append", link, appendSrc)
	if md := apiMarkdown(t, docObj); !strings.Contains(md, "FILE-APPENDED") || strings.Contains(md, appendSrc) {
		t.Errorf("doc append <file> should append the file's contents, not its path:\n%s", md)
	}
	runCLI(t, runOpts{sid: sidA}, "doc", "edit", docObj, "--old", "MARKER-ORIGINAL", "--new", "MARKER-EDITED")
	md := apiMarkdown(t, docObj)
	mustContain(t, md, "MARKER-EDITED", "doc body after edit")
	if strings.Contains(md, "MARKER-ORIGINAL") {
		t.Errorf("doc edit left the old text in place:\n%s", md)
	}

	// show renders the body
	out, _ = runCLI(t, runOpts{sid: sidA}, "doc", "show", link)
	mustContain(t, out, "# E2E Report", "doc show")
	mustContain(t, out, link, "doc show link")
	mustContain(t, out, "kind=report", "doc show meta")
	mustContain(t, out, "linear=SYN-1", "doc show meta")
	mustContain(t, out, "APPENDED-LINE", "doc show body")

	// stdin with neither --title nor an H1 is refused
	_, errOut = runCLI(t, runOpts{sid: sidA, stdin: "no heading here\n", wantFail: true}, "doc", "add", "-")
	mustContain(t, errOut, "pass --title", "doc add without a title")

	// a link into a space this machine does not know is refused
	_, errOut = runCLI(t, runOpts{sid: sidA, wantFail: true}, "doc", "show", "any://o/bafyunknownspace.e2e/"+docObj)
	mustContain(t, errOut, "is not set up here", "doc show with a link into an unknown space")

	// archive hides it from the default listing only
	runCLI(t, runOpts{sid: sidA}, "doc", "archive", docObj)
	if v, _ := group(apiObject(t, docObj), d)[ps.prop("doc", "archived")].(bool); !v {
		t.Error("doc archive did not set the archived property")
	}
	out, _ = runCLI(t, runOpts{sid: sidA}, "doc", "list")
	if strings.Contains(out, "E2E Report") {
		t.Errorf("archived doc still shows in `doc list`:\n%s", out)
	}
	out, _ = runCLI(t, runOpts{sid: sidA}, "doc", "list", "--all")
	mustContain(t, out, "E2E Report", "doc list --all")
}

// --- 5. tasks ---

func testTasks(t *testing.T) {
	ps := loadStateT(t, homeA).priv()
	tk, d := ps.owner("task"), ps.owner("doc")
	out, _ := runCLI(t, runOpts{sid: sidA, stdin: "# E2E Task One\n\nHandle the first e2e task.\n"}, "task", "add", "-")
	taskObj = linkId(t, out)

	row := apiObject(t, taskObj)
	mustEqual(t, typeOf(row), "page", "task object type")
	mustBeFiled(t, row, d, "task (Doc)")
	mustBeFiled(t, row, tk, "task (Task)")
	if n := len(apiQueryObjects(t, collectionFilter(tk, map[string]any{"any.name": "E2E Task One"}))); n != 1 {
		t.Errorf("want one member of the Task collection named E2E Task One, got %d", n)
	}
	mustEqual(t, choiceOf(row, d, ps.prop("doc", "kind")), "prompt", "task doc kind")
	mustEqual(t, choiceOf(row, tk, ps.prop("task", "status")), "todo", "task status after add")
	mustEqual(t, choiceOf(row, tk, ps.prop("task", "priority")), "normal", "task priority after add")
	if a := arrOf(row, tk, ps.prop("task", "assignee")); len(a) != 0 {
		t.Errorf("task add without --assignee assigned %v", a)
	}
	mustOnlyDeclaredProps(t, ps, "task", row)

	out, _ = runCLI(t, runOpts{sid: sidA}, "task", "list")
	mustContain(t, out, "E2E Task One", "task list")
	mustContain(t, out, "todo", "task list status column")

	out, _ = runCLI(t, runOpts{sid: sidA}, "task", "show", taskObj)
	mustContain(t, out, "Handle the first e2e task", "task show body")
	mustContain(t, out, "task=todo", "task show meta")

	// starting an unassigned task assigns it to me
	runCLI(t, runOpts{sid: sidA}, "task", "start", taskObj)
	row = apiObject(t, taskObj)
	mustEqual(t, choiceOf(row, tk, ps.prop("task", "status")), "in_progress", "task status after start")
	mustEqualSlice(t, arrOf(row, tk, ps.prop("task", "assignee")), []string{"any://u/" + identity}, "task assignee after start")

	// the next session start only counts open tasks; it never lists them
	// (that made the agent ask about them on every start)
	ctx := sessionStartHook(t, sidB, "startup")
	mustContain(t, ctx, "1 open task(s)", "session-start context counts the open task")
	if strings.Contains(ctx, "PENDING TASKS") || strings.Contains(ctx, "E2E Task One") {
		t.Errorf("session-start must not list pending tasks:\n%s", ctx)
	}

	_, errOut := runCLI(t, runOpts{sid: sidA}, "task", "done", "any://o/"+spaceId+"/"+taskObj)
	mustNotLogErrors(t, errOut, "task done")
	row = apiObject(t, taskObj)
	mustEqual(t, choiceOf(row, tk, ps.prop("task", "status")), "done", "task status after done")
	mustEqual(t, firstLink(row, tk, ps.prop("task", "handledBy")), ps.MachineObjectId, "task handledBy")
	mustEqual(t, firstLink(row, tk, ps.prop("task", "handledIn")), sessionObjA, "task handledIn")

	out, _ = runCLI(t, runOpts{sid: sidA}, "task", "list")
	if strings.Contains(out, "E2E Task One") {
		t.Errorf("done task still shows in `task list`:\n%s", out)
	}
	out, _ = runCLI(t, runOpts{sid: sidA}, "task", "list", "--all")
	mustContain(t, out, "E2E Task One", "task list --all")

	ctx = sessionStartHook(t, sidB, "resume")
	mustContain(t, ctx, "0 open task(s)", "session-start count after `task done`")
}

// --- 5b. assignee ---

func testTaskAssignee(t *testing.T) {
	ps := loadStateT(t, homeA).priv()
	tk := ps.owner("task")
	me := "any://u/" + identity
	assigneeOf := func(id string) []string { return arrOf(apiObject(t, id), tk, ps.prop("task", "assignee")) }

	// --assignee takes an identity (or a member name, see the collab space)
	out, _ := runCLI(t, runOpts{sid: sidA, stdin: "# E2E Task Mine\n\nAssigned to me.\n"},
		"task", "add", "-", "--assignee", identity, "--priority", "high")
	mine := linkId(t, out)
	mustEqualSlice(t, assigneeOf(mine), []string{me}, "assignee of a task added with --assignee <identity>")
	mustEqual(t, choiceOf(apiObject(t, mine), tk, ps.prop("task", "priority")), "high", "task priority")

	// a task of someone else (a member link nobody here holds)
	out, _ = runCLI(t, runOpts{sid: sidA, stdin: "# E2E Task Theirs\n\nSomeone else's.\n"}, "task", "add", "-")
	theirs := linkId(t, out)
	apiPost(t, "/v1/spaces/"+spaceId+"/properties/"+theirs+"/set/"+tk,
		map[string]any{"patch": map[string]any{ps.prop("task", "assignee"): []string{"any://u/" + fakeIdentity}}}, nil)

	// --assignee rejects a non-member
	_, errOut := runCLI(t, runOpts{sid: sidA, stdin: "# E2E Task Nobody\n\nNo such member.\n", wantFail: true},
		"task", "add", "-", "--assignee", "nobody-in-this-space")
	mustContain(t, errOut, "is not a member of", "task add --assignee with a non-member")

	out, _ = runCLI(t, runOpts{sid: sidA}, "task", "list")
	mustContain(t, out, "E2E Task Mine", "task list")
	mustContain(t, out, "E2E Task Theirs", "task list")
	out, _ = runCLI(t, runOpts{sid: sidA}, "task", "list", "--mine")
	mustContain(t, out, "E2E Task Mine", "task list --mine shows my task")
	if strings.Contains(out, "E2E Task Theirs") {
		t.Errorf("task list --mine shows a task assigned to someone else:\n%s", out)
	}
	out, _ = runCLI(t, runOpts{sid: sidA}, "task", "show", mine)
	mustContain(t, out, "assignee=", "task show names the assignee")
	mustContain(t, out, "priority=high", "task show priority")

	// the session-start count is "unassigned or me": mine counts, theirs not
	ctx := sessionStartHook(t, sidB, "resume")
	mustContain(t, ctx, "1 open task(s)", "session-start count with my task and someone else's")

	// starting someone else's task keeps their assignee
	runCLI(t, runOpts{sid: sidA}, "task", "start", theirs)
	mustEqualSlice(t, assigneeOf(theirs), []string{"any://u/" + fakeIdentity}, "assignee after starting someone else's task")

	for _, id := range []string{mine, theirs} {
		runCLI(t, runOpts{sid: sidA}, "task", "cancel", id)
		mustEqual(t, choiceOf(apiObject(t, id), tk, ps.prop("task", "status")), "cancelled", "task status after cancel")
	}
	out, _ = runCLI(t, runOpts{sid: sidA}, "task", "list")
	if strings.Contains(out, "E2E Task Mine") || strings.Contains(out, "E2E Task Theirs") {
		t.Errorf("cancelled tasks still show in `task list`:\n%s", out)
	}
}

// --- 6. plans ---

func testPlans(t *testing.T) {
	ps := loadStateT(t, homeA).priv()
	pl := ps.owner("plan")
	out, _ := runCLI(t, runOpts{sid: sidA, stdin: "# E2E Task Two\n\nHandle the second e2e task.\n"}, "task", "add", "-")
	task2Obj = linkId(t, out)

	planBody := "First step of the e2e plan.\n\nSecond step of the e2e plan.\n"
	out, errOut := runCLI(t, runOpts{sid: sidA, stdin: planBody}, "plan", "add", task2Obj, "-", "--status", "approved")
	mustEqual(t, linkId(t, out), task2Obj, "plan add reports the task object")
	mustNotLogErrors(t, errOut, "plan add")

	row := apiObject(t, task2Obj)
	mustBeFiled(t, row, pl, "task with a plan (Plan)")
	mustBeFiled(t, row, ps.owner("task"), "task with a plan (Task)")
	mustEqual(t, choiceOf(row, pl, ps.prop("plan", "status")), "approved", "plan status")
	md := apiMarkdown(t, task2Obj)
	mustContain(t, md, "## Plan", "task body after plan add")
	mustContain(t, md, "First step of the e2e plan", "plan body in the task body")
	mustContain(t, md, "Handle the second e2e task", "original task body kept")

	// the plan run is recorded on the doc's sessions relation
	if got := linksOf(row, ps.owner("doc"), ps.prop("doc", "sessions")); !contains(got, sessionObjA) {
		t.Errorf("doc sessions = %v, want it to include the session %s", got, sessionObjA)
	}

	out, _ = runCLI(t, runOpts{sid: sidA}, "plan", "list")
	mustContain(t, out, "E2E Task Two", "plan list")
	mustContain(t, out, "approved", "plan list status column")
	out, _ = runCLI(t, runOpts{sid: sidA}, "plan", "show", task2Obj)
	mustContain(t, out, "plan=approved", "plan show meta")

	// task done closes the plan too
	runCLI(t, runOpts{sid: sidA}, "task", "done", task2Obj)
	mustEqual(t, choiceOf(apiObject(t, task2Obj), pl, ps.prop("plan", "status")), "done", "plan status after task done")
	out, _ = runCLI(t, runOpts{sid: sidA}, "plan", "list")
	if strings.Contains(out, "E2E Task Two") {
		t.Errorf("done plan still shows in `plan list`:\n%s", out)
	}
	out, _ = runCLI(t, runOpts{sid: sidA}, "plan", "list", "--all")
	mustContain(t, out, "E2E Task Two", "plan list --all")

	// a standalone plan is a Doc of kind plan filed under Plan
	runCLI(t, runOpts{sid: sidA, stdin: "Standalone e2e plan body.\n"},
		"plan", "add", "-", "--title", "E2E Standalone Plan")
	rows := docsNamed(t, ps, "E2E Standalone Plan")
	if len(rows) != 1 {
		t.Fatalf("want exactly one E2E Standalone Plan doc, got %d", len(rows))
	}
	row = rows[0]
	planId, _ := row["id"].(string)
	mustEqual(t, choiceOf(row, ps.owner("doc"), ps.prop("doc", "kind")), "plan", "standalone plan doc kind")
	mustBeFiled(t, row, pl, "standalone plan")
	mustEqual(t, choiceOf(row, pl, ps.prop("plan", "status")), "draft", "standalone plan status")
	mustContain(t, apiMarkdown(t, planId), "Standalone e2e plan body", "standalone plan body")

	// approve, then done records the machine and the session
	runCLI(t, runOpts{sid: sidA}, "plan", "approve", planId)
	row = apiObject(t, planId)
	mustEqual(t, choiceOf(row, pl, ps.prop("plan", "status")), "approved", "plan status after approve")
	if group(row, pl)[ps.prop("plan", "approvedAt")] == nil {
		t.Error("plan approve did not set approvedAt")
	}
	runCLI(t, runOpts{sid: sidA}, "plan", "done", planId)
	row = apiObject(t, planId)
	mustEqual(t, choiceOf(row, pl, ps.prop("plan", "status")), "done", "plan status after done")
	mustEqual(t, firstLink(row, pl, ps.prop("plan", "executedBy")), ps.MachineObjectId, "plan executedBy")
	if got := linksOf(row, pl, ps.prop("plan", "executedIn")); !contains(got, sessionObjA) {
		t.Errorf("plan executedIn = %v, want it to include the session %s", got, sessionObjA)
	}
}

// --- 7. session lifecycle ---

func testSessionLifecycle(t *testing.T) {
	ps := loadStateT(t, homeA).priv()
	s := ps.owner("session")
	if sessionObjA == "" {
		t.Fatal("no session page from the session-start test")
	}

	runCLI(t, runOpts{sid: sidA}, "session", "note", "A note appended by the e2e suite.")
	// -s also takes the session page's id or link, which is what the
	// session-start context prints
	runCLI(t, runOpts{}, "session", "note", "-s", "any://o/"+spaceId+"/"+sessionObjA, "A note by object link.")
	mustContain(t, apiMarkdown(t, sessionObjA), "A note by object link.", "session note -s <object link>")
	sessionBody := apiMarkdown(t, sessionObjA)
	mustContain(t, sessionBody, "A note appended by the e2e suite", "session body after note")
	mustContain(t, sessionBody, "Started ", "session body header")
	if strings.HasPrefix(strings.TrimSpace(sessionBody), "# ") {
		t.Errorf("session body starts with an H1 title:\n%s", sessionBody)
	}

	runCLI(t, runOpts{sid: sidA}, "session", "update", "--summary", "E2E session summary", "--linear", "SYN-2")
	row := apiObject(t, sessionObjA)
	mustEqual(t, strOf(row, s, ps.prop("session", "summary")), "E2E session summary", "session summary")
	if got := linksOf(row, s, ps.prop("session", "linear")); !contains(got, linearIssueId(t, ps, "SYN-2")) {
		t.Errorf("session linear = %v, want it to include SYN-2", got)
	}

	// nothing writes stray keys into the session's property group
	mustOnlyDeclaredProps(t, ps, "session", apiObject(t, sessionObjA))

	out, _ := runCLI(t, runOpts{sid: sidA}, "session", "list")
	mustContain(t, out, "E2E session summary", "session list")
	mustContain(t, out, scope, "session list project column")

	transcript := "{\"type\":\"user\",\"message\":\"e2e hello\"}\n{\"type\":\"assistant\",\"message\":\"e2e hi\"}\n"
	tPath := filepath.Join(tmpRoot, "transcript.jsonl")
	writeFile(t, tPath, transcript)

	in := map[string]string{"session_id": sidA, "cwd": projDir, "transcript_path": tPath}
	_, errOut := runCLI(t, runOpts{stdin: mustJSON(in)}, "hook", "--tool", "claude", "session-end")
	mustNotLogErrors(t, errOut, "session-end hook")

	row = apiObject(t, sessionObjA)
	mustEqual(t, choiceOf(row, s, ps.prop("session", "status")), "ended", "session status after session-end")
	if endedAt := group(row, s)[ps.prop("session", "endedAt")]; endedAt == nil {
		t.Error("session-end did not set endedAt")
	}
	link := strOf(row, s, ps.prop("session", "transcript"))
	prefix := "any://f/" + spaceId + "/"
	if !strings.HasPrefix(link, prefix) || len(link) == len(prefix) {
		t.Errorf("transcript link = %q, want %s<fileId>", link, prefix)
	}
	mustEqual(t, choiceOf(apiObject(t, ps.MachineObjectId), ps.owner("machine"), ps.prop("machine", "status")),
		"idle", "machine status after session-end")

	// the session also worked on SYN-1: `doc add --linear` ran inside it
	out, _ = runCLI(t, runOpts{sid: sidA}, "session", "show")
	mustContain(t, out, "status=ended", "session show meta")
	mustContain(t, out, "linear=SYN-1,SYN-2", "session show linear")

	outPath := filepath.Join(tmpRoot, "transcript-restored.jsonl")
	out, _ = runCLI(t, runOpts{sid: sidA}, "session", "transcript", "-o", outPath)
	mustContain(t, out, "wrote", "session transcript output")
	mustEqual(t, readFile(t, outPath), transcript, "restored transcript bytes")
}

// --- 8. a collab space ---

func testCollabSpace(t *testing.T) {
	out, errOut := runCLI(t, runOpts{sid: sidA}, "space", "create", teamName)
	m := regexp.MustCompile(`(?m)^(\S+)  ` + regexp.QuoteMeta(teamName) + ` created`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("space create printed no `<id>  %s created` line:\n%s\n%s", teamName, out, errOut)
	}
	teamId = m[1]
	trackSpace(teamId)

	st := loadStateT(t, homeA)
	ts := st.Spaces[teamId]
	if ts == nil {
		t.Fatalf("state.json has no space %s after space create (have %v)", teamId, keys(st.Spaces))
	}
	mustEqual(t, ts.Name, teamName, "collab space name")
	mustEqual(t, ts.Kind, "collab", "collab space kind")
	mustEqual(t, ts.MachineObjectId, "", "collab space machine page (machines stay private)")
	mustEqual(t, ts.SetupVersion, wantSetupVersion(t), "collab space setupVersion")
	if !ts.collabOn() {
		mustContain(t, errOut, "collabs are off in "+teamName, "space create warning without the Collab bundle")
	}
	checkSpaceSetup(t, ts)
	if ts.owner("doc") == st.priv().owner("doc") {
		t.Errorf("the collab space reuses the private space's Doc collection id %s", ts.owner("doc"))
	}

	out, _ = runCLI(t, runOpts{sid: sidA}, "space", "list")
	for _, want := range []struct{ id, kind string }{{spaceId, "private"}, {teamId, "collab"}} {
		if !regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(want.id) + `\s+` + want.kind + `\s`).MatchString(out) {
			t.Errorf("space list has no %s line for %s:\n%s", want.kind, want.id, out)
		}
	}

	// a view edited in any-ui survives a re-ensure, a stock view is restored
	dvHost := ts.dataview("collab")
	if dvHost != "" {
		bare := map[string]any{"visible": []string{"name"}, "order": []string{"name"}}
		apiPost(t, "/v1/spaces/"+teamId+"/modify", map[string]any{"objectId": dvHost, "dataset": "views", "records": []map[string]any{
			{"id": "all", "ops": []map[string]any{{"type": "$set", "path": "query.anyUi", "value": map[string]any{"version": 1}}, {"type": "$set", "path": "layoutSettings", "value": bare}}},
			{"id": "default", "ops": []map[string]any{{"type": "$set", "path": "layoutSettings", "value": bare}}},
		}}, nil)
		lowerSetupVersion(t, homeA, teamId, dvHost)
		runCLI(t, runOpts{sid: sidA}, "collab", "list", "--space", teamName)
		for _, v := range apiRecordsIn(t, teamId, dvHost, "views", nil) {
			ls, _ := v["layoutSettings"].(map[string]any)
			vis, _ := ls["visible"].([]any)
			switch v["id"] {
			case "all":
				if len(vis) != 1 {
					t.Errorf("re-ensure overwrote a view edited in any-ui: %v", vis)
				}
			case "default":
				if len(vis) < 2 {
					t.Errorf("re-ensure left a stock view's columns at %v", vis)
				}
			}
		}
		if dvs := apiRecordsIn(t, teamId, dvHost, "dataviews", nil); len(dvs) != 1 || int(dvs[0]["setupVersion"].(float64)) != wantSetupVersion(t) {
			t.Errorf("dataview setupVersion after re-ensure: %s", mustJSON(dvs))
		}
		// back to stock
		apiPost(t, "/v1/spaces/"+teamId+"/modify", map[string]any{"objectId": dvHost, "dataset": "views", "records": []map[string]any{
			{"id": "all", "ops": []map[string]any{{"type": "$unset", "path": "query.anyUi"}}}}}, nil)
		lowerSetupVersion(t, homeA, teamId, dvHost)
		runCLI(t, runOpts{sid: sidA}, "collab", "list", "--space", teamName)
	}

	// a doc filed in the collab space by name
	docBody := "# E2E Shared Doc\n\nShared with the team by the e2e suite.\n"
	out, errOut = runCLI(t, runOpts{sid: sidA, stdin: docBody}, "doc", "add", "-", "--space", teamName, "--kind", "spec")
	sharedDoc := linkId(t, out)
	mustContain(t, out, "any://o/"+teamId+"/"+sharedDoc, "doc add --space prints a link into the collab space")
	mustNotLogErrors(t, errOut, "doc add --space")
	row := apiObject(t, sharedDoc, teamId)
	d := ts.owner("doc")
	mustEqual(t, typeOf(row), "page", "shared doc type")
	mustBeFiled(t, row, d, "shared doc")
	mustEqual(t, choiceOf(row, d, ts.prop("doc", "kind")), "spec", "shared doc kind")
	mustEqualSlice(t, arrOf(row, d, ts.prop("doc", "author")), []string{"any://u/" + identity}, "shared doc author")
	if o := arrOf(row, d, ts.prop("doc", "originSession")); len(o) != 0 {
		t.Errorf("shared doc relates to the private session %v (relations never cross spaces)", o)
	}
	teamProject := firstLink(row, d, ts.prop("doc", "project"))
	if teamProject == "" {
		t.Fatal("shared doc has no project relation")
	}
	tp := apiObject(t, teamProject, teamId)
	mustEqual(t, nameOf(tp), scope, "collab project page name")
	mustBeFiled(t, tp, ts.owner("project"), "collab project page")
	mustEqual(t, strOf(tp, ts.owner("project"), ts.prop("project", "key")), scope, "collab project key")
	if teamProject == projectObj {
		t.Errorf("the collab space reuses the private project page id %s", projectObj)
	}
	// every machine and member converges on the one page per scope
	mustEqual(t, bundleChild(t, teamId, "agents/v1", "project:"+scope, ts.owner("project")), teamProject,
		"collab project page = deterministic child project:<scope>")

	// a link names its space: show needs no --space
	out, _ = runCLI(t, runOpts{sid: sidA}, "doc", "show", "any://o/"+teamId+"/"+sharedDoc)
	mustContain(t, out, "# E2E Shared Doc", "doc show <collab link>")
	mustContain(t, out, "Shared with the team", "doc show <collab link> body")
	out, _ = runCLI(t, runOpts{sid: sidA}, "doc", "list", "--space", teamName)
	mustContain(t, out, "E2E Shared Doc", "doc list --space")
	out, _ = runCLI(t, runOpts{sid: sidA}, "doc", "list")
	mustContain(t, out, "E2E Shared Doc", "doc list without --space spans the collab space")
	mustContain(t, out, "("+teamName+")", "doc list names the collab space")

	// a shared task, assigned by member name when the profile has one
	assignee := identity
	if name := memberName(t, teamId, identity); name != "" {
		assignee = name
	}
	out, _ = runCLI(t, runOpts{sid: sidA, stdin: "# E2E Shared Task\n\nA task for the team.\n"},
		"task", "add", "-", "--space", teamId, "--assignee", assignee)
	sharedTask := linkId(t, out)
	taskLink := "any://o/" + teamId + "/" + sharedTask
	row = apiObject(t, sharedTask, teamId)
	tk := ts.owner("task")
	mustBeFiled(t, row, tk, "shared task")
	mustEqualSlice(t, arrOf(row, tk, ts.prop("task", "assignee")), []string{"any://u/" + identity},
		"shared task assignee (--assignee "+assignee+")")
	mustEqual(t, firstLink(row, d, ts.prop("doc", "project")), teamProject, "shared task project relation (converged)")
	if n := len(apiQueryObjectsIn(t, teamId, collectionFilter(ts.owner("project"), map[string]any{"any.name": scope}))); n != 1 {
		t.Errorf("want one project page named %s in the collab space, got %d", scope, n)
	}
	out, _ = runCLI(t, runOpts{sid: sidA}, "task", "list", "--space", teamName, "--mine")
	mustContain(t, out, "E2E Shared Task", "task list --space --mine")
	out, _ = runCLI(t, runOpts{sid: sidA}, "task", "list")
	mustContain(t, out, "E2E Shared Task", "task list without --space spans the collab space")
	mustContain(t, out, "("+teamName+")", "task list names the collab space")
	out, _ = runCLI(t, runOpts{sid: sidA}, "task", "show", sharedTask)
	mustContain(t, out, "E2E Shared Task", "task show <bare id> finds the collab space's task")
	runCLI(t, runOpts{sid: sidA}, "task", "start", taskLink)
	row = apiObject(t, sharedTask, teamId)
	mustEqual(t, choiceOf(row, tk, ts.prop("task", "status")), "in_progress", "shared task status after start")
	for _, x := range []string{"handledBy", "handledIn"} {
		if v := arrOf(row, tk, ts.prop("task", x)); len(v) != 0 {
			t.Errorf("shared task %s = %v: machine-bound properties stay private", x, v)
		}
	}
	runCLI(t, runOpts{sid: sidA}, "task", "done", taskLink)
	mustEqual(t, choiceOf(apiObject(t, sharedTask, teamId), tk, ts.prop("task", "status")), "done", "shared task status after done")

	out, _ = runCLI(t, runOpts{sid: sidA}, "project", "list", "--space", teamName)
	mustContain(t, out, "any://o/"+teamId+"/"+teamProject, "project list --space")
	mustContain(t, out, scope, "project list --space")

	// session share copies the private page into the collab space
	_, errOut = runCLI(t, runOpts{sid: sidA, wantFail: true}, "session", "share")
	mustContain(t, errOut, "needs --space", "session share without --space")
	_, errOut = runCLI(t, runOpts{sid: sidA, wantFail: true}, "session", "share", "--space", spaceName)
	mustContain(t, errOut, "is the private space", "session share into the private space")

	ps := st.priv()
	out, _ = runCLI(t, runOpts{sid: sidA}, "session", "share", "--space", teamName, "--transcript")
	copyId := linkId(t, out)
	mustContain(t, out, "any://o/"+teamId+"/"+copyId, "session share prints the copy's link")
	src := apiObject(t, sessionObjA)
	cp := apiObject(t, copyId, teamId)
	s := ts.owner("session")
	mustEqual(t, typeOf(cp), "page", "shared session type")
	mustBeFiled(t, cp, s, "shared session")
	mustEqual(t, nameOf(cp), nameOf(src), "shared session name")
	mustEqual(t, strOf(cp, s, ts.prop("session", "summary")), "E2E session summary", "shared session summary")
	mustEqual(t, choiceOf(cp, s, ts.prop("session", "tool")), "claude", "shared session tool")
	mustEqual(t, choiceOf(cp, s, ts.prop("session", "status")), "ended", "shared session status")
	mustEqual(t, strOf(cp, s, ts.prop("session", "host")), hostName, "shared session host")
	mustEqual(t, strOf(cp, s, ts.prop("session", "sessionId")), sidA, "shared session id")
	mustEqualSlice(t, arrOf(cp, s, ts.prop("session", "owner")), []string{"any://u/" + identity}, "shared session owner")
	mustEqual(t, firstLink(cp, s, ts.prop("session", "project")), teamProject, "shared session project (re-resolved)")
	mustEqual(t, strOf(cp, s, ts.prop("session", "sharedFrom")), "any://o/"+spaceId+"/"+sessionObjA, "shared session sharedFrom")
	mustEqual(t, strOf(src, ps.owner("session"), ps.prop("session", "sharedTo")), "any://o/"+teamId+"/"+copyId, "private session sharedTo")
	if v := arrOf(cp, s, ts.prop("session", "machine")); len(v) != 0 {
		t.Errorf("shared session relates to a machine %v (machines stay private)", v)
	}
	// the session's issues re-resolve to the collab space's issue pages
	got := linksOf(cp, s, ts.prop("session", "linear"))
	for _, key := range []string{"SYN-1", "SYN-2"} {
		if !contains(got, linearIssueIdIn(t, ts, key)) {
			t.Errorf("shared session linear = %v, want the collab space's %s page among them", got, key)
		}
	}
	mustEqual(t, len(got), 2, "shared session linear count")
	mustContain(t, apiMarkdownIn(t, teamId, copyId), "A note appended by the e2e suite", "shared session body")
	tlink := strOf(cp, s, ts.prop("session", "transcript"))
	tprefix := "any://f/" + teamId + "/"
	if !strings.HasPrefix(tlink, tprefix) || len(tlink) == len(tprefix) {
		t.Errorf("shared transcript link = %q, want %s<fileId>", tlink, tprefix)
	} else {
		mustContain(t, gunzip(t, apiFileIn(t, teamId, strings.TrimPrefix(tlink, tprefix))), "e2e hello", "shared transcript bytes")
	}
	mustOnlyDeclaredProps(t, ts, "session", cp)

	_, errOut = runCLI(t, runOpts{sid: sidA, wantFail: true}, "session", "share", "--space", teamName)
	mustContain(t, errOut, "already shared", "a second session share")
	out, _ = runCLI(t, runOpts{sid: sidA}, "session", "list", "--space", teamName)
	mustContain(t, out, "E2E session summary", "session list --space")

	// collabs: requests between members' agents (the chat itself is not
	// exercised here)
	reqPath := filepath.Join(tmpRoot, "request.md")
	writeFile(t, reqPath, "# E2E Help Request\n\nPlease look at the e2e suite.\nhttps://github.com/anyproto/any-agents/pull/1\n\n## Done when\n\nThe suite passes twice.\n")
	if !ts.collabOn() {
		_, errOut = runCLI(t, runOpts{sid: sidA, wantFail: true}, "collab", "open", reqPath, "--space", teamName)
		mustContain(t, errOut, "collabs are off in "+teamName, "collab open without the Collab bundle")
		_, errOut = runCLI(t, runOpts{sid: sidA, wantFail: true}, "collab", "list", "--space", teamName)
		mustContain(t, errOut, "collabs are off in "+teamName, "collab list without the Collab bundle")
	} else {
		noDod := filepath.Join(tmpRoot, "request-nodod.md")
		writeFile(t, noDod, "# No DoD\n\nJust a question.\n")
		_, errOut = runCLI(t, runOpts{sid: sidA, wantFail: true}, "collab", "open", noDod, "--space", teamName)
		mustContain(t, errOut, "definition of done", "collab open without a definition of done")
		out, _ = runCLI(t, runOpts{sid: sidA}, "collab", "open", reqPath, "--space", teamName, "--announce")
		collabId := linkId(t, out)
		collabLink := "any://o/" + teamId + "/" + collabId
		mustContain(t, out, collabLink, "collab open prints the collab's link")

		// --announce posts into the general chat with the page attached
		var gen map[string]any
		apiPost(t, "/v1/catalog/general-chat/setup", map[string]string{"spaceId": teamId}, &gen)
		bundles, _ := gen["bundles"].([]any)
		if len(bundles) == 0 {
			t.Fatalf("no general chat in the team space: %s", mustJSON(gen))
		}
		generalChat, _ := bundles[0].(map[string]any)["bundle"].(map[string]any)["rootId"].(string)
		mustAnnounced(t, teamId, generalChat, "Help wanted: E2E Help Request", collabLink)
		out, _ = runCLI(t, runOpts{sid: sidA, stdin: "Team notes.\n"}, "release", "add", "-", "--project", "any", "--version", "v9.9.8", "--space", teamName, "--announce")
		mustAnnounced(t, teamId, generalChat, "any v9.9.8 is out", "any://o/"+teamId+"/"+linkId(t, out))
		row = apiObject(t, collabId, teamId)
		c := ts.owner("collab")
		mustEqual(t, typeOf(row), c, "collab object type")
		mustEqual(t, nameOf(row), "E2E Help Request", "collab name")
		mustEqual(t, choiceOf(row, c, ts.prop("collab", "status")), "request", "collab status after open")
		mustEqualSlice(t, arrOf(row, c, ts.prop("collab", "requester")), []string{"any://u/" + identity}, "collab requester")
		mustEqual(t, firstLink(row, c, ts.prop("collab", "project")), teamProject, "collab project")
		mustEqual(t, strOf(row, c, ts.prop("collab", "originSession")), "any://o/"+spaceId+"/"+sessionObjA, "collab originSession")
		mustEqual(t, strOf(row, c, ts.prop("collab", "pr")), "https://github.com/anyproto/any-agents/pull/1", "collab pr from the body")
		mustEqual(t, strOf(row, c, ts.prop("collab", "doneWhen")), "The suite passes twice.", "collab doneWhen from the body")
		mustContain(t, apiMarkdownIn(t, teamId, collabId), "Please look at the e2e suite", "collab body")
		mustContain(t, apiMarkdown(t, sessionObjA), collabLink, "the private session notes the opened collab")
		out, _ = runCLI(t, runOpts{sid: sidA}, "collab", "list", "--space", teamName, "--mine")
		mustContain(t, out, "E2E Help Request", "collab list --mine")
		runCLI(t, runOpts{sid: sidA}, "collab", "join", collabLink)
		row = apiObject(t, collabId, teamId)
		mustEqual(t, choiceOf(row, c, ts.prop("collab", "status")), "in_progress", "collab status after join")
		mustEqualSlice(t, arrOf(row, c, ts.prop("collab", "assignees")), []string{"any://u/" + identity}, "collab assignees after join")
		_, errOut = runCLI(t, runOpts{sid: sidA, wantFail: true}, "collab", "done", collabLink)
		mustContain(t, errOut, "--met", "collab done without --met")
		runCLI(t, runOpts{sid: sidA}, "collab", "done", collabLink, "--met", "the suite passed twice")
		out, _ = runCLI(t, runOpts{sid: sidA}, "collab", "show", collabLink)
		mustContain(t, out, "Done: the suite passed twice", "collab done posts the evidence to the chat")
		runCLI(t, runOpts{sid: sidA}, "collab", "reopen", collabLink, "--met", "closed too early")
		row = apiObject(t, collabId, teamId)
		mustEqual(t, choiceOf(row, c, ts.prop("collab", "status")), "in_progress", "collab status after reopen")
		if group(row, c)[ts.prop("collab", "resolvedAt")] != nil {
			t.Error("collab reopen kept resolvedAt")
		}
		runCLI(t, runOpts{sid: sidA}, "collab", "done", collabLink, "--met", "the suite passed twice")
		out, _ = runCLI(t, runOpts{sid: sidA}, "collab", "wait", collabLink, "--timeout", "5s")
		mustContain(t, out, "nothing more to wait for", "collab wait returns once the collab is closed")
		_, errOut = runCLI(t, runOpts{sid: sidA, wantFail: true}, "collab", "done", collabLink, "--met", "again")
		mustContain(t, errOut, "already done", "collab done on a closed collab")
		row = apiObject(t, collabId, teamId)
		mustEqual(t, choiceOf(row, c, ts.prop("collab", "status")), "done", "collab status after done")
		if group(row, c)[ts.prop("collab", "resolvedAt")] == nil {
			t.Error("collab done did not set resolvedAt")
		}
		out, _ = runCLI(t, runOpts{sid: sidA}, "collab", "list", "--space", teamName)
		if strings.Contains(out, "E2E Help Request") {
			t.Errorf("a done collab still shows in `collab list`:\n%s", out)
		}
	}

	// the session-start hook re-checks the collab space and stays quiet
	ctx := sessionStartHook(t, sidB, "resume")
	mustContain(t, ctx, "shared context is on", "session-start context with a collab space")

	// remove forgets the space on this machine; add adopts it again
	_, errOut = runCLI(t, runOpts{sid: sidA, wantFail: true}, "space", "remove", spaceName)
	mustContain(t, errOut, "cannot be removed", "space remove of the private space")
	out, _ = runCLI(t, runOpts{sid: sidA}, "space", "remove", teamName)
	mustContain(t, out, "forgotten", "space remove output")
	if loadStateT(t, homeA).Spaces[teamId] != nil {
		t.Error("state.json keeps the removed collab space")
	}
	if info, err := getSpace(teamId); err != nil || info.Status == "deleted" {
		t.Errorf("space remove touched the space itself (status %v, err %v)", info.Status, err)
	}
	_, errOut = runCLI(t, runOpts{sid: sidA, wantFail: true}, "doc", "show", "any://o/"+teamId+"/"+sharedDoc)
	mustContain(t, errOut, "is not set up here", "doc show <link> into a removed space")

	out, _ = runCLI(t, runOpts{sid: sidA}, "space", "add", teamName)
	mustContain(t, out, teamId+"  "+teamName+" set up as a collab space", "space add output")
	readded := loadStateT(t, homeA).Spaces[teamId]
	if readded == nil {
		t.Fatal("state.json has no collab space after space add")
	}
	for name := range ts.Bundles {
		mustEqual(t, readded.owner(name), ts.owner(name), "bundle "+name+" root after space add (adopted, not reinstalled)")
	}
}

// --- 8b. a second member ---

// testCollabMember joins a second account (ANY_AGENTS_ADDR_B) to the
// collab space as a writer: install on its own machine, invite, join,
// accept, then `space add` adopts the bundles the owner installed.
func testCollabMember(t *testing.T) {
	if addrB == "" {
		t.Skip("ANY_AGENTS_ADDR_B unset: no second member")
	}
	if teamId == "" {
		t.Fatal("no collab space: collab_space_create_share_and_remove failed")
	}
	runCLI(t, runOpts{member: true}, "install", "--create-space")
	sm := loadStateT(t, homeM)
	if sm.Private != "" {
		trackSpaceAt(addrB, sm.Private)
	}
	memberIdentity = sm.Identity
	if memberIdentity == "" || memberIdentity == identity {
		t.Fatalf("member identity %q: want a second account (the owner is %s)", memberIdentity, identity)
	}

	var inv struct {
		InviteToken string `json:"inviteToken"`
	}
	apiPost(t, "/v1/spaces/"+teamId+"/invites", nil, &inv)
	apiDoAt(t, addrB, "POST", "/v1/spaces/join", map[string]any{"inviteToken": inv.InviteToken}, nil)
	var requestId string
	waitFor(t, 60*time.Second, "the member's join request", func() bool {
		var out struct {
			Requests []struct{ RecordId, Identity string } `json:"requests"`
		}
		apiGet(t, "/v1/spaces/"+teamId+"/members/requests", &out)
		for _, r := range out.Requests {
			if r.Identity == memberIdentity {
				requestId = r.RecordId
				return true
			}
		}
		return false
	})
	apiPost(t, "/v1/spaces/"+teamId+"/acl/accept", map[string]any{"requestRecordId": requestId, "permission": "writer"}, nil)
	waitFor(t, 60*time.Second, "the member's copy of the collab space to turn active", func() bool {
		info, err := getSpaceAt(addrB, teamId)
		return err == nil && info.Status == "active" && info.OwnRole == "writer"
	})

	// right after the join the bundles' roots are still syncing: space add
	// answers bundle.not_ready until they arrive
	var out string
	waitFor(t, 60*time.Second, "the member's server to sync the collab space's bundles", func() bool {
		stdout, stderr, err := execCLI(runOpts{member: true}, "space", "add", teamId)
		if err != nil && !strings.Contains(stderr, "bundle.not_ready") {
			t.Fatalf("member space add: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
		}
		out = stdout
		return err == nil
	})
	mustContain(t, out, teamId+"  "+teamName+" set up as a collab space", "member space add")
	ts, tm := loadStateT(t, homeA).Spaces[teamId], loadStateT(t, homeM).Spaces[teamId]
	if ts == nil || tm == nil {
		t.Fatalf("collab space %s missing from a state.json (owner %v, member %v)", teamId, ts != nil, tm != nil)
	}
	mustEqual(t, tm.Kind, "collab", "member's collab space kind")

	// a close is a vote: a helper's `done` records its agreement, the
	// collab closes once the requester agreed too; a user's decision
	// closes it alone
	reqM := filepath.Join(tmpRoot, "request-member.md")
	writeFile(t, reqM, "# Member request\n\nFrom the second member.\n\n## Done when\n\nThe owner answers.\n")
	out, _ = runCLI(t, runOpts{member: true}, "collab", "open", reqM, "--space", teamName, "--to", identity)
	memberCollabId := linkId(t, out)
	memberCollab := "any://o/" + teamId + "/" + memberCollabId
	// the index row arrives before the object's tree: wait for a read
	waitFor(t, 60*time.Second, "the member's collab to sync to the owner", func() bool {
		_, _, err := execCLI(runOpts{sid: sidA}, "collab", "show", memberCollab)
		return err == nil
	})
	out, _ = runCLI(t, runOpts{sid: sidA}, "collab", "done", memberCollab, "--met", "answered in the chat")
	mustContain(t, out, "agreed; closes once", "collab done by a helper records an agreement")
	cc := ts.owner("collab")
	row := apiObject(t, memberCollabId, teamId)
	mustEqual(t, choiceOf(row, cc, ts.prop("collab", "status")), "request", "collab status with one of two agreements")
	mustEqualSlice(t, arrOf(row, cc, ts.prop("collab", "agreed")), []string{"any://u/" + identity}, "collab agreed after the helper's done")
	out, _ = runCLI(t, runOpts{sid: sidA}, "collab", "show", memberCollab)
	mustContain(t, out, "Done: answered in the chat", "the helper's evidence is in the chat")
	mustContain(t, out, "Closes once", "the chat names who still has to agree")
	mustContain(t, out, "agreed to close:", "collab show lists the agreements")

	// a reply from the other side holds the owner's agent once at Stop and
	// is named on the next prompt, until the collab is read
	runCLI(t, runOpts{member: true}, "collab", "say", memberCollab, "ping from the member")
	waitFor(t, 60*time.Second, "the member's reply to sync to the owner", func() bool {
		return len(apiRecordsIn(t, teamId, memberCollabId, "chat_messages", nil)) > 1
	})
	mustEqual(t, stopHook(t, sidA, true), "", "stop hook while already active")
	mustContain(t, stopHook(t, sidA, false), "Member request", "stop hook blocks on an unread reply")
	mustContain(t, promptHook(t, sidA, "what should I look at next in this repository today"), "unread replies", "prompt hook names the unread reply")
	runCLI(t, runOpts{sid: sidA}, "collab", "show", memberCollab)
	mustEqual(t, stopHook(t, sidA, false), "", "stop hook after the reply was read")

	// two waiters of the same member both return on a new message
	type waitRes struct {
		out string
		err error
	}
	ch := make(chan waitRes, 2)
	for _, sid := range []string{sidA, "e2e-sibling-session"} {
		go func(sid string) {
			out, _, err := execCLI(runOpts{sid: sid}, "collab", "wait", memberCollab, "--timeout", "60s")
			ch <- waitRes{out, err}
		}(sid)
	}
	time.Sleep(3 * time.Second)
	runCLI(t, runOpts{member: true}, "collab", "say", memberCollab, "second ping for two waiters")
	for i := 0; i < 2; i++ {
		select {
		case r := <-ch:
			if r.err != nil || !strings.Contains(r.out, "second ping for two waiters") {
				t.Errorf("waiter %d: err=%v out=%q", i, r.err, r.out)
			}
		case <-time.After(70 * time.Second):
			t.Fatal("a waiter did not return on the member's message")
		}
	}
	// an edited message wakes a waiter and is marked
	var editId string
	for _, m := range apiRecordsAt(t, addrB, teamId, memberCollabId, "chat_messages", nil) {
		if txt, _ := m["text"].(string); txt == "second ping for two waiters" {
			editId, _ = m["id"].(string)
		}
	}
	if editId == "" {
		t.Fatal("the member's message is missing on the member's server")
	}
	edited := make(chan waitRes, 1)
	go func() {
		out, _, err := execCLI(runOpts{sid: sidA}, "collab", "wait", memberCollab, "--timeout", "60s")
		edited <- waitRes{out, err}
	}()
	time.Sleep(3 * time.Second)
	apiDoAt(t, addrB, "PATCH", "/v1/spaces/"+teamId+"/objects/"+memberCollabId+"/chat/messages/"+editId, map[string]any{"text": "second ping, edited"}, nil)
	select {
	case r := <-edited:
		if r.err != nil || !strings.Contains(r.out, "second ping, edited") || !strings.Contains(r.out, ", edited]") {
			t.Errorf("wait after an edit: err=%v out=%q", r.err, r.out)
		}
	case <-time.After(70 * time.Second):
		t.Fatal("collab wait did not wake on an edited message")
	}
	// another session of the same member sees who holds the collab
	out, _ = runCLI(t, runOpts{sid: "e2e-other-session"}, "collab", "list", "--space", teamName)
	mustContain(t, out, "(another session of yours)", "collab list marks a sibling session's collab")

	// the requester's agreement completes the close
	waitFor(t, 60*time.Second, "the owner's agreement to sync to the member", func() bool {
		out, _, err := execCLI(runOpts{member: true}, "collab", "show", memberCollab)
		return err == nil && strings.Contains(out, "agreed to close:")
	})
	runCLI(t, runOpts{member: true}, "collab", "done", memberCollab, "--met", "the owner answered")
	waitFor(t, 60*time.Second, "the close to sync to the owner", func() bool {
		row = apiObject(t, memberCollabId, teamId)
		return choiceOf(row, cc, ts.prop("collab", "status")) == "done"
	})
	if got := arrOf(row, cc, ts.prop("collab", "agreed")); len(got) != 2 {
		t.Errorf("collab agreed after both agreed = %v, want both members", got)
	}

	// reopen drops the agreements; a user's decision closes alone
	runCLI(t, runOpts{member: true}, "collab", "reopen", memberCollab, "--met", "one more thing")
	waitFor(t, 60*time.Second, "the reopen to sync to the owner", func() bool {
		row = apiObject(t, memberCollabId, teamId)
		return choiceOf(row, cc, ts.prop("collab", "status")) == "in_progress"
	})
	if got := arrOf(row, cc, ts.prop("collab", "agreed")); len(got) != 0 {
		t.Errorf("collab reopen kept the agreements: %v", got)
	}
	_, errOut := runCLI(t, runOpts{sid: sidA, wantFail: true}, "collab", "cancel", memberCollab)
	mustContain(t, errOut, "--user", "collab cancel without a reason names the user's decision as the other way")
	runCLI(t, runOpts{sid: sidA}, "collab", "cancel", memberCollab, "--user", "e2e over")
	out, _ = runCLI(t, runOpts{sid: sidA}, "collab", "show", memberCollab)
	mustContain(t, out, "Canceled on the user's decision: e2e over", "collab cancel --user posts the decision")
	mustEqual(t, choiceOf(apiObject(t, memberCollabId, teamId), cc, ts.prop("collab", "status")), "canceled", "collab status after cancel --user by a helper")
	for name := range ts.Bundles {
		mustEqual(t, tm.owner(name), ts.owner(name), "member's bundle "+name+" root (adopted, not reinstalled)")
	}
	mustEqual(t, tm.coll("memories"), ts.coll("memories"), "member's memories dataset")
	memberReady = !t.Failed()
}

// --- 8c. shared memories ---

// testSharedMemory covers shared project memories from the publisher's
// side (share --move, edits through push and the post-tool hook, a file
// written into the mirror, unshare, the private copy winning, a failing
// batch) and, with a second member, the read-only side.
func testSharedMemory(t *testing.T) {
	if teamId == "" {
		t.Fatal("no collab space: collab_space_create_share_and_remove failed")
	}
	st := loadStateT(t, homeA)
	ps, ts := st.priv(), st.Spaces[teamId]
	if ts == nil {
		t.Fatalf("state.json has no collab space %s", teamId)
	}
	if !memberReady {
		t.Log("no second member: the read-only side is not exercised")
	}
	const (
		gamma, delta, eps, zeta, eta = "e2e-shared-gamma", "e2e-shared-delta", "e2e-shared-epsilon", "e2e-shared-zeta", "e2e-shared-eta"
		gammaDesc, deltaDesc         = "Gamma memory moved to the team", "Delta memory moved to the team"
	)
	recId := func(slug string) string { return scope + ":" + slug }
	memDir := memoryDir(homeA, projDir)
	mirror := filepath.Join(memDir, "shared", sharedDir(teamName, teamId))
	index := func() string { t.Helper(); return readFile(t, filepath.Join(memDir, "MEMORY.md")) }
	teamBody := func(slug string) string {
		t.Helper()
		return fmt.Sprint(teamMemRecords(t, addr, ts)[recId(slug)]["body"])
	}
	gammaPath, deltaPath := filepath.Join(mirror, gamma+".md"), filepath.Join(mirror, delta+".md")
	gammaFile := func(body string) string { return sharedMemFile(gamma, gammaDesc, "project", teamName, body) }

	// two private memories, then one share --move call for both
	writeFile(t, filepath.Join(memDir, gamma+".md"), memFile(gamma, gammaDesc, "project", "Gamma body, written privately."))
	writeFile(t, filepath.Join(memDir, delta+".md"), memFile(delta, deltaDesc, "reference", "Delta body, written privately."))
	runCLI(t, runOpts{}, "push")
	recs := memRecords(t, ps)
	for _, slug := range []string{gamma, delta} {
		if r := recs[recId(slug)]; r == nil || fmt.Sprint(r["kind"]) == "retracted" {
			t.Fatalf("no live private record %s after push (have %v)", recId(slug), keys(recs))
		}
	}
	out, errOut := runCLI(t, runOpts{}, "memory", "share", gamma, delta, "--space", teamName, "--move")
	mustNotLogErrors(t, errOut, "memory share --move")
	team := teamMemRecords(t, addr, ts)
	recs = memRecords(t, ps)
	idx := index()
	for _, slug := range []string{gamma, delta} {
		mustContain(t, out, slug+" moved to "+teamName+" as "+recId(slug), "memory share --move output")
		if r := team[recId(slug)]; r == nil {
			t.Errorf("no team record %s after share --move (have %v)", recId(slug), keys(team))
		} else {
			mustEqual(t, fmt.Sprint(r["author"]), identity, slug+" team record author")
			mustContain(t, fmt.Sprint(r["body"]), "written privately", slug+" team record body")
		}
		mustEqual(t, fmt.Sprint(recs[recId(slug)]["kind"]), "retracted", slug+" private record kind after --move")
		mustNotExist(t, filepath.Join(memDir, slug+".md"), "the private file of "+slug+" after --move")
		p := filepath.Join(mirror, slug+".md")
		mustBeWritable(t, p, true, slug+" mirror file after --move")
		mustContain(t, readFile(t, p), "\n  shared: "+teamName+"\n", slug+" mirror file frontmatter")
		mustListShared(t, idx, slug)
	}
	mustEqual(t, fmt.Sprint(team[recId(delta)]["kind"]), "reference", "delta team record kind")

	// a pull brings no private copy back
	out, errOut = runCLI(t, runOpts{}, "pull")
	mustNotLogErrors(t, errOut, "pull after share --move")
	mustContain(t, out, "1 shared set(s)", "pull output after share --move")
	for _, slug := range []string{gamma, delta} {
		mustNotExist(t, filepath.Join(memDir, slug+".md"), "the private file of "+slug+" after a pull")
		mustBeWritable(t, filepath.Join(mirror, slug+".md"), true, slug+" mirror file after a pull")
	}

	// the publisher's mirror file syncs like a private one: push publishes
	// an edit and the next pull keeps it
	writeFile(t, gammaPath, gammaFile("Gamma body, edited in the mirror."))
	runCLI(t, runOpts{}, "push")
	mustContain(t, teamBody(gamma), "edited in the mirror", "gamma team body after push")
	_, errOut = runCLI(t, runOpts{}, "pull")
	mustNotLogErrors(t, errOut, "pull after the mirror edit")
	mustEqual(t, readFile(t, gammaPath), gammaFile("Gamma body, edited in the mirror."), "gamma mirror file after pull")
	mustNotExist(t, gammaPath+".conflict", "a .conflict copy after pushing a mirror edit")

	// the post-tool hook pushes a file-tool edit and a shell edit
	deltaFile := sharedMemFile(delta, deltaDesc, "reference", teamName, "Delta body, edited through the post-tool hook.")
	writeFile(t, deltaPath, deltaFile)
	mustNotLogErrors(t, postToolHook(t, runOpts{}, deltaPath, ""), "post-tool hook on an own shared file")
	mustContain(t, teamBody(delta), "edited through the post-tool hook", "delta team body after the post-tool hook")
	writeFile(t, gammaPath, gammaFile("Gamma body, edited by a shell command."))
	mustNotLogErrors(t, postToolHook(t, runOpts{}, "", "sed -i 's/a/a/' "+gammaPath), "post-tool hook on a shell edit")
	mustContain(t, teamBody(gamma), "edited by a shell command", "gamma team body after the shell-edit hook")
	_, errOut = runCLI(t, runOpts{}, "pull")
	mustNotLogErrors(t, errOut, "pull after the hook pushes")
	mustEqual(t, readFile(t, gammaPath), gammaFile("Gamma body, edited by a shell command."), "gamma mirror file after the hook push")
	mustEqual(t, readFile(t, deltaPath), deltaFile, "delta mirror file after the hook push")
	for _, p := range []string{gammaPath, deltaPath} {
		mustNotExist(t, p+".conflict", "a .conflict copy after the hook pushes")
	}

	// the member's copy is read-only and the space always wins there
	memDirM := memoryDir(homeM, projDir)
	mirrorM := filepath.Join(memDirM, "shared", sharedDir(teamName, teamId))
	gammaM, deltaM := filepath.Join(mirrorM, gamma+".md"), filepath.Join(mirrorM, delta+".md")
	var tm *spaceState
	if memberReady {
		tm = loadStateT(t, homeM).Spaces[teamId]
		waitMemberRecord(t, tm, recId(gamma), "the shell edit of gamma", bodyHas("edited by a shell command"))
		waitMemberRecord(t, tm, recId(delta), "the hook edit of delta", bodyHas("edited through the post-tool hook"))
		out, errOut = runCLI(t, runOpts{member: true}, "pull")
		mustNotLogErrors(t, errOut, "member pull")
		mustContain(t, out, "1 shared set(s)", "member pull output")
		mustEqual(t, readFile(t, gammaM), gammaFile("Gamma body, edited by a shell command."), "member's gamma file")
		mustEqual(t, readFile(t, deltaM), deltaFile, "member's delta file")
		mustBeWritable(t, gammaM, false, "member's gamma file")
		mustListShared(t, readFile(t, filepath.Join(memDirM, "MEMORY.md")), gamma)

		// an edit of another member's file is never pushed
		forceWrite(t, gammaM, gammaFile("Gamma body, tampered by the member."), 0o444)
		errOut = postToolHook(t, runOpts{member: true}, gammaM, "")
		mustContain(t, errOut, "is a read-only mirror of another member's memory", "member post-tool hook on the owner's file")
		mustContain(t, fmt.Sprint(teamMemRecords(t, addrB, tm)[recId(gamma)]["body"]), "edited by a shell command",
			"gamma team body after the member's hook")
		_, errOut = runCLI(t, runOpts{member: true}, "pull")
		mustNotLogErrors(t, errOut, "member pull over a tampered file")
		mustEqual(t, readFile(t, gammaM), gammaFile("Gamma body, edited by a shell command."), "member's gamma file after pull")
		mustBeWritable(t, gammaM, false, "member's gamma file rewritten by pull")

		// the publisher's next edit replaces the member's copy, still read-only
		writeFile(t, gammaPath, gammaFile("Gamma body, third revision."))
		runCLI(t, runOpts{}, "push")
		waitMemberRecord(t, tm, recId(gamma), "the third revision of gamma", bodyHas("third revision"))
		runCLI(t, runOpts{member: true}, "pull")
		mustEqual(t, readFile(t, gammaM), gammaFile("Gamma body, third revision."), "member's gamma file after an update")
		mustBeWritable(t, gammaM, false, "member's gamma file after an update")
		_, errOut = runCLI(t, runOpts{member: true}, "push")
		mustNotLogErrors(t, errOut, "member push (other members' files are never pushed)")

		// the member's publish of the same slug is refused, and a refused
		// --move keeps the private file
		privM := filepath.Join(memDirM, gamma+".md")
		writeFile(t, privM, memFile(gamma, "The member's own gamma", "project", "Member body."))
		_, errOut = runCLI(t, runOpts{member: true, wantFail: true}, "memory", "share", gamma, "--space", teamName, "--move")
		mustContain(t, errOut, "any-agents: "+recId(gamma)+": ", "member share of the owner's slug")
		mustContain(t, errOut, "author", "member share rejection reason")
		mustContain(t, errOut, "published by another member", "member share rejection hint")
		if _, err := os.Stat(privM); err != nil {
			t.Errorf("a refused share --move removed the member's private file: %v", err)
		}
		r := teamMemRecords(t, addrB, tm)[recId(gamma)]
		mustEqual(t, fmt.Sprint(r["author"]), identity, "gamma author after the member's refused share")
		mustContain(t, fmt.Sprint(r["body"]), "third revision", "gamma body after the member's refused share")
		if err := os.Remove(privM); err != nil {
			t.Fatal(err)
		}
	}

	// a new file written into the mirror is a share: pull keeps it until
	// push publishes it
	epsPath := filepath.Join(mirror, eps+".md")
	writeFile(t, epsPath, memFile(eps, "Epsilon written straight into the mirror", "project", "Epsilon body, never private."))
	_, errOut = runCLI(t, runOpts{}, "pull")
	mustNotLogErrors(t, errOut, "pull with a new mirror file")
	if _, err := os.Stat(epsPath); err != nil {
		t.Fatalf("pull removed an own mirror file not pushed yet: %v", err)
	}
	runCLI(t, runOpts{}, "push")
	if r := teamMemRecords(t, addr, ts)[recId(eps)]; r == nil {
		t.Errorf("push did not publish the new mirror file as %s", recId(eps))
	} else {
		mustEqual(t, fmt.Sprint(r["author"]), identity, "epsilon team record author")
		mustEqual(t, fmt.Sprint(r["title"]), "Epsilon written straight into the mirror", "epsilon team record title")
		mustContain(t, fmt.Sprint(r["body"]), "never private", "epsilon team record body")
	}
	mustEqual(t, memRecords(t, ps)[recId(eps)] == nil, true, "no private record for a file written into the mirror")
	runCLI(t, runOpts{}, "pull")
	mustBeWritable(t, epsPath, true, "epsilon mirror file after push and pull")
	mustListShared(t, index(), eps)
	if memberReady {
		waitMemberRecord(t, tm, recId(eps), "epsilon", bodyHas("never private"))
		runCLI(t, runOpts{member: true}, "pull")
		epsM := filepath.Join(mirrorM, eps+".md")
		mustContain(t, readFile(t, epsM), "Epsilon body, never private.", "member's epsilon file")
		mustBeWritable(t, epsM, false, "member's epsilon file")
	}

	// unshare moves the memory back to the private space by default
	out, _ = runCLI(t, runOpts{}, "memory", "unshare", eps, "--space", teamName)
	mustContain(t, out, recId(eps)+" moved back to the private space", "memory unshare output (move back)")
	epsPriv := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(epsPath))), eps+".md")
	mustContain(t, readFile(t, epsPriv), "Epsilon body, never private.", "epsilon restored as a private file")
	mustEqual(t, memRecords(t, ps)[recId(eps)] != nil, true, "epsilon has a private record after unshare")
	mustEqual(t, fmt.Sprint(teamMemRecords(t, addr, ts)[recId(eps)]["kind"]), "retracted", "epsilon team record kind after unshare")
	mustNotExist(t, epsPath, "epsilon mirror file after unshare")
	// drop the restored copy again so the counts later in the suite hold
	if err := os.Remove(epsPriv); err != nil {
		t.Fatal(err)
	}
	runCLI(t, runOpts{}, "push")
	if r := memRecords(t, ps)[recId(eps)]; r != nil && fmt.Sprint(r["kind"]) != "retracted" {
		t.Errorf("epsilon private record still live after its file was removed: kind %v", r["kind"])
	}

	// unshare --delete retracts without a private copy; every member's
	// pull drops the file
	out, _ = runCLI(t, runOpts{}, "memory", "unshare", delta, "--space", teamName, "--delete")
	mustContain(t, out, recId(delta)+" retracted from "+teamName, "memory unshare output")
	mustEqual(t, fmt.Sprint(teamMemRecords(t, addr, ts)[recId(delta)]["kind"]), "retracted", "delta team record kind after unshare")
	_, errOut = runCLI(t, runOpts{}, "pull")
	mustNotLogErrors(t, errOut, "pull after unshare")
	mustNotExist(t, deltaPath, "the owner's delta file after unshare and pull")
	if strings.Contains(index(), "["+delta+"]") {
		t.Errorf("MEMORY.md still lists the unshared %s:\n%s", delta, index())
	}
	if memberReady {
		waitMemberRecord(t, tm, recId(delta), "the retraction of delta", func(r map[string]any) bool {
			return fmt.Sprint(r["kind"]) == "retracted"
		})
		runCLI(t, runOpts{member: true}, "pull")
		mustNotExist(t, deltaM, "the member's delta file after unshare and pull")
		if idxM := readFile(t, filepath.Join(memDirM, "MEMORY.md")); strings.Contains(idxM, "["+delta+"]") {
			t.Errorf("the member's MEMORY.md still lists the unshared %s:\n%s", delta, idxM)
		}
	}

	// share without --move: the private copy wins and the mirror skips it
	zetaPriv := filepath.Join(memDir, zeta+".md")
	writeFile(t, zetaPriv, memFile(zeta, "Zeta stays private", "project", "Zeta body, private and shared."))
	runCLI(t, runOpts{}, "push")
	out, _ = runCLI(t, runOpts{}, "memory", "share", zeta, "--space", teamName)
	mustContain(t, out, zeta+" shared to "+teamName+" as "+recId(zeta), "memory share output")
	mustEqual(t, fmt.Sprint(teamMemRecords(t, addr, ts)[recId(zeta)]["author"]), identity, "zeta team record author")
	mustEqual(t, fmt.Sprint(memRecords(t, ps)[recId(zeta)]["kind"]), "project", "zeta private record kind after a share without --move")
	if _, err := os.Stat(zetaPriv); err != nil {
		t.Errorf("share without --move removed the private file: %v", err)
	}
	_, errOut = runCLI(t, runOpts{}, "pull")
	mustContain(t, errOut, "the private copies win", "pull stderr with a memory both private and shared")
	mustNotExist(t, filepath.Join(mirror, zeta+".md"), "a mirror file of the private zeta")
	idx = index()
	if n := strings.Count(idx, "["+zeta+"]"); n != 1 {
		t.Errorf("MEMORY.md lists %s %d times, want once:\n%s", zeta, n, idx)
	}
	mustContain(t, idx, "["+zeta+"]("+zeta+".md)", "MEMORY.md lists zeta as the private file")

	// a batch with an unknown slug fails before anything is shared
	etaPriv := filepath.Join(memDir, eta+".md")
	writeFile(t, etaPriv, memFile(eta, "Eta never leaves", "project", "Eta body."))
	_, errOut = runCLI(t, runOpts{wantFail: true}, "memory", "share", eta, "e2e-no-such-memory", "--space", teamName)
	mustContain(t, errOut, filepath.Join(memDir, "e2e-no-such-memory.md"), "memory share with an unknown slug")
	if r := teamMemRecords(t, addr, ts)[recId(eta)]; r != nil {
		t.Errorf("a failed batch still shared %s: %s", eta, mustJSON(r))
	}
	if err := os.Remove(etaPriv); err != nil {
		t.Fatal(err)
	}

	// leave the project as the later subtests expect it: one live private
	// memory, nothing both private and shared
	runCLI(t, runOpts{}, "memory", "unshare", zeta, "--space", teamName)
	if err := os.Remove(zetaPriv); err != nil {
		t.Fatal(err)
	}
	runCLI(t, runOpts{}, "push")
	mustEqual(t, fmt.Sprint(memRecords(t, ps)[recId(zeta)]["kind"]), "retracted", "zeta private record after the cleanup")
}

// teamMemRecords reads a collab space's memory records for the scope
// through one server (the owner's or the member's).
func teamMemRecords(t *testing.T, server string, sp *spaceState) map[string]map[string]any {
	t.Helper()
	return recordsById(apiRecordsAt(t, server, sp.Id, sp.owner("app"), sp.coll("memories"), map[string]any{"scope": scope}))
}

// waitMemberRecord polls the member's server until a collab memory record
// satisfies cond: the owner's writes reach it through the sync network.
func waitMemberRecord(t *testing.T, tm *spaceState, id, what string, cond func(map[string]any) bool) {
	t.Helper()
	waitFor(t, 60*time.Second, "the member's server to sync "+what, func() bool {
		r := teamMemRecords(t, addrB, tm)[id]
		return r != nil && cond(r)
	})
}

func bodyHas(s string) func(map[string]any) bool {
	return func(r map[string]any) bool { return strings.Contains(fmt.Sprint(r["body"]), s) }
}

// postToolHook pipes a PostToolUse payload (a file-tool edit, or a shell
// command when filePath is empty) and returns the hook's stderr.
func postToolHook(t *testing.T, o runOpts, filePath, command string) string {
	t.Helper()
	in := map[string]any{"session_id": sidA, "cwd": projDir, "tool_name": "Edit",
		"tool_input": map[string]string{"file_path": filePath}}
	if filePath == "" {
		in["tool_name"], in["tool_input"] = "Bash", map[string]string{"command": command}
	}
	o.stdin = mustJSON(in)
	_, errOut := runCLI(t, o, "hook", "--tool", "claude", "post-tool")
	return errOut
}

// mustListShared fails unless MEMORY.md lists a slug exactly once, as an
// entry of the collab space's "Shared by" section.
func mustListShared(t *testing.T, index, slug string) {
	t.Helper()
	if n := strings.Count(index, "["+slug+"]"); n != 1 {
		t.Errorf("MEMORY.md lists %s %d times, want once:\n%s", slug, n, index)
		return
	}
	head := strings.Index(index, "\n## Shared by "+teamName+" ")
	entry := strings.Index(index, "["+slug+"](shared/"+sharedDir(teamName, teamId)+"/"+slug+".md)")
	if head < 0 || entry < head {
		t.Errorf("MEMORY.md does not list %s under \"Shared by %s\":\n%s", slug, teamName, index)
	}
}

// mustBeWritable checks the owner-write bit: the publisher's own mirror
// files are writable, other members' read-only.
func mustBeWritable(t *testing.T, path string, want bool, what string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Errorf("%s: %v", what, err)
		return
	}
	if got := fi.Mode()&0o200 != 0; got != want {
		t.Errorf("%s: mode %v, want writable=%v", what, fi.Mode(), want)
	}
}

func mustNotExist(t *testing.T, path, what string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s: %s exists (stat err %v)", what, path, err)
	}
}

// forceWrite rewrites a (possibly read-only) file and leaves it with mode.
func forceWrite(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, content)
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// --- 9. search ---

func testSearch(t *testing.T) {
	mustHaveSearchIndex(t)
	// The body index is asynchronous: poll until the doc's editor_blocks
	// window shows up (the same hit auto-recall needs).
	var hits []searchHit
	waitFor(t, 30*time.Second, "the doc body to be indexed", func() bool {
		hits = searchJSON(t, phrase)
		for _, h := range hits {
			if h.ObjectId == docObj && h.Dataset == "editor_blocks" {
				return true
			}
		}
		return false
	})
	t.Logf("hits for %q: %s", phrase, hitSummary(hits))

	out, _ := runCLI(t, runOpts{sid: sidA}, "search", phrase)
	mustContain(t, out, "doc \"E2E Report\"", "search hit label")
	mustContain(t, out, "any://o/"+spaceId+"/"+docObj, "search hit link")
	mustContain(t, out, phrase, "search hit body snippet")

	// a hit on the name alone is listed too (only `props`-scope hits are
	// dropped from the plain listing): "Dup" appears in no body.
	waitFor(t, 30*time.Second, "the doc name to be indexed", func() bool {
		out, _ = runCLI(t, runOpts{sid: sidA}, "search", "E2E Dup Doc")
		return strings.Contains(out, "doc \"E2E Dup Doc\"")
	})

	// --space searches the collab space
	if teamId != "" {
		waitFor(t, 30*time.Second, "the shared doc to be indexed in the collab space", func() bool {
			out, _ = runCLI(t, runOpts{sid: sidA}, "search", "Shared with the team", "--space", teamName)
			return strings.Contains(out, "any://o/"+teamId+"/")
		})
	}

	// an object moved to any-ui's bin leaves search and the listings
	out, _ = runCLI(t, runOpts{sid: sidA, stdin: "# E2E Binned Doc\n\nThe quixotic marmalade protocol.\n"}, "doc", "add", "-")
	binned := linkId(t, out)
	waitFor(t, 30*time.Second, "the binned doc to be indexed", func() bool {
		out, _ = runCLI(t, runOpts{sid: sidA}, "search", "quixotic marmalade protocol")
		return strings.Contains(out, "E2E Binned Doc")
	})
	apiPost(t, "/v1/spaces/"+spaceId+"/properties/"+binned+"/collections/bin", nil, nil)
	waitFor(t, 30*time.Second, "the binned doc to leave the search results", func() bool {
		out, _ = runCLI(t, runOpts{sid: sidA}, "search", "quixotic marmalade protocol")
		return !strings.Contains(out, "E2E Binned Doc")
	})
	out, _ = runCLI(t, runOpts{sid: sidA}, "doc", "list")
	if strings.Contains(out, "E2E Binned Doc") {
		t.Errorf("doc list shows a binned doc:\n%s", out)
	}
}

type searchHit struct {
	ObjectId string  `json:"objectId"`
	Dataset  string  `json:"dataset"`
	RecordId string  `json:"recordId"`
	Score    float64 `json:"score"`
}

func searchJSON(t *testing.T, query string) []searchHit {
	t.Helper()
	out, _ := runCLI(t, runOpts{sid: sidA}, "search", query, "--json")
	var res struct {
		Hits       []searchHit `json:"hits"`
		Mode       string      `json:"mode"`
		ScoreScale string      `json:"scoreScale"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("search --json: %v\n%s", err, out)
	}
	searchScoreScale, searchMode = res.ScoreScale, res.Mode
	return res.Hits
}

// mustHaveSearchIndex skips the search-backed subtests on a server built
// without an index: it answers every search with 409 index.disabled.
func mustHaveSearchIndex(t *testing.T) {
	t.Helper()
	resp, err := http.Post("http://"+addr+"/v1/spaces/"+spaceId+"/search", "application/json",
		strings.NewReader(`{"query":"ping","limit":1}`))
	if err != nil {
		t.Fatalf("search probe: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	switch {
	case resp.StatusCode == 409 && strings.Contains(string(body), "index.disabled"):
		t.Skip("the server runs without a search index (index.enabled)")
	case resp.StatusCode >= 300:
		t.Fatalf("search probe: %d %s", resp.StatusCode, body)
	}
}

func hitSummary(hits []searchHit) string {
	var b strings.Builder
	for _, h := range hits {
		fmt.Fprintf(&b, "\n  %.4f %-14s %-16s %s", h.Score, h.Dataset, h.RecordId, h.ObjectId)
	}
	return b.String()
}

// --- 10. feedback + stats ---

func testFeedbackAndStats(t *testing.T) {
	ps := loadStateT(t, homeA).priv()
	out, _ := runCLI(t, runOpts{sid: sidA}, "feedback", "The e2e suite left this feedback.",
		"--kind", "note", "--ref", scope+":e2e-alpha")
	mustContain(t, out, "recorded", "feedback output")

	rows := apiRecords(t, ps.owner("app"), ps.coll("feedback"), nil)
	found := false
	for _, r := range rows {
		if strings.Contains(fmt.Sprint(r["text"]), "The e2e suite left this feedback") {
			found = true
			mustEqual(t, fmt.Sprint(r["kind"]), "note", "feedback kind")
			mustEqual(t, fmt.Sprint(r["ref"]), scope+":e2e-alpha", "feedback ref")
			mustEqual(t, fmt.Sprint(r["session"]), sidA, "feedback session")
			mustEqual(t, fmt.Sprint(r["machine"]), hostName, "feedback machine")
		}
	}
	if !found {
		t.Errorf("feedback record not found among %d rows", len(rows))
	}

	out, _ = runCLI(t, runOpts{sid: sidA}, "stats", "--global")
	mustContain(t, out, hostName, "stats --global mentions the machine")
	mustContain(t, out, "== machines", "stats --global sections")
	mustContain(t, out, "== usage", "stats --global sections")
	mustContain(t, out, "The e2e suite left this feedback", "stats --global feedback")

	// the flushed rollup: one record per machine and day
	var day map[string]any
	for _, r := range apiRecords(t, ps.owner("app"), ps.coll("stats"), map[string]any{"machine": hostName}) {
		day = r
	}
	if day == nil {
		t.Error("no stats record for this machine")
	} else if n, _ := day["sessions"].(float64); n < 1 {
		t.Errorf("stats record counts %v sessions, want at least 1: %s", day["sessions"], mustJSON(day))
	}

	out, _ = runCLI(t, runOpts{sid: sidA}, "stats")
	mustContain(t, out, "== project "+scope, "stats project header")
	mustContain(t, out, "memories   1", "stats project memory count")

	out, _ = runCLI(t, runOpts{sid: sidA}, "machine", "list")
	mustContain(t, out, "any://o/"+spaceId+"/"+ps.MachineObjectId, "machine list")
	mustContain(t, out, hostName, "machine list")
}

// --- 11. prompt hook ---

func testPromptHook(t *testing.T) {
	mustHaveSearchIndex(t)
	ps := loadStateT(t, homeA).priv()
	prompt := "Remind me what the " + phrase + " actually means for this project"
	// Body scores climb while the index warms, so wait until the doc would
	// actually pass the recall gate — a hook run before that injects other
	// hits (and burns them in the session's seen-set).
	var hits []searchHit
	waitFor(t, 30*time.Second, "the doc body to clear the auto-recall gate", func() bool {
		hits = searchJSON(t, prompt)
		return recallWouldPick(ps, hits, docObj)
	})
	t.Logf("hits for the prompt: %s", hitSummary(hits))

	first := promptHook(t, sidP, prompt)
	if first == "" {
		t.Fatalf("auto-recall injected nothing although the doc passes the gate; hits:%s", hitSummary(hits))
	}
	mustContain(t, first, "auto-recall", "prompt hook context")
	mustContain(t, first, "E2E Report", "prompt hook names the doc")
	mustContain(t, first, phrase, "prompt hook injects the body window")

	// the per-session seen-set: the same prompt never repeats a record
	second := promptHook(t, sidP, prompt)
	for _, label := range recallLabels(second) {
		if contains(recallLabels(first), label) {
			t.Errorf("the same prompt injected %s twice in one session:\nfirst:\n%s\nsecond:\n%s",
				label, first, second)
		}
	}
	if second != "" {
		t.Logf("the second prompt injected only records the first one missed:\n%s", second)
	}

	// short prompts and slash commands stay silent
	if ctx := promptHook(t, sidP, "/help"); ctx != "" {
		t.Errorf("a slash command triggered auto-recall:\n%s", ctx)
	}

	// another project's memories: a workflow rule travels, a project note stays home
	const other = "agents-e2e-elsewhere"
	apiPost(t, "/v1/spaces/"+spaceId+"/upsert", map[string]any{
		"objectId": ps.owner("app"), "dataset": ps.coll("memories"),
		"records": []map[string]any{
			{"id": other + ":flombastic-rule", "fields": map[string]any{"scope": other, "slug": "flombastic-rule", "kind": "feedback",
				"title": "Flombastic verbs get a kibble review", "body": "Flombastic verbs always get a kibble review before merge.", "links": []string{}, "origin": "e2e"}},
			{"id": other + ":flombastic-parser", "fields": map[string]any{"scope": other, "slug": "flombastic-parser", "kind": "project",
				"title": "Where the flombastic parser lives", "body": "The flombastic parser lives in pkg/flomb and panics on empty input.", "links": []string{}, "origin": "e2e"}},
		},
	}, nil)
	// one prompt per memory: on the unit scale only the top hit clears the gate
	clears := func(prompt, recordId string) {
		waitFor(t, 30*time.Second, recordId+" to clear the recall gate", func() bool {
			hits = searchJSON(t, prompt)
			for _, h := range hits {
				if h.Score >= ragMinScore() && h.RecordId == recordId {
					return true
				}
			}
			return false
		})
	}
	rulePrompt := "Which review do flombastic verbs always get before a merge?"
	clears(rulePrompt, other+":flombastic-rule")
	mustContain(t, promptHook(t, sidP, rulePrompt), other+":flombastic-rule", "another project's feedback memory is recalled")
	parserPrompt := "Where does the flombastic parser live and what does it do on empty input?"
	clears(parserPrompt, other+":flombastic-parser")
	if ctx := promptHook(t, sidP, parserPrompt); strings.Contains(ctx, other+":flombastic-parser") {
		t.Errorf("another project's project memory was recalled:\n%s", ctx)
	}
}

// searchScoreScale and searchMode are the score scale and the mode of
// the last searchJSON reply.
var searchScoreScale, searchMode string

// ragMinScore mirrors the hook's default gate for the last reply: its
// floor on the reply's score scale (ANY_AGENTS_RAG_MIN), and no hit
// passes a reply that is not hybrid.
func ragMinScore() float64 {
	switch {
	case searchMode != "hybrid":
		return math.Inf(1)
	case searchScoreScale == "unit":
		return 0.90
	}
	return 0.030
}

// recallWouldPick reports whether auto-recall would inject objectId's body:
// the hit must clear the score gate and land in the first three hits of an
// injectable dataset (the app root's own body is never injected).
func recallWouldPick(sp *spaceState, hits []searchHit, objectId string) bool {
	n := 0
	for _, h := range hits {
		if h.Score < ragMinScore() {
			continue
		}
		if h.ObjectId == sp.owner("app") && h.Dataset == "editor_blocks" {
			continue
		}
		switch h.Dataset {
		case sp.coll("memories"), sp.coll("config"), sp.coll("feedback"), "editor_blocks":
		default:
			continue
		}
		if h.ObjectId == objectId {
			return true
		}
		if n++; n == 3 {
			return false // the cap is reached before the doc
		}
	}
	return false
}

var recallLabelRe = regexp.MustCompile(`(?m)^\[(.+)\]$`)

// recallLabels lists the "[doc \"X\" any://…]" headers of an injected note.
func recallLabels(ctx string) []string {
	var out []string
	for _, m := range recallLabelRe.FindAllStringSubmatch(ctx, -1) {
		out = append(out, m[1])
	}
	return out
}

// --- 12. eval ---

func testEval(t *testing.T) {
	mustHaveSearchIndex(t)
	// no golden set in the bridge home yet
	_, errOut := runCLI(t, runOpts{sid: sidA, wantFail: true}, "eval")
	mustContain(t, errOut, filepath.Join(bridgeHome(homeA), "eval", "golden.json"), "eval without a golden set")

	golden := `[{"query": "alpha memory second revision", "expect": ["` + scope + `:e2e-alpha"]},
	            {"query": "a recipe for banana bread with walnuts", "expect": []}]`
	writeFile(t, filepath.Join(bridgeHome(homeA), "eval", "golden.json"), golden)
	out, _ := runCLI(t, runOpts{sid: sidA}, "eval")
	mustContain(t, out, "positives: 1", "eval over $ANY_AGENTS_HOME/eval/golden.json")
	mustContain(t, out, "negatives: 1", "eval over $ANY_AGENTS_HOME/eval/golden.json")

	other := filepath.Join(tmpRoot, "golden-other.json")
	writeFile(t, other, `[{"query": "alpha memory", "expect": ["`+scope+`:e2e-alpha"]},
	                      {"query": "beta memory", "expect": ["`+scope+`:e2e-beta"]}]`)
	out, _ = runCLI(t, runOpts{sid: sidA}, "eval", "--golden", other)
	mustContain(t, out, "positives: 2", "eval --golden")
	mustContain(t, out, "negatives: 0", "eval --golden")
}

// --- 13. status page ---

func testStatusPage(t *testing.T) {
	ps := loadStateT(t, homeA).priv()
	md := apiMarkdown(t, ps.owner("app"))
	for _, want := range []string{"Private context of this account's coding agents", "## Open tasks",
		"## Sessions", "## Recent docs", "## Memory", "## Machines", hostName} {
		mustContain(t, md, want, "status page")
	}
	if ps.collabOn() {
		mustContain(t, md, "## Open collabs", "status page")
	}
	// the app root's name is its title; the body never repeats it
	mustNoTitleH1(t, md, "Agents", "status page")
	mustContain(t, md, "any://o/"+spaceId+"/"+ps.MachineObjectId, "status page links the machine")
	mustContain(t, md, "any://o/"+spaceId+"/"+sessionObjA, "status page links the session")
	if strings.Contains(md, "E2E Shared Doc") {
		t.Errorf("the private status page lists the collab space's doc:\n%s", md)
	}
}

// --- running the binary ---

type runOpts struct {
	home     string // sandbox HOME; "" = homeA
	dir      string // working directory; "" = projDir
	stdin    string
	sid      string // CLAUDE_CODE_SESSION_ID
	extraEnv []string
	wantFail bool
	member   bool // run as the second member: homeM, addrB, its own private space
}

// bridgeHome is the sandbox's ANY_AGENTS_HOME, toolHome its CLAUDE_CONFIG_DIR.
func bridgeHome(home string) string { return filepath.Join(home, ".any-agents") }
func toolHome(home string) string   { return filepath.Join(home, ".claude") }

func execCLI(o runOpts, args ...string) (stdout, stderr string, err error) {
	home, dir, server, private := o.home, o.dir, addr, spaceName
	if o.member {
		home, server, private = homeM, addrB, spaceName+"-member"
	}
	if home == "" {
		home = homeA
	}
	if dir == "" {
		dir = projDir
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binPath, args...)
	cmd.Dir = dir
	cmd.Env = append([]string{
		"HOME=" + home,
		"PATH=" + sandboxPath(),
		"TMPDIR=" + tmpRoot,
		"ANY_AGENTS_E2E=1",
		"ANY_AGENTS_HOME=" + bridgeHome(home),
		"CLAUDE_CONFIG_DIR=" + toolHome(home),
		"ANY_AGENTS_SPACE=" + private,
		"ANY_AGENTS_ADDR=" + server,
	}, o.extraEnv...)
	if o.sid != "" {
		cmd.Env = append(cmd.Env, "CLAUDE_CODE_SESSION_ID="+o.sid)
	}
	if o.stdin != "" {
		cmd.Stdin = strings.NewReader(o.stdin)
	}
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &outBuf, &errBuf
	err = cmd.Run()
	return outBuf.String(), errBuf.String(), err
}

// sandboxPath drops the real user's bin directories from PATH so the CLI
// sees only system tools (git). It keeps the run hermetic: `linctl` and
// `claude` stay invisible, so Linear issue pages are minted from the key
// alone instead of calling the Linear API.
func sandboxPath() string {
	realHome, err := os.UserHomeDir()
	if err != nil || realHome == "" {
		return os.Getenv("PATH")
	}
	var kept []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" || strings.HasPrefix(dir, realHome+string(filepath.Separator)) {
			continue
		}
		kept = append(kept, dir)
	}
	return strings.Join(kept, string(filepath.ListSeparator))
}

func runCLI(t *testing.T, o runOpts, args ...string) (stdout, stderr string) {
	t.Helper()
	out, errOut, err := execCLI(o, args...)
	switch {
	case err != nil && !o.wantFail:
		t.Fatalf("any-agents %s: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, out, errOut)
	case err == nil && o.wantFail:
		t.Fatalf("any-agents %s: expected a non-zero exit\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), out, errOut)
	}
	return out, errOut
}

type hookOutput struct {
	HookSpecificOutput struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

// sessionStartHook pipes a SessionStart payload into the command the
// installed hook runs and returns the injected context.
func sessionStartHook(t *testing.T, sessionId, source string) string {
	t.Helper()
	in := map[string]string{"session_id": sessionId, "cwd": projDir, "source": source}
	out, errOut := runCLI(t, runOpts{stdin: mustJSON(in)}, "hook", "--tool", "claude", "session-start")
	mustNotLogErrors(t, errOut, "session-start hook")
	var h hookOutput
	if err := json.Unmarshal([]byte(out), &h); err != nil {
		t.Fatalf("hook session-start output: %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	mustEqual(t, h.HookSpecificOutput.HookEventName, "SessionStart", "hookEventName")
	if h.HookSpecificOutput.AdditionalContext == "" {
		t.Fatalf("hook session-start injected no context\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	return h.HookSpecificOutput.AdditionalContext
}

// promptHook pipes a UserPromptSubmit payload and returns the injected
// context ("" when the hook stayed silent).
func promptHook(t *testing.T, sessionId, prompt string) string {
	t.Helper()
	in := map[string]string{"session_id": sessionId, "cwd": projDir, "prompt": prompt}
	out, errOut := runCLI(t, runOpts{stdin: mustJSON(in)}, "hook", "--tool", "claude", "prompt")
	if strings.TrimSpace(out) == "" {
		return ""
	}
	var h hookOutput
	if err := json.Unmarshal([]byte(out), &h); err != nil {
		t.Fatalf("hook prompt output: %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	mustEqual(t, h.HookSpecificOutput.HookEventName, "UserPromptSubmit", "hookEventName")
	return h.HookSpecificOutput.AdditionalContext
}

// --- state.json ---

type bundleState struct {
	RootId      string            `json:"rootId"`
	Props       map[string]string `json:"props"`
	Collections map[string]string `json:"collections"`
	Dataview    string            `json:"dataview"`
}

type spaceState struct {
	Id              string                  `json:"id"`
	Name            string                  `json:"name"`
	Kind            string                  `json:"kind"`
	Usecases        map[string]bool         `json:"usecases"`
	Bundles         map[string]*bundleState `json:"bundles"`
	MachineObjectId string                  `json:"machineObjectId"`
	SetupVersion    int                     `json:"setupVersion"`
}

type cliState struct {
	Version       int                    `json:"version"`
	Identity      string                 `json:"identity"`
	TechSpaceId   string                 `json:"techSpaceId"`
	Private       string                 `json:"private"`
	Spaces        map[string]*spaceState `json:"spaces"`
	LastSessionId string                 `json:"lastSessionId"`
}

// priv is the private space (empty when missing, so lookups fail loudly
// in the assertions rather than with a nil pointer).
func (s *cliState) priv() *spaceState {
	if sp := s.Spaces[s.Private]; sp != nil {
		return sp
	}
	return &spaceState{}
}

func (s *spaceState) bundle(name string) *bundleState {
	if b := s.Bundles[name]; b != nil {
		return b
	}
	return &bundleState{}
}

// owner is a bundle's root: the collection id property values are
// grouped under, or the type id for the app root and Collab.
func (s *spaceState) owner(name string) string      { return s.bundle(name).RootId }
func (s *spaceState) prop(name, xkey string) string { return s.bundle(name).Props[xkey] }
func (s *spaceState) coll(key string) string        { return s.bundle("app").Collections[key] }
func (s *spaceState) dataview(name string) string   { return s.bundle(name).Dataview }
func (s *spaceState) collabOn() bool                { return s.owner("collab") != "" }
func (s *spaceState) propPath(name, xkey string) string {
	return s.owner(name) + "." + s.prop(name, xkey)
}

func statePath(home string) string { return filepath.Join(bridgeHome(home), "state.json") }

func loadState(home string) (*cliState, error) {
	b, err := os.ReadFile(statePath(home))
	if err != nil {
		return nil, err
	}
	var st cliState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func loadStateT(t *testing.T, home string) *cliState {
	t.Helper()
	st, err := loadState(home)
	if err != nil {
		t.Fatalf("read state.json: %v", err)
	}
	return st
}

// memoryDir mirrors tool.go: $CLAUDE_CONFIG_DIR/projects/<folded cwd>/memory.
func memoryDir(home, projectDir string) string {
	var b strings.Builder
	for _, r := range projectDir {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return filepath.Join(toolHome(home), "projects", b.String(), "memory")
}

func memFile(name, description, kind, body string) string {
	return fmt.Sprintf("---\nname: %s\ndescription: %s\nmetadata:\n  type: %s\n---\n\n%s\n",
		name, description, kind, body)
}

// sharedMemFile mirrors memfiles.go's renderSharedMemFile: the memory
// file plus the collab space it is shared in.
func sharedMemFile(name, description, kind, space, body string) string {
	return fmt.Sprintf("---\nname: %s\ndescription: %s\nmetadata:\n  type: %s\n  shared: %s\n---\n\n%s\n",
		name, description, kind, space, body)
}

// sharedDir mirrors syncmem.go's sharedDirName: the space name folded to
// [a-z0-9-] plus the last six alphanumerics of its id.
func sharedDir(name, id string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
			b.WriteRune('-')
		}
	}
	folded := strings.Trim(b.String(), "-")
	if folded == "" {
		folded = "space"
	}
	tail := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return -1
	}, id)
	if len(tail) > 6 {
		tail = tail[len(tail)-6:]
	}
	return folded + "-" + tail
}

// --- HTTP ---

func health() error { return healthAt(addr) }

func healthAt(server string) error {
	resp, err := http.Get("http://" + server + "/v1/health")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("health returned %d", resp.StatusCode)
	}
	return nil
}

type spaceInfo struct {
	Id      string `json:"id"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	OwnRole string `json:"ownRole"`
}

func httpJSON(method, path string, out any) error { return httpJSONAt(addr, method, path, out) }

func httpJSONAt(server, method, path string, out any) error {
	req, err := http.NewRequest(method, "http://"+server+path, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, b)
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

func listSpacesAt(server string) ([]spaceInfo, error) {
	var out struct {
		Spaces []spaceInfo `json:"spaces"`
	}
	return out.Spaces, httpJSONAt(server, "GET", "/v1/spaces", &out)
}

func getSpace(id string) (spaceInfo, error) { return getSpaceAt(addr, id) }

func getSpaceAt(server, id string) (spaceInfo, error) {
	var out spaceInfo
	return out, httpJSONAt(server, "GET", "/v1/spaces/"+id, &out)
}

func deleteSpaceAt(server, id string) error {
	return httpJSONAt(server, "DELETE", "/v1/spaces/"+id, nil)
}

func apiDo(t *testing.T, method, path string, in, out any) {
	t.Helper()
	apiDoAt(t, addr, method, path, in, out)
}

func apiDoAt(t *testing.T, server, method, path string, in, out any) {
	t.Helper()
	var body io.Reader
	if in != nil {
		body = strings.NewReader(mustJSON(in))
	}
	req, err := http.NewRequest(method, "http://"+server+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, data)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s %s: decode: %v\n%s", method, path, err, data)
		}
	}
}

func apiGet(t *testing.T, path string, out any)      { t.Helper(); apiDo(t, "GET", path, nil, out) }
func apiPost(t *testing.T, path string, in, out any) { t.Helper(); apiDo(t, "POST", path, in, out) }

// apiObject reads an object row; the space defaults to the private one.
func apiObject(t *testing.T, id string, space ...string) map[string]any {
	t.Helper()
	if id == "" {
		t.Fatal("apiObject: empty object id")
	}
	sp := spaceId
	if len(space) > 0 {
		sp = space[0]
	}
	var out struct {
		Record map[string]any `json:"record"`
	}
	apiGet(t, "/v1/spaces/"+sp+"/objects/"+id, &out)
	return out.Record
}

func apiMarkdown(t *testing.T, id string) string { t.Helper(); return apiMarkdownIn(t, spaceId, id) }

func apiMarkdownIn(t *testing.T, space, id string) string {
	t.Helper()
	var out struct {
		Content string `json:"content"`
	}
	apiGet(t, "/v1/spaces/"+space+"/objects/"+id+"/editor/editor_blocks/markdown", &out)
	return out.Content
}

// apiFileIn downloads an attachment's raw bytes.
func apiFileIn(t *testing.T, space, fileId string) []byte {
	t.Helper()
	resp, err := http.Get("http://" + addr + "/v1/spaces/" + space + "/files/" + fileId + "/content")
	if err != nil {
		t.Fatalf("download file %s: %v", fileId, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		t.Fatalf("download file %s: %d %s", fileId, resp.StatusCode, data)
	}
	return data
}

func gunzip(t *testing.T, data []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	return string(out)
}

func apiQueryObjects(t *testing.T, filter map[string]any) []map[string]any {
	t.Helper()
	return apiQueryObjectsIn(t, spaceId, filter)
}

func apiQueryObjectsIn(t *testing.T, space string, filter map[string]any) []map[string]any {
	t.Helper()
	var out struct {
		Records []map[string]any `json:"records"`
	}
	apiPost(t, "/v1/spaces/"+space+"/objects/query",
		map[string]any{"filter": filter, "limit": 500}, &out)
	return out.Records
}

func apiRecords(t *testing.T, objectId, dataset string, filter map[string]any) []map[string]any {
	t.Helper()
	return apiRecordsIn(t, spaceId, objectId, dataset, filter)
}

func apiRecordsIn(t *testing.T, space, objectId, dataset string, filter map[string]any) []map[string]any {
	t.Helper()
	return apiRecordsAt(t, addr, space, objectId, dataset, filter)
}

func apiRecordsAt(t *testing.T, server, space, objectId, dataset string, filter map[string]any) []map[string]any {
	t.Helper()
	var out struct {
		Records []map[string]any `json:"records"`
	}
	body := map[string]any{"objectId": objectId, "dataset": dataset, "limit": 500}
	if filter != nil {
		body["filter"] = filter
	}
	apiDoAt(t, server, "POST", "/v1/spaces/"+space+"/query", body, &out)
	return out.Records
}

// bundleChild resolves the deterministic child page of a bundle root for
// a seed (idempotent: an existing child comes back as is).
func bundleChild(t *testing.T, space, bundleId, seed, collection string) string {
	t.Helper()
	var out struct {
		ObjectId string `json:"objectId"`
	}
	apiPost(t, "/v1/spaces/"+space+"/bundles/"+url.PathEscape(bundleId)+"/children",
		map[string]any{"seed": seed, "type": "page", "collections": []string{collection}}, &out)
	return out.ObjectId
}

// memberName is a member's profile name in a space ("" when unnamed).
func memberName(t *testing.T, space, ident string) string {
	t.Helper()
	var out struct {
		Members []struct{ Identity, Name string } `json:"members"`
	}
	apiGet(t, "/v1/spaces/"+space+"/members", &out)
	for _, m := range out.Members {
		if m.Identity == ident {
			return m.Name
		}
	}
	return ""
}

// --- row accessors (mirror ids.go) ---

// collectionFilter selects the live members of a collection.
func collectionFilter(collectionId string, extra ...map[string]any) map[string]any {
	and := []any{map[string]any{"any.collections": collectionId},
		map[string]any{"any.collections": map[string]any{"$nin": []string{"bin"}}}}
	for _, e := range extra {
		and = append(and, e)
	}
	return map[string]any{"$and": and}
}

func group(row map[string]any, ownerId string) map[string]any {
	g, _ := row[ownerId].(map[string]any)
	if g == nil {
		return map[string]any{}
	}
	return g
}

func nameOf(row map[string]any) string {
	n, _ := group(row, "any")["name"].(string)
	return n
}

// typeOf reads an object's one type.
func typeOf(row map[string]any) string {
	t, _ := group(row, "any")["type"].(string)
	return t
}

// inCollection reports whether an object is filed under a collection.
func inCollection(row map[string]any, collectionId string) bool {
	return contains(arrOf(row, "any", "collections"), collectionId)
}

func arrOf(row map[string]any, ownerId, propId string) []string {
	arr, _ := group(row, ownerId)[propId].([]any)
	var out []string
	for _, v := range arr {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func choiceOf(row map[string]any, ownerId, propId string) string {
	if v := arrOf(row, ownerId, propId); len(v) > 0 {
		return v[0]
	}
	return ""
}

func strOf(row map[string]any, ownerId, propId string) string {
	s, _ := group(row, ownerId)[propId].(string)
	return s
}

func numOf(row map[string]any, ownerId, propId string) float64 {
	f, _ := group(row, ownerId)[propId].(float64)
	return f
}

// linksOf reads a relation value (an array of any://<id>).
func linksOf(row map[string]any, ownerId, propId string) []string {
	var out []string
	for _, v := range arrOf(row, ownerId, propId) {
		out = append(out, strings.TrimPrefix(v, "any://"))
	}
	return out
}

// firstLink reads the first id out of a relation value.
func firstLink(row map[string]any, ownerId, propId string) string {
	if ids := linksOf(row, ownerId, propId); len(ids) > 0 {
		return ids[0]
	}
	return ""
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

var linkRe = regexp.MustCompile(`any://o/[^/\s]+/([A-Za-z0-9._-]+)`)

// linkId pulls the object id out of the first any://o/<space>/<id> link in
// a command's output.
// mustAnnounced finds the general-chat message whose text contains what
// and checks that it carries link as an attachment.
func mustAnnounced(t *testing.T, space, chatId, what, link string) {
	t.Helper()
	for _, m := range apiRecordsIn(t, space, chatId, "chat_messages", nil) {
		text, _ := m["text"].(string)
		if !strings.Contains(text, what) {
			continue
		}
		atts, _ := m["attachments"].(map[string]any)
		for _, a := range atts {
			if am, _ := a.(map[string]any); am["link"] == link {
				return
			}
		}
		t.Errorf("announcement %q has no attachment %s: %s", what, link, mustJSON(m))
		return
	}
	t.Errorf("no announcement containing %q in the general chat", what)
}

func linkId(t *testing.T, out string) string {
	t.Helper()
	m := linkRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no any://o/ link in output:\n%s", out)
	}
	return m[1]
}

func sessionRow(t *testing.T, sp *spaceState, sessionId string) map[string]any {
	t.Helper()
	rows := apiQueryObjectsIn(t, sp.Id, collectionFilter(sp.owner("session"),
		map[string]any{sp.propPath("session", "sessionId"): sessionId}))
	if len(rows) != 1 {
		t.Fatalf("want one session page for %s, got %d", sessionId, len(rows))
	}
	return rows[0]
}

// linearIssueId resolves the Linear issue page minted for a key in the
// private space. With no linctl in the sandbox the page carries only the key.
func linearIssueId(t *testing.T, sp *spaceState, key string) string {
	t.Helper()
	return linearIssueIdIn(t, sp, key)
}

func linearIssueIdIn(t *testing.T, sp *spaceState, key string) string {
	t.Helper()
	rows := apiQueryObjectsIn(t, sp.Id, collectionFilter(sp.owner("linear"),
		map[string]any{sp.propPath("linear", "key"): key}))
	if len(rows) != 1 {
		t.Fatalf("%s: want one Linear issue page for %s, got %d", sp.Name, key, len(rows))
	}
	mustEqual(t, typeOf(rows[0]), "page", key+" issue page type")
	// Without linctl the name is the bare key; an enriched page adds the title.
	if name := nameOf(rows[0]); !strings.HasPrefix(name, key) {
		t.Errorf("Linear issue page name = %q, want it to start with %s", name, key)
	}
	id, _ := rows[0]["id"].(string)
	return id
}

func docsNamed(t *testing.T, sp *spaceState, name string) []map[string]any {
	t.Helper()
	return apiQueryObjectsIn(t, sp.Id, collectionFilter(sp.owner("doc"), map[string]any{"any.name": name}))
}

func memRecords(t *testing.T, sp *spaceState) map[string]map[string]any {
	t.Helper()
	return recordsById(apiRecordsIn(t, sp.Id, sp.owner("app"), sp.coll("memories"), map[string]any{"scope": scope}))
}

func recordsById(rows []map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, r := range rows {
		id, _ := r["id"].(string)
		out[id] = r
	}
	return out
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// --- assertions and small helpers ---

// mustBeFiled fails when an object is not filed under a collection.
func mustBeFiled(t *testing.T, row map[string]any, collectionId, what string) {
	t.Helper()
	if collectionId == "" || !inCollection(row, collectionId) {
		t.Errorf("%s is not filed under %q: collections %v", what, collectionId, group(row, "any")["collections"])
	}
}

// mustNoTitleH1 fails when a stored body still opens with the object's
// name as an H1 (clients render the name as the title).
func mustNoTitleH1(t *testing.T, body, name, what string) {
	t.Helper()
	if strings.HasPrefix(strings.TrimLeft(body, "\n"), "# "+name) {
		t.Errorf("%s still starts with the %q H1:\n%s", what, "# "+name, body)
	}
}

// mustNotLogErrors fails when a command logged a diagnostic. Commands log
// swallowed server errors as "any-agents: …" on stderr while still exiting 0.
func mustNotLogErrors(t *testing.T, stderr, what string) {
	t.Helper()
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "any-agents:") && !strings.Contains(line, "memories in scope") {
			t.Errorf("%s logged a diagnostic: %s", what, line)
		}
	}
}

// mustOnlyDeclaredProps fails when an object's property group carries a key
// that the bundle does not declare (an empty key, or a leftover property).
func mustOnlyDeclaredProps(t *testing.T, sp *spaceState, bundle string, row map[string]any) {
	t.Helper()
	declared := map[string]bool{}
	for _, id := range sp.bundle(bundle).Props {
		declared[id] = true
	}
	for key := range group(row, sp.owner(bundle)) {
		if !declared[key] {
			t.Errorf("%s object carries undeclared property %q (declared: %v)",
				bundle, key, sp.bundle(bundle).Props)
		}
	}
}

func mustContain(t *testing.T, got, want, what string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Errorf("%s: expected %q in:\n%s", what, want, got)
	}
}

func mustEqual[T comparable](t *testing.T, got, want T, what string) {
	t.Helper()
	if got != want {
		t.Errorf("%s: got %v, want %v", what, got, want)
	}
}

func mustEqualSlice(t *testing.T, got, want []string, what string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: got %q, want %q", what, got, want)
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// waitFor polls until cond holds or the deadline passes (for the search
// index and other asynchronous server work).
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", d, what)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// wantSetupVersion is the binary's setupVersion, read from the source so
// a bump does not have to be mirrored here.
func wantSetupVersion(t *testing.T) int {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "schema.go"))
	if err != nil {
		t.Fatalf("read schema.go: %v", err)
	}
	m := regexp.MustCompile(`setupVersion\s*=\s*(\d+)`).FindSubmatch(src)
	if m == nil {
		t.Fatal("setupVersion not found in schema.go")
	}
	n, _ := strconv.Atoi(string(m[1]))
	return n
}

// stopHook pipes a Stop payload and returns the block reason ("" when
// the hook let the agent stop).
func stopHook(t *testing.T, sessionId string, active bool) string {
	t.Helper()
	in := map[string]any{"session_id": sessionId, "cwd": projDir, "stop_hook_active": active}
	out, _ := runCLI(t, runOpts{stdin: mustJSON(in)}, "hook", "--tool", "claude", "stop")
	if strings.TrimSpace(out) == "" {
		return ""
	}
	var h struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(out), &h); err != nil {
		t.Fatalf("hook stop output: %v\n%s", err, out)
	}
	mustEqual(t, h.Decision, "block", "stop hook decision")
	return h.Reason
}

// --- 5c. task update and link checks ---

func testTaskUpdate(t *testing.T) {
	ps := loadStateT(t, homeA).priv()
	tk, d := ps.owner("task"), ps.owner("doc")
	out, _ := runCLI(t, runOpts{sid: sidA, stdin: "# E2E Task Update\n\nChange me.\n"}, "task", "add", "-")
	id := linkId(t, out)
	_, errOut := runCLI(t, runOpts{sid: sidA, wantFail: true}, "task", "update", id)
	mustContain(t, errOut, "nothing to change", "task update without flags")
	runCLI(t, runOpts{sid: sidA}, "task", "update", id, "--priority", "high", "--assignee", identity, "--title", "E2E Task Updated")
	row := apiObject(t, id)
	mustEqual(t, nameOf(row), "E2E Task Updated", "task update --title")
	mustEqual(t, choiceOf(row, tk, ps.prop("task", "priority")), "high", "task update --priority")
	mustEqualSlice(t, arrOf(row, tk, ps.prop("task", "assignee")), []string{"any://u/" + identity}, "task update --assignee")
	runCLI(t, runOpts{sid: sidA}, "task", "update", id, "--assignee", "-")
	if a := arrOf(apiObject(t, id), tk, ps.prop("task", "assignee")); len(a) != 0 {
		t.Errorf("task update --assignee - kept %v", a)
	}
	_, errOut = runCLI(t, runOpts{sid: sidA, wantFail: true}, "task", "update", id, "--priority", "urgent")
	mustContain(t, errOut, "low, normal or high", "task update with a bad priority")

	// link takes objects of the space only
	_, errOut = runCLI(t, runOpts{sid: sidA, wantFail: true}, "task", "link", id, "DROID-388")
	mustContain(t, errOut, "--linear", "task link with an issue key")
	_, errOut = runCLI(t, runOpts{sid: sidA, wantFail: true}, "task", "link", id, "not-an-object")
	mustContain(t, errOut, "not an object", "task link with a bare word")
	if n := len(arrOf(apiObject(t, id), d, ps.prop("doc", "related"))); n != 0 {
		t.Errorf("task link stored %d target(s) from invalid refs", n)
	}
	runCLI(t, runOpts{sid: sidA}, "task", "link", id, taskObj)
	mustEqualSlice(t, arrOf(apiObject(t, id), d, ps.prop("doc", "related")), []string{"any://" + taskObj}, "task link to an object")
}

// lowerSetupVersion makes a space look set up by an older binary: the
// state file's version and the dataview's record both drop to 1, so the
// next command on the space re-ensures it.
func lowerSetupVersion(t *testing.T, home, spaceId, dvHost string) {
	t.Helper()
	path := filepath.Join(home, ".any-agents", "state.json")
	var st map[string]any
	if err := json.Unmarshal([]byte(readFile(t, path)), &st); err != nil {
		t.Fatal(err)
	}
	spaces, _ := st["spaces"].(map[string]any)
	sp, _ := spaces[spaceId].(map[string]any)
	if sp == nil {
		t.Fatalf("state.json has no space %s", spaceId)
	}
	sp["setupVersion"] = 1
	writeFile(t, path, mustJSON(st))
	apiPost(t, "/v1/spaces/"+spaceId+"/modify", map[string]any{"objectId": dvHost, "dataset": "dataviews", "records": []map[string]any{
		{"id": "default", "ops": []map[string]any{{"type": "$set", "path": "setupVersion", "value": 1}}}}}, nil)
}
