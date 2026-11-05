package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Hook handlers. Hooks must NEVER break an agent session: any failure
// (server down, space missing) logs to stderr and exits 0.

// hookClient times out well inside the harness's own hook timeout, so a
// hung server costs a logged error, not a killed hook.
func hookClient() *Client {
	c := NewClient()
	c.hc.Timeout = 25 * time.Second
	return c
}

func hookSessionStart(tool Tool) {
	in := tool.ParseHookInput(os.Stdin)
	c := hookClient()
	st, err := loadState()
	if err == nil {
		err = ensureSetup(c, st, false)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "any-agents: setup: %v\n", err)
		return
	}
	sp := st.private()
	bumpStat("sessions", 1)
	defer flushStats(c, sp)
	shared := pullSharedMemory(c, st, in.Cwd)
	if n, err := pullMemory(c, sp, in.Cwd, false, shared); err != nil {
		recordErrorStat("pull", err)
	} else if n > 0 || len(shared) > 0 {
		fmt.Fprintf(os.Stderr, "any-agents: %d memories in scope, %d shared set(s)\n", n, len(shared))
	}
	if _, err := pullConfig(c, sp); err != nil {
		recordErrorStat("config pull", err)
	}
	if in.SessionId == "" {
		return
	}
	ctx, err := sessionStart(c, st, tool, in.SessionId, in.Cwd, in.Source)
	if err != nil {
		recordErrorStat("session start", err)
		return
	}
	ctx += collabDigest(c, st) + releaseDigest(c, st, in.Cwd)
	tool.EmitContext("SessionStart", ctx)
	if err := renderStatusPage(c, sp); err != nil {
		fmt.Fprintf(os.Stderr, "any-agents: status page: %v\n", err)
	}
}

func hookSessionEnd(tool Tool) {
	in := tool.ParseHookInput(os.Stdin)
	c := hookClient()
	st, err := loadState()
	if err != nil || st.private() == nil || !st.private().ready() {
		return
	}
	sp := st.private()
	defer flushStats(c, sp)
	if in.SessionId != "" {
		if _, err := sessionEnd(c, st, in.SessionId); err != nil {
			recordErrorStat("session end", err)
		}
		// A missing transcript is normal (cleared or instantly-exited
		// session) — only real archive failures count as errors.
		if in.TranscriptPath != "" {
			if _, statErr := os.Stat(in.TranscriptPath); statErr == nil {
				if err := archiveTranscript(c, sp, in.SessionId, in.TranscriptPath); err != nil {
					recordErrorStat("transcript archive", err)
				}
			}
		}
	}
	if err := pushMemory(c, sp, in.Cwd); err != nil {
		recordErrorStat("push", err)
	}
	if err := pushConfig(c, sp); err != nil {
		recordErrorStat("config push", err)
	}
	_ = touchMachine(c, sp, "idle")
	for _, s := range append([]*Space{sp}, st.collabs()...) {
		if s.ready() {
			if err := renderStatusPage(c, s); err != nil {
				fmt.Fprintf(os.Stderr, "any-agents: status page %s: %v\n", s.Name, err)
			}
		}
	}
}

func hookPostTool(tool Tool) {
	in := tool.ParseHookInput(os.Stdin)
	st, err := loadState()
	if err != nil || st.private() == nil || !st.private().ready() {
		return
	}
	sp := st.private()
	c := hookClient()
	home := tool.Home()
	// Shell-mediated edits (sed/python heredocs) bypass the file-tool
	// matcher; sniff the command for config and memory paths.
	if in.FilePath == "" && in.Command != "" {
		cmd := in.Command
		for _, root := range tool.ConfigRoots() {
			if strings.Contains(cmd, filepath.Join(filepath.Base(home), root)) {
				if err := pushConfig(c, sp); err != nil {
					fmt.Fprintf(os.Stderr, "any-agents: config push: %v\n", err)
				}
				break
			}
		}
		if strings.Contains(cmd, "/memory/") {
			if err := pushMemory(c, sp, in.Cwd); err != nil {
				fmt.Fprintf(os.Stderr, "any-agents: push: %v\n", err)
			}
			if err := pushSharedMemory(c, st, in.Cwd); err != nil {
				fmt.Fprintf(os.Stderr, "any-agents: shared push: %v\n", err)
			}
		}
		return
	}
	path := in.FilePath
	if path == "" {
		return
	}
	memDir := tool.MemoryDir(in.Cwd)
	if strings.HasPrefix(path, filepath.Join(memDir, "shared")+string(filepath.Separator)) {
		if _, err := os.Stat(path); err == nil && !ownFile(path) {
			fmt.Fprintf(os.Stderr, "any-agents: %s is a read-only mirror of another member's memory; share your own or ask the publisher\n", path)
			return
		}
		if err := pushSharedMemory(c, st, in.Cwd); err != nil {
			fmt.Fprintf(os.Stderr, "any-agents: shared push: %v\n", err)
		}
		return
	}
	if strings.HasPrefix(path, memDir+string(filepath.Separator)) {
		if err := pushMemory(c, sp, in.Cwd); err != nil {
			fmt.Fprintf(os.Stderr, "any-agents: push: %v\n", err)
		}
		return
	}
	for _, root := range tool.ConfigRoots() {
		p := filepath.Join(home, root)
		if path == p || strings.HasPrefix(path, p+string(filepath.Separator)) {
			if err := pushConfig(c, sp); err != nil {
				fmt.Fprintf(os.Stderr, "any-agents: config push: %v\n", err)
			}
			return
		}
	}
}

