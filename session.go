package main

import (
	"bytes"
	"compress/gzip"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

// A session is one page filed under Session in the private space, with
// the scratch notes as its body, a transcript attachment, and relations
// to the project and machine. Docs created in the session point back
// through originSession; the session's docs are its backlinks. `session
// share` copies the page into a collab space.

func sessionById(c *Client, sp *Space, sid string) map[string]any {
	rows, err := c.QueryObjects(sp.Id, Query{
		Filter: bundleFilter(sp, "session", map[string]any{propPath(sp, "session", "sessionId"): sid}), Limit: 1})
	if err != nil || len(rows) == 0 {
		return nil
	}
	return rows[0]
}

// sessionStart creates or resumes the session object and returns the
// context note injected into the agent session.
func sessionStart(c *Client, st *State, tool Tool, sessionId, cwd, source string) (string, error) {
	sp := st.private()
	scope := resolveScope(cwd)
	projectId, err := ensureProject(c, sp, scope)
	if err != nil {
		return "", err
	}
	now := time.Now()
	row := sessionById(c, sp, sessionId)
	var objectId string
	if row != nil {
		objectId, _ = row["id"].(string)
		_ = c.SetProperties(sp.Id, objectId, sp.owner("session"), props(sp, "session", map[string]any{
			"status": []string{"active"}, "machine": linkList([]string{sp.MachineObjectId}),
		}))
		_ = c.MarkdownAppend(sp.Id, objectId, fmt.Sprintf("\nResumed %s (%s).\n", now.Format(time.RFC3339), source))
	} else {
		host, _ := os.Hostname()
		title := fmt.Sprintf("%s %s %s", scope, now.Format("2006-01-02 15:04"), shortId(sessionId))
		body := fmt.Sprintf("Started %s on %s in `%s` (session `%s`).\n", now.Format(time.RFC3339), host, cwd, sessionId)
		objectId, err = newObject(c, sp, pageType, []string{sp.owner("session")}, title, map[string]map[string]any{
			sp.owner("session"): props(sp, "session", map[string]any{
				"sessionId": sessionId, "cwd": cwd, "host": host, "status": []string{"active"}, "startedAt": dateVal(now),
				"tool": []string{tool.Name()}, "toolVersion": tool.Version(), "owner": []string{st.me()},
				"project": linkList([]string{projectId}), "machine": linkList([]string{sp.MachineObjectId}),
			}),
		}, body)
		if err != nil {
			return "", err
		}
	}
	st.LastSessionId = sessionId
	if err := st.save(); err != nil {
		return "", err
	}
	// Open tasks are not announced here on purpose: the agent would ask
	// about them on every start. They stay one `any-agents task list` away
	// and on the status page.
	var b strings.Builder
	n := len(pendingTasks(c, st, sp, projectId))
	fmt.Fprintf(&b, "any-agents: shared context is on (space %s, project %q, session object %s, %d open task(s) — only act on them when asked).\n", sp.Name, scope, globalLink(sp.Id, objectId), n)
	b.WriteString("Use it: `any-agents search \"<topic>\"` recalls memories, docs and past sessions; " +
		"`any-agents session note -` appends findings to this session's page; " +
		"`any-agents session update --summary \"…\" [--linear KEY]` sets the session summary and the issues it worked on; " +
		"`any-agents doc add <file.md|-> [--kind report|spec|note] [--linear KEY] [--space S]` saves a document for humans and other agents; " +
		"`any-agents task list|show|done` handles the queue; `any-agents plan add <taskId> <file|->` records a plan on a task; " +
		"`any-agents collab open|list|show|join|say|wait|done` asks other members' agents for help or helps them; " +
		"`any-agents feedback \"…\" --kind memory-wrong --ref <id>` reports a stale memory.\n")
	return b.String(), nil
}

func sessionEnd(c *Client, st *State, sessionId string) (string, error) {
	sp := st.private()
	row := sessionById(c, sp, sessionId)
	if row == nil {
		return "", fmt.Errorf("session %s not found", sessionId)
	}
	objectId, _ := row["id"].(string)
	vals := map[string]any{"status": []string{"ended"}, "endedAt": dateVal(time.Now())}
	if err := c.SetProperties(sp.Id, objectId, sp.owner("session"), props(sp, "session", vals)); err != nil {
		return objectId, err
	}
	syncSharedSession(c, st, row, vals)
	return objectId, nil
}

// syncSharedSession mirrors status, end time and summary onto the copy a
// `session share` made, when its space is set up on this machine.
func syncSharedSession(c *Client, st *State, row map[string]any, vals map[string]any) {
	sp := st.private()
	link := strOf(row, sp.owner("session"), sp.prop("session", "sharedTo"))
	if link == "" {
		return
	}
	to := st.space(parseSpaceId(link))
	if to == nil || !to.ready() {
		return
	}
	_ = c.SetProperties(to.Id, parseObjectId(link), to.owner("session"), props(to, "session", vals))
}

// archiveTranscript gzips the JSONL transcript and attaches it to the
// session object; the previous attachment (a resumed session) is dropped.
func archiveTranscript(c *Client, sp *Space, sessionId, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(data) > 200<<20 {
		return fmt.Errorf("transcript too large (%d bytes)", len(data))
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(data); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	row := sessionById(c, sp, sessionId)
	if row == nil {
		return fmt.Errorf("session %s not found", sessionId)
	}
	objectId, _ := row["id"].(string)
	fid, err := c.AttachFile(sp.Id, objectId, "transcript-"+shortId(sessionId)+".jsonl.gz", buf.Bytes())
	if err != nil {
		return err
	}
	prev := strOf(row, sp.owner("session"), sp.prop("session", "transcript"))
	if err := c.SetProperties(sp.Id, objectId, sp.owner("session"), props(sp, "session", map[string]any{
		"transcript": fileLink(sp.Id, fid),
	})); err != nil {
		return err
	}
	if old := fileIdOf(prev); old != "" && old != fid {
		_ = c.DeleteFile(sp.Id, old)
	}
	return nil
}

func fileLink(space, fileId string) string { return "any://f/" + space + "/" + fileId }

func fileIdOf(link string) string {
	if i := strings.LastIndex(link, "/"); i >= 0 && strings.HasPrefix(link, "any://f/") {
		return link[i+1:]
	}
	return ""
}

func cmdSession(args []string) {
	if len(args) == 0 {
		fatal(fmt.Errorf("usage: session note|show|update|list|transcript|share"))
	}
	c := NewClient()
	st := mustSetup(c)
	sp := st.private()
	fs, pos := parseArgs("session "+args[0], args[1:], func(fs *flag.FlagSet) {
		fs.String("s", "", "session id (default: the tool's session env var, then the last seen)")
		fs.String("summary", "", "session summary")
		fs.String("title", "", "session title")
		fs.String("linear", "", "Linear issue keys this session worked on, comma-separated")
		fs.String("project", "", "filter by project scope (list)")
		fs.String("o", "", "output file (transcript)")
		fs.Int("n", 20, "max rows (list)")
		fs.Bool("transcript", false, "share: attach the transcript to the copy too")
		spaceFlag(fs)
	})
	get := func(k string) string { return fs.Lookup(k).Value.String() }
	switch args[0] {
	case "list":
		lsp := spaceOf(st, get("space"))
		rows, err := objectsOf(c, lsp, "session", []string{"-" + propPath(lsp, "session", "startedAt")})
		if err != nil {
			fatal(err)
		}
		projects := nameIndex(c, lsp, "project")
		machines := nameIndex(c, lsp, "machine")
		n := 0
		for _, r := range rows {
			t := lsp.owner("session")
			proj := projects[firstLink(r, lsp, "session", "project")]
			if p := get("project"); p != "" && proj != p {
				continue
			}
			if n++; n > atoi(get("n")) {
				break
			}
			summary := strOf(r, t, lsp.prop("session", "summary"))
			if summary == "" {
				summary = nameOf(r)
			}
			on := machines[firstLink(r, lsp, "session", "machine")]
			if on == "" {
				on = strOf(r, t, lsp.prop("session", "host"))
			}
			fmt.Printf("%s  %s %-14s %-10s %-6s %s\n", globalLink(lsp.Id, r["id"].(string)),
				day(recordFloat(group(r, t), lsp.prop("session", "startedAt"))), pad(proj, 14),
				pad(on, 10), choiceOf(r, t, lsp.prop("session", "status")), trunc(summary, 80))
		}
		return
	}
	sid := resolveSession(st, get("s"))
	row := sessionById(c, sp, sid)
	if row == nil {
		fatal(fmt.Errorf("no session object for %q (pass -s <id>)", sid))
	}
	objectId, _ := row["id"].(string)
	switch args[0] {
	case "note", "append":
		text, err := textArg(pos)
		if err != nil {
			fatal(err)
		}
		if strings.TrimSpace(text) == "" {
			fatal(fmt.Errorf("session note <text|file|->"))
		}
		if err := c.MarkdownAppend(sp.Id, objectId, text); err != nil {
			fatal(err)
		}
	case "show":
		showObject(c, sp, objectId, false)
	case "update":
		if s := get("summary"); s != "" {
			if err := c.SetProperties(sp.Id, objectId, sp.owner("session"), props(sp, "session", map[string]any{"summary": s})); err != nil {
				fatal(err)
			}
			syncSharedSession(c, st, row, map[string]any{"summary": s})
		}
		if t := get("title"); t != "" {
			if err := c.SetProperties(sp.Id, objectId, "any", map[string]any{"name": t}); err != nil {
				fatal(err)
			}
		}
		if l := get("linear"); l != "" {
			links, err := linearLinks(c, sp, l)
			if err != nil {
				fatal(err)
			}
			noteSessionLinear(c, st, objectId, links, sp)
		}
	case "transcript":
		fid := fileIdOf(strOf(row, sp.owner("session"), sp.prop("session", "transcript")))
		if fid == "" {
			fatal(fmt.Errorf("no transcript archived for this session"))
		}
		gz, err := c.DownloadFile(sp.Id, fid)
		if err != nil {
			fatal(err)
		}
		zr, err := gzip.NewReader(bytes.NewReader(gz))
		if err != nil {
			fatal(err)
		}
		var out bytes.Buffer
		if _, err := out.ReadFrom(zr); err != nil {
			fatal(err)
		}
		if o := get("o"); o != "" {
			if err := os.WriteFile(o, out.Bytes(), 0o644); err != nil {
				fatal(err)
			}
			fmt.Printf("wrote %s (%d bytes)\n", o, out.Len())
		} else {
			os.Stdout.Write(out.Bytes())
		}
	case "share":
		if get("space") == "" {
			fatal(fmt.Errorf("session share needs --space <collab space>"))
		}
		to := spaceOf(st, get("space"))
		if to.Kind != kindCollab {
			fatal(fmt.Errorf("%s is the private space", to.Name))
		}
		id, err := shareSession(c, st, row, to, get("transcript") == "true")
		if err != nil {
			fatal(err)
		}
		fmt.Printf("%s  shared to %s\n", globalLink(to.Id, id), to.Name)
	default:
		fatal(fmt.Errorf("unknown session command %q", args[0]))
	}
}

// shareSession copies a private session page into a collab space: name,
// body, summary, tool, host, the project page re-resolved there, and
// optionally the transcript; the two pages link to each other.
func shareSession(c *Client, st *State, row map[string]any, to *Space, withTranscript bool) (string, error) {
	sp := st.private()
	t := sp.owner("session")
	srcId, _ := row["id"].(string)
	if existing := strOf(row, t, sp.prop("session", "sharedTo")); existing != "" && parseSpaceId(existing) == to.Id {
		return "", fmt.Errorf("already shared: %s", existing)
	}
	body, err := c.MarkdownGet(sp.Id, srcId)
	if err != nil {
		return "", err
	}
	// The copy carries no local paths: the working directory stays
	// private, and the generated header line loses it.
	if cwd := strOf(row, t, sp.prop("session", "cwd")); cwd != "" {
		body = strings.Replace(body, " in `"+cwd+"`", "", 1)
	}
	vals := map[string]any{
		"status":      []string{choiceOf(row, t, sp.prop("session", "status"))},
		"tool":        []string{choiceOf(row, t, sp.prop("session", "tool"))},
		"owner":       []string{st.me()},
		"summary":     strOf(row, t, sp.prop("session", "summary")),
		"host":        strOf(row, t, sp.prop("session", "host")),
		"toolVersion": strOf(row, t, sp.prop("session", "toolVersion")),
		"sessionId":   strOf(row, t, sp.prop("session", "sessionId")),
		"sharedFrom":  globalLink(sp.Id, srcId),
	}
	for _, k := range []string{"startedAt", "endedAt"} {
		if v, ok := group(row, t)[sp.prop("session", k)]; ok {
			vals[k] = v
		}
	}
	if pid := firstLink(row, sp, "session", "project"); pid != "" {
		if key := projectKey(c, sp, pid); key != "" {
			if id, err := ensureProject(c, to, key); err == nil {
				vals["project"] = linkList([]string{id})
			}
		}
	}
	id, err := newObject(c, to, pageType, []string{to.owner("session")}, nameOf(row),
		map[string]map[string]any{to.owner("session"): props(to, "session", vals)}, body)
	if err != nil {
		return "", err
	}
	if keys := linearKeys(c, sp); len(keys) > 0 {
		for _, lid := range linksOf(row, t, sp.prop("session", "linear")) {
			if key := keys[lid]; key != "" {
				if iid, err := ensureLinearIssue(c, to, key); err == nil {
					_ = addRelation(c, to, id, "session", "linear", iid)
				}
			}
		}
	}
	if withTranscript {
		if fid := fileIdOf(strOf(row, t, sp.prop("session", "transcript"))); fid != "" {
			if data, err := c.DownloadFile(sp.Id, fid); err == nil {
				if nf, err := c.AttachFile(to.Id, id, "transcript-"+shortId(vals["sessionId"].(string))+".jsonl.gz", data); err == nil {
					_ = c.SetProperties(to.Id, id, to.owner("session"), props(to, "session", map[string]any{"transcript": fileLink(to.Id, nf)}))
				}
			}
		}
	}
	_ = c.SetProperties(sp.Id, srcId, t, props(sp, "session", map[string]any{"sharedTo": globalLink(to.Id, id)}))
	return id, nil
}