// hookPrompt is the auto-recall (RAG) path: search the private space and
// every collab space with the user's prompt and inject the strongest hits
// as context. A score threshold keeps FTS-always-matches noise out, a hard
// cap bounds context cost, and a per-session seen-set avoids re-injecting
// the same record.
func hookPrompt(tool Tool) {
	in := tool.ParseHookInput(os.Stdin)
	st, err := loadState()
	if err != nil || st.private() == nil || !st.private().ready() {
		return
	}
	c := hookClient()
	prompt := strings.TrimSpace(in.Prompt)
	notice := unreadCollabNotice(c, st, in.SessionId)
	if len(prompt) < 20 || len(strings.Fields(prompt)) < 4 ||
		strings.HasPrefix(prompt, "/") || strings.HasPrefix(prompt, "!") {
		if notice != "" {
			tool.EmitContext("UserPromptSubmit", notice)
		}
		return
	}
	seen := loadRagSeen(in.SessionId)
	scope := resolveScope(in.Cwd)
	type hit struct {
		sp *Space
		h  SearchHit
	}
	bumpStat("ragPrompts", 1)
	// One search per space, concurrently: the prompt hook has to answer
	// well inside the harness's timeout however many spaces there are.
	spaces := append([]*Space{st.private()}, st.collabs()...)
	results := make([][]hit, len(spaces))
	var wg sync.WaitGroup
	for i, sp := range spaces {
		if !sp.ready() {
			continue
		}
		wg.Add(1)
		go func(i int, sp *Space) {
			defer wg.Done()
			res, err := c.Search(sp.Id, prompt, 8)
			if err != nil {
				return
			}
			for _, h := range recallPick(c, sp, scope, res.Hits, ragMinScore(), seen, 3) {
				results[i] = append(results[i], hit{sp, h})
			}
		}(i, sp)
	}
	wg.Wait()
	var picked []hit
	for _, r := range results {
		picked = append(picked, r...)
	}
	if len(picked) == 0 {
		if notice != "" {
			tool.EmitContext("UserPromptSubmit", notice)
		}
		return
	}
	sort.SliceStable(picked, func(i, j int) bool { return picked[i].h.Score > picked[j].h.Score })
	// One body excerpt per prompt across all spaces, the best-scoring one.
	kept, excerpts := picked[:0], 0
	for _, p := range picked {
		if p.h.Dataset == "editor_blocks" {
			if excerpts == 1 {
				continue
			}
			excerpts++
		}
		kept = append(kept, p)
	}
	picked = kept
	if len(picked) > 3 {
		picked = picked[:3]
	}
	var b strings.Builder
	b.WriteString("any-agents auto-recall — shared context possibly relevant to this prompt (ignore if not applicable; more via `any-agents search \"...\" [--space S]`):\n")
	for _, p := range picked {
		seen[p.h.Dataset+"/"+p.h.RecordId] = true
		label := hitLabel(c, p.sp, p.h)
		if p.sp.Kind == kindCollab {
			label = p.sp.Name + ": " + label
		}
		fmt.Fprintf(&b, "\n[%s]\n%s\n", label, trunc(p.h.Data, 500))
	}
	saveRagSeen(in.SessionId, seen)
	bumpStat("ragInjected", len(picked))
	if notice != "" {
		b.WriteString("\n" + notice)
	}
	tool.EmitContext("UserPromptSubmit", b.String())
}

// unreadCollabNotice names the open collabs I am part of that have
// messages this machine has not read, or "". It nags on every prompt
// until `collab show` or `collab wait` advances the cursor.
func unreadCollabNotice(c *Client, st *State, sessionId string) string {
	var items []string
	for _, sp := range st.collabs() {
		if !sp.ready() || !sp.has("collab") {
			continue
		}
		rows, err := objectsOf(c, sp, "collab", []string{"-" + propPath(sp, "collab", "date")},
			map[string]any{propPath(sp, "collab", "status"): map[string]any{"$in": collabOpen}})
		if err != nil {
			continue
		}
		t := sp.owner("collab")
		for _, r := range rows {
			requester := firstMember(r, t, sp.prop("collab", "requester"))
			if requester != st.me() && !contains(membersOf(r, t, sp.prop("collab", "assignees")), st.me()) {
				continue
			}
			id, _ := r["id"].(string)
			if !engaged(id, sessionId) {
				continue // a sibling session of this member handles it
			}
			if n := newMessageCount(c, st, sp, id, 10); n > 0 {
				role := "helper"
				if requester == st.me() {
					role = "requester"
				}
				items = append(items, fmt.Sprintf("%q +%d (you are the %s) %s", nameOf(r), n, role, globalLink(sp.Id, id)))
			}
		}
	}
	if len(items) == 0 {
		return ""
	}
	return "any-agents: unread replies on collab(s) you are part of: " + strings.Join(items, "; ") +
		" — read with `any-agents collab show <link>`, answer with `collab say`, then keep `collab wait <link>` running until the collab closes: once your part is done, `collab done <link> --met \"<evidence>\"` records your agreement, and it closes when every participant agreed or a user decides (`--user \"<their words>\"`).\n"
}

// hookStop holds the agent once per turn when a collab this session took
// part in has an unread reply, so posting an answer is not the end of
// the task; a sibling session of the same member is left alone.
func hookStop(tool Tool) {
	in := tool.ParseHookInput(os.Stdin)
	if in.StopHookActive {
		return
	}
	st, err := loadState()
	if err != nil || st.private() == nil || !st.private().ready() {
		return
	}
	if notice := unreadCollabNotice(hookClient(), st, in.SessionId); notice != "" {
		tool.EmitBlock(strings.TrimSpace(notice))
	}
}

func ragMinScore() float64 {
	minScore := 0.030
	if v := os.Getenv("ANY_AGENTS_RAG_MIN"); v != "" {
		fmt.Sscanf(v, "%f", &minScore)
	}
	return minScore
}

// recallPick applies the injection gate: threshold, dataset allowlist
// (memories, config files, feedback notes and document bodies — property
// and name hits are noise, and the app's status page is a digest of
// everything else), unseen, capped. Another project's memory is injected
// only as a workflow rule (kind user or feedback); its project and
// reference memories describe that repo and are noise elsewhere. One body
// excerpt per space: excerpts are long and rarely acted on. Shared with
// eval so the simulation stays faithful.
func recallPick(c *Client, sp *Space, scope string, hits []SearchHit, minScore float64, seen map[string]bool, cap int) []SearchHit {
	var picked []SearchHit
	excerpts := 0
	for _, h := range hits {
		if h.Score < minScore || (seen != nil && seen[h.Dataset+"/"+h.RecordId]) {
			continue
		}
		if h.ObjectId == sp.owner("app") && h.Dataset == "editor_blocks" {
			continue
		}
		switch h.Dataset {
		case sp.coll(dsMemories):
			if strings.HasPrefix(strings.TrimSpace(h.Data), retractedTitle) {
				continue // a retracted shared memory
			}
			if !memoryTravels(c, sp, h, scope) {
				continue
			}
		case sp.coll(dsConfig), sp.coll(dsFeedback):
		case "editor_blocks":
			if excerpts == 1 {
				continue
			}
			excerpts++
		default:
			continue
		}
		picked = append(picked, h)
		if len(picked) == cap {
			break
		}
	}
	return picked
}

// memoryTravels reports whether a memory hit applies in a session of
// project scope: always for the project's own memories, for another
// project's only when it is a user or feedback memory. A record that
// cannot be read is kept.
func memoryTravels(c *Client, sp *Space, h SearchHit, scope string) bool {
	memScope, _, ok := strings.Cut(h.RecordId, ":")
	if !ok || scope == "" || memScope == scope {
		return true
	}
	rows, err := c.QueryRecords(sp.Id, h.ObjectId, h.Dataset, map[string]any{"id": h.RecordId}, nil, 1)
	if err != nil || len(rows) == 0 {
		return true
	}
	kind, _ := rows[0]["kind"].(string)
	return kind == "user" || kind == "feedback"
}

// hitLabel names a hit for a human: "memory any:foo" or "doc 'Title'".
func hitLabel(c *Client, sp *Space, h SearchHit) string {
	switch h.Dataset {
	case sp.coll(dsMemories):
		return "memory " + h.RecordId
	case sp.coll(dsConfig):
		return "config " + h.RecordId
	case sp.coll(dsFeedback):
		return "feedback " + h.RecordId
	}
	row, err := c.GetObject(sp.Id, h.ObjectId)
	if err != nil {
		return "object " + h.ObjectId
	}
	kind := "page"
	switch {
	case sp.isA(row, "session"):
		kind = "session"
	case sp.isA(row, "task"):
		kind = "task"
	case sp.isA(row, "doc"):
		kind = "doc"
	case sp.isA(row, "project"):
		kind = "project"
	case sp.has("collab") && sp.isA(row, "collab"):
		kind = "collab"
	}
	return fmt.Sprintf("%s %q %s", kind, nameOf(row), globalLink(sp.Id, h.ObjectId))
}

func ragSeenPath(sessionId string) string {
	return filepath.Join(stateDir(), "rag", shortId(sessionId)+".json")
}

func loadRagSeen(sessionId string) map[string]bool {
	seen := map[string]bool{}
	if sessionId == "" {
		return seen
	}
	if b, err := os.ReadFile(ragSeenPath(sessionId)); err == nil {
		_ = json.Unmarshal(b, &seen)
	}
	return seen
}

func saveRagSeen(sessionId string, seen map[string]bool) {
	if sessionId == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(ragSeenPath(sessionId)), 0o755)
	b, _ := json.Marshal(seen)
	_ = os.WriteFile(ragSeenPath(sessionId), b, 0o644)
}
