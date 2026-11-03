package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// A collab is a help request between agents: one object of type
// agent_collab in a collab space, whose body states the request and
// whose chat is the thread. The requester's agent opens it; helpers'
// agents join and talk; whoever finishes closes it. The bridge keeps its
// own per-collab cursor on the chat (never the account's read state, so
// the humans' unread badges stay intact).

var collabOpen = []string{"request", "in_progress"}

// resolveCollab turns <id|link> into the space and object id: a link
// names its space; otherwise --space; otherwise every collab space is tried.
func resolveCollab(c *Client, st *State, ref, spaceFlag string) (*Space, string, error) {
	id := parseObjectId(ref)
	if spaceId := parseSpaceId(ref); spaceId != "" && spaceId != ref {
		if sp := st.space(spaceId); sp != nil {
			return sp, id, nil
		}
		return nil, "", fmt.Errorf("space %s of %s is not set up here", spaceId, ref)
	}
	if spaceFlag != "" {
		sp, err := st.pick(spaceFlag)
		return sp, id, err
	}
	for _, sp := range st.collabs() {
		if _, err := c.GetObject(sp.Id, id); err == nil {
			return sp, id, nil
		}
	}
	return nil, "", fmt.Errorf("collab %s not found in any collab space (pass its link or --space)", ref)
}

func collabSpace(st *State, flag string) *Space {
	sp := spaceOf(st, flag)
	if !sp.has("collab") {
		fatal(fmt.Errorf("collabs are off in %s (the any server reserves the chat module)", sp.Name))
	}
	return sp
}

func cmdCollab(args []string) {
	if len(args) == 0 {
		fatal(fmt.Errorf("usage: collab open|list|show|join|say|wait|done|cancel|reopen"))
	}
	c := NewClient()
	st := mustSetup(c)
	switch args[0] {
	case "open":
		collabOpenCmd(c, st, args[1:])
	case "list":
		collabList(c, st, args[1:])
	case "show":
		fs, pos := parseArgs("collab show", args[1:], func(fs *flag.FlagSet) {
			spaceFlag(fs)
			fs.Int("n", 50, "messages to print")
		})
		if len(pos) < 1 {
			fatal(fmt.Errorf("collab show <id>"))
		}
		sp, id, err := resolveCollab(c, st, pos[0], fs.Lookup("space").Value.String())
		if err != nil {
			fatal(err)
		}
		if !sp.has("collab") {
			fatal(fmt.Errorf("collabs are off in %s on this machine — run `any-agents install` (after updating the any server if it predates chat on user types)", sp.Name))
		}
		collabShow(c, st, sp, id, atoi(fs.Lookup("n").Value.String()))
	case "join", "done", "cancel", "reopen", "say", "wait":
		fs, pos := parseArgs("collab "+args[0], args[1:], func(fs *flag.FlagSet) {
			spaceFlag(fs)
			fs.Duration("timeout", 90*time.Second, "how long wait blocks for a reply (exit 2 on timeout: run it again to keep waiting)")
			fs.Bool("all", false, "wait: return on any message, own ones included")
			fs.String("met", "", "done: how the definition of done was met; cancel/reopen: the reason (posted to the chat)")
		})
		if len(pos) < 1 {
			fatal(fmt.Errorf("collab %s <id>", args[0]))
		}
		sp, id, err := resolveCollab(c, st, pos[0], fs.Lookup("space").Value.String())
		if err != nil {
			fatal(err)
		}
		if !sp.has("collab") {
			fatal(fmt.Errorf("collabs are off in %s on this machine — run `any-agents install` (after updating the any server if it predates chat on user types)", sp.Name))
		}
		switch args[0] {
		case "join":
			if err := collabJoin(c, st, sp, id); err != nil {
				fatal(err)
			}
			_ = renderStatusPage(c, sp)
		case "reopen":
			// A closed request comes back as in_progress (request when
			// nobody has joined); the requester or a helper may do it.
			row, err := c.GetObject(sp.Id, id)
			if err != nil {
				fatal(err)
			}
			t := sp.owner("collab")
			requester := firstMember(row, t, sp.prop("collab", "requester"))
			assignees := membersOf(row, t, sp.prop("collab", "assignees"))
			if requester != st.me() && !contains(assignees, st.me()) {
				fatal(fmt.Errorf("only the requester or a helper reopens a collab"))
			}
			status := "in_progress"
			if len(assignees) == 0 {
				status = "request"
			}
			pp := map[string]any{"status": []string{status}, "resolvedAt": nil}
			if err := c.SetProperties(sp.Id, id, t, props(sp, "collab", pp)); err != nil {
				fatal(err)
			}
			if met := strings.TrimSpace(fs.Lookup("met").Value.String()); met != "" {
				if err := collabSay(c, st, sp, id, "Reopened: "+met); err != nil {
					fatal(err)
				}
			}
			_ = renderStatusPage(c, sp)
			fmt.Printf("%s  %s\n", globalLink(sp.Id, id), status)
		case "done", "cancel":
			// The requester closes: the helper reports in the chat and
			// leaves the request open.
			row, err := c.GetObject(sp.Id, id)
			if err != nil {
				fatal(err)
			}
			t := sp.owner("collab")
			if requester := firstMember(row, t, sp.prop("collab", "requester")); requester != "" && requester != st.me() {
				fatal(fmt.Errorf("only the requester (%s) closes a collab — say in its chat that your part is done", memberIndex(c, sp)[identityOf(requester)]))
			}
			status := map[string]string{"done": "done", "cancel": "canceled"}[args[0]]
			met := strings.TrimSpace(fs.Lookup("met").Value.String())
			if args[0] == "done" {
				if met == "" {
					dod := strOf(row, t, sp.prop("collab", "doneWhen"))
					if dod != "" {
						dod = " (done when: " + dod + ")"
					}
					fatal(fmt.Errorf("collab done needs --met \"<how the definition of done was met>\"%s", dod))
				}
				if err := collabSay(c, st, sp, id, "Done: "+met); err != nil {
					fatal(err)
				}
			} else if met != "" {
				if err := collabSay(c, st, sp, id, "Canceled: "+met); err != nil {
					fatal(err)
				}
			}
			pp := map[string]any{"status": []string{status}, "resolvedAt": dateVal(time.Now())}
			if err := c.SetProperties(sp.Id, id, sp.owner("collab"), props(sp, "collab", pp)); err != nil {
				fatal(err)
			}
			_ = renderStatusPage(c, sp)
			fmt.Printf("%s  %s\n", globalLink(sp.Id, id), status)
		case "say":
			text, err := textArg(pos[1:])
			if err != nil {
				fatal(err)
			}
			if strings.TrimSpace(text) == "" {
				fatal(fmt.Errorf("collab say <id> <text|file|->"))
			}
			if err := collabSay(c, st, sp, id, text); err != nil {
				fatal(err)
			}
			fmt.Fprintf(os.Stderr, "any-agents: posted; now `any-agents collab wait %s` for the reply (exit 2 = none yet, run it again) until the requester closes the collab\n", globalLink(sp.Id, id))
		case "wait":
			timeout, _ := time.ParseDuration(fs.Lookup("timeout").Value.String())
			msgs, closedAs, err := collabWait(c, st, sp, id, timeout, fs.Lookup("all").Value.String() == "true")
			if err != nil {
				fatal(err)
			}
			if closedAs != "" {
				fmt.Printf("collab %s — nothing more to wait for\n", closedAs)
				return
			}
			if len(msgs) == 0 {
				fmt.Fprintln(os.Stderr, "any-agents: no reply within the timeout")
				os.Exit(2)
			}
			printMessages(msgs, memberIndex(c, sp))
		}
	default:
		fatal(fmt.Errorf("unknown collab command %q", args[0]))
	}
}

// collabOpenCmd files a request: body from the file, status request,
// requester = me, optional assignees, the converged project page of the
// cwd repo, Linear keys, and a link back to the private session.
func collabOpenCmd(c *Client, st *State, args []string) {
	fs, pos := parseArgs("collab open", args, func(fs *flag.FlagSet) {
		spaceFlag(fs)
		fs.String("title", "", "object name (default: first H1)")
		fs.String("to", "", "members to assign, comma-separated names or identities")
		fs.String("project", "", "project scope (default: the cwd repo)")
		fs.String("linear", "", "Linear issue keys, comma-separated")
		fs.String("related", "", "comma-separated object ids or links to relate")
		fs.String("pr", "", "GitHub pull request URL (default: the first PR link in the body)")
		fs.String("done-when", "", "definition of done (default: the request's '## Done when' section)")
		fs.Bool("announce", false, "also post the request with its link into the space's general chat")
		fs.String("s", "", "session id (default: the tool's session env var)")
	})
	get := func(k string) string { return fs.Lookup(k).Value.String() }
	if len(pos) < 1 {
		fatal(fmt.Errorf("collab open <file.md|-> --space S"))
	}
	if get("space") == "" {
		fatal(fmt.Errorf("collab open needs --space <collab space>"))
	}
	sp := collabSpace(st, get("space"))
	if sp.Kind != kindCollab {
		fatal(fmt.Errorf("%s is the private space — open a collab in a shared space", sp.Name))
	}
	data, err := readSource(pos[0])
	if err != nil {
		fatal(err)
	}
	body := string(data)
	name := titleOf(get("title"), body, "")
	if name == "" {
		fatal(fmt.Errorf("pass --title or start the request with a '# Heading'"))
	}
	dod := firstNonEmpty(strings.TrimSpace(get("done-when")), doneWhenOf(body))
	if dod == "" {
		fatal(fmt.Errorf("collab open needs a definition of done: --done-when \"…\" or a '## Done when' section in the request"))
	}
	ci := currentContext(c, st, sp, get("s"), get("project"))
	cp := map[string]any{"status": []string{"request"}, "requester": []string{st.me()}, "date": dateVal(time.Now()), "doneWhen": dod}
	if ci.projectId != "" {
		cp["project"] = linkList([]string{ci.projectId})
	}
	if ci.sessionId != "" {
		cp["originSession"] = globalLink(st.Private, ci.sessionId)
	}
	if pr := firstNonEmpty(get("pr"), prLink(body)); pr != "" {
		cp["pr"] = pr
	}
	if v := get("to"); v != "" {
		links, err := resolveMembers(c, sp, v)
		if err != nil {
			fatal(err)
		}
		cp["assignees"] = links
	}
	if v := get("linear"); v != "" {
		links, err := linearLinks(c, sp, v)
		if err != nil {
			fatal(err)
		}
		cp["linear"] = links
	}
	if v := get("related"); v != "" {
		ids, err := sameSpaceIds(sp, v)
		if err != nil {
			fatal(err)
		}
		cp["related"] = linkList(ids)
	}
	id, err := newObject(c, sp, sp.owner("collab"), nil, name, map[string]map[string]any{sp.owner("collab"): props(sp, "collab", cp)}, body)
	if err != nil {
		if isCode(err, "type.reserved_carrier") {
			fatal(fmt.Errorf("this any server predates chat on user types: it can join and talk in collabs but not open them — update the server (%v)", err))
		}
		fatal(err)
	}
	if ci.sessionId != "" {
		_ = c.MarkdownAppend(sp.st.private().Id, ci.sessionId, fmt.Sprintf("\nOpened collab [%s](%s) in %s.\n", name, globalLink(sp.Id, id), sp.Name))
	}
	saveCursor(id, "")
	_ = renderStatusPage(c, sp)
	if get("announce") == "true" {
		if err := announceCollab(c, st, sp, id, name); err != nil {
			fmt.Fprintf(os.Stderr, "any-agents: announce: %v\n", err)
		}
	}
	fmt.Printf("%s  %s\n", globalLink(sp.Id, id), name)
}

// announceCollab posts the request's title and link into the space's
// general chat so members can ask their agents to join.
func announceCollab(c *Client, st *State, sp *Space, id, name string) error {
	res, err := c.CatalogSetup(sp.Id, "general-chat")
	if err != nil {
		return err
	}
	if len(res) == 0 || res[0].Bundle.RootId == "" {
		return fmt.Errorf("no general chat in %s", sp.Name)
	}
	text := fmt.Sprintf("Help wanted: %s\n%s\n`any-agents collab join %s`", name, globalLink(sp.Id, id), globalLink(sp.Id, id))
	_, err = c.ChatSend(sp.Id, res[0].Bundle.RootId, text, agentEnvelope(c, st, sp))
	return err
}

func collabList(c *Client, st *State, args []string) {
	fs, _ := parseArgs("collab list", args, func(fs *flag.FlagSet) {
		spaceFlag(fs)
		fs.Bool("mine", false, "only collabs I requested or joined")
		fs.String("status", "", "filter: request | in_progress | done | canceled (default: open ones)")
	})
	get := func(k string) string { return fs.Lookup(k).Value.String() }
	spaces := st.collabs()
	if s := get("space"); s != "" {
		spaces = []*Space{collabSpace(st, s)}
	}
	for _, sp := range spaces {
		if !sp.has("collab") {
			continue
		}
		statuses := collabOpen
		if s := get("status"); s != "" {
			statuses = []string{s}
		}
		rows, err := objectsOf(c, sp, "collab", []string{"-" + propPath(sp, "collab", "date")},
			map[string]any{propPath(sp, "collab", "status"): map[string]any{"$in": statuses}})
		if err != nil {
			fatal(err)
		}
		members := memberIndex(c, sp)
		projects := nameIndex(c, sp, "project")
		t := sp.owner("collab")
		for _, r := range rows {
			requester := firstMember(r, t, sp.prop("collab", "requester"))
			assignees := membersOf(r, t, sp.prop("collab", "assignees"))
			if get("mine") == "true" && requester != st.me() && !contains(assignees, st.me()) {
				continue
			}
			var names []string
			for _, a := range assignees {
				names = append(names, members[identityOf(a)])
			}
			id, _ := r["id"].(string)
			fresh := ""
			if n := newMessageCount(c, st, sp, id, 10); n > 0 {
				fresh = fmt.Sprintf(" +%d new", n)
			}
			fmt.Printf("%s  %-11s %-10s %-12s %s%s", globalLink(sp.Id, id), choiceOf(r, t, sp.prop("collab", "status")),
				pad(members[identityOf(requester)], 10), pad(projects[firstLink(r, sp, "collab", "project")], 12), nameOf(r), fresh)
			if pr := strOf(r, t, sp.prop("collab", "pr")); pr != "" {
				fmt.Printf("  %s", prShort(pr))
			}
			if len(names) > 0 {
				fmt.Printf("  [%s]", strings.Join(names, ", "))
			}
			fmt.Printf("  (%s)\n", sp.Name)
		}
	}
}

func collabShow(c *Client, st *State, sp *Space, id string, n int) {
	row, err := c.GetObject(sp.Id, id)
	if err != nil {
		fatal(err)
	}
	t := sp.owner("collab")
	members := memberIndex(c, sp)
	var names []string
	for _, a := range membersOf(row, t, sp.prop("collab", "assignees")) {
		names = append(names, members[identityOf(a)])
	}
	fmt.Printf("# %s\n%s\nstatus=%s  requester=%s  assignees=%s  space=%s\n", nameOf(row), globalLink(sp.Id, id),
		choiceOf(row, t, sp.prop("collab", "status")), members[identityOf(firstMember(row, t, sp.prop("collab", "requester")))],
		strings.Join(names, ","), sp.Name)
	if pr := strOf(row, t, sp.prop("collab", "pr")); pr != "" {
		fmt.Printf("pr=%s\n", pr)
	}
	if dod := strOf(row, t, sp.prop("collab", "doneWhen")); dod != "" {
		fmt.Printf("done when: %s\n", dod)
	}
	body, err := c.MarkdownGet(sp.Id, id)
	if err != nil {
		fatal(err)
	}
	fmt.Println()
	fmt.Println(strings.TrimSpace(body))
	msgs, err := chatTail(c, sp, id, n)
	if err != nil {
		fatal(err)
	}
	fmt.Println("\n## Chat")
	if len(msgs) == 0 {
		fmt.Println("(no messages yet)")
	}
	printMessages(msgs, members)
	if len(msgs) > 0 {
		saveCursor(id, verOf(msgs[len(msgs)-1]))
	}
}

// collabJoin adds me to the assignees and moves the request to in_progress.
func collabJoin(c *Client, st *State, sp *Space, id string) error {
	row, err := c.GetObject(sp.Id, id)
	if err != nil {
		return err
	}
	t := sp.owner("collab")
	cur := membersOf(row, t, sp.prop("collab", "assignees"))
	pp := map[string]any{"assignees": addToLinks(cur, st.me())}
	if choiceOf(row, t, sp.prop("collab", "status")) == "request" {
		pp["status"] = []string{"in_progress"}
	}
	if err := c.SetProperties(sp.Id, id, t, props(sp, "collab", pp)); err != nil {
		return err
	}
	// On the first join everything before it counts as read: wait returns
	// only what follows. A repeated join keeps the cursor.
	if !contains(cur, st.me()) {
		if msgs, err := chatTail(c, sp, id, 1); err == nil && len(msgs) > 0 {
			saveCursor(id, verOf(msgs[0]))
		}
	}
	fmt.Printf("%s  joined\n", globalLink(sp.Id, id))
	return nil
}

// collabSay posts one message as this account's agent. The cursor stays:
// others' messages that arrived before this one are still unseen, and
// wait skips own messages anyway.
func collabSay(c *Client, st *State, sp *Space, id, text string) error {
	if _, err := c.ChatSend(sp.Id, id, text, agentEnvelope(c, st, sp)); err != nil {
		return err
	}
	fmt.Printf("%s  sent\n", globalLink(sp.Id, id))
	return nil
}

// collabWait polls the chat until a message newer than the cursor
// arrives (by default one that is not mine), or the timeout passes.
func collabWait(c *Client, st *State, sp *Space, id string, timeout time.Duration, all bool) ([]map[string]any, string, error) {
	deadline := time.Now().Add(timeout)
	closed := func() string {
		row, err := c.GetObject(sp.Id, id)
		if err != nil {
			return ""
		}
		switch s := choiceOf(row, sp.owner("collab"), sp.prop("collab", "status")); s {
		case "done", "canceled":
			return s
		}
		return ""
	}
	if _, ok := loadCursor(id); !ok {
		// No cursor on this machine: the backlog is `collab show`'s job;
		// wait starts from now. A collab opened here has an empty cursor
		// and every reply counts.
		if msgs, err := chatTail(c, sp, id, 1); err == nil && len(msgs) > 0 {
			saveCursor(id, verOf(msgs[0]))
		} else {
			saveCursor(id, "")
		}
	}
	for {
		cursor, _ := loadCursor(id)
		msgs, err := chatSince(c, sp, id, cursor, 50)
		if err != nil {
			return nil, "", err
		}
		var out []map[string]any
		for _, m := range msgs {
			if all || !ownAgentMessage(m, st) {
				out = append(out, m)
			}
		}
		if len(msgs) > 0 {
			saveCursor(id, verOf(msgs[len(msgs)-1]))
		}
		if len(out) > 0 {
			return out, "", nil
		}
		if s := closed(); s != "" {
			return nil, s, nil
		}
		if time.Now().After(deadline) {
			return nil, "", nil
		}
		time.Sleep(2 * time.Second)
	}
}

// collabDigest is the session-start note on collab spaces: open requests
// others filed, and my collabs with messages I have not seen.
func collabDigest(c *Client, st *State) string {
	var b strings.Builder
	for _, sp := range st.collabs() {
		if err := ensureSpaceSetup(c, sp); err != nil {
			fmt.Fprintf(os.Stderr, "any-agents: collab space %s: %v\n", sp.Name, err)
			continue
		}
		if !sp.has("collab") {
			continue
		}
		rows, err := objectsOf(c, sp, "collab", []string{"-" + propPath(sp, "collab", "date")},
			map[string]any{propPath(sp, "collab", "status"): map[string]any{"$in": collabOpen}})
		if err != nil {
			continue
		}
		t := sp.owner("collab")
		var requests, forMe, fresh, replies []string
		for _, r := range rows {
			id, _ := r["id"].(string)
			requester := firstMember(r, t, sp.prop("collab", "requester"))
			assignees := membersOf(r, t, sp.prop("collab", "assignees"))
			mine := requester == st.me() || contains(assignees, st.me())
			status := choiceOf(r, t, sp.prop("collab", "status"))
			if status == "request" && requester != st.me() && contains(assignees, st.me()) && len(forMe) < 5 {
				item := fmt.Sprintf("%q %s", nameOf(r), globalLink(sp.Id, id))
				if dod := strOf(r, t, sp.prop("collab", "doneWhen")); dod != "" {
					item += " (done when: " + trunc(dod, 160) + ")"
				}
				forMe = append(forMe, item)
			}
			if status == "request" && !mine && len(requests) < 5 {
				requests = append(requests, fmt.Sprintf("%q %s", nameOf(r), globalLink(sp.Id, id)))
			}
			if mine && len(fresh)+len(replies) < 10 {
				if n := newMessageCount(c, st, sp, id, 10); n > 0 {
					item := fmt.Sprintf("%q +%d %s", nameOf(r), n, globalLink(sp.Id, id))
					if requester == st.me() {
						if dod := strOf(r, t, sp.prop("collab", "doneWhen")); dod != "" {
							item += " (done when: " + trunc(dod, 160) + ")"
						}
						replies = append(replies, item)
					} else {
						fresh = append(fresh, item)
					}
				}
			}
		}
		if len(forMe) > 0 {
			fmt.Fprintf(&b, "Collab space %s has %d help request(s) addressed to you: %s — `any-agents collab show <link>` reads one, `collab join` takes it; report in its chat and leave it open, the requester closes it.\n", sp.Name, len(forMe), strings.Join(forMe, "; "))
		}
		if len(replies) > 0 {
			fmt.Fprintf(&b, "Replies on your request(s) in %s: %s — read them with `collab show`; answer with `collab say`; close with `any-agents collab done <link> --met \"<evidence>\"` only once the definition of done is met, or when the user says so.\n", sp.Name, strings.Join(replies, "; "))
		}
		if len(requests) > 0 {
			fmt.Fprintf(&b, "Collab space %s has %d open help request(s) nobody took: %s — `any-agents collab show <link>` reads one, `collab join` takes it.\n", sp.Name, len(requests), strings.Join(requests, "; "))
		}
		if len(fresh) > 0 {
			fmt.Fprintf(&b, "New messages on your collabs in %s: %s — `any-agents collab wait <link>` reads them.\n", sp.Name, strings.Join(fresh, "; "))
		}
	}
	return b.String()
}

// newMessageCount counts messages after the cursor that are not mine
// (capped at limit).
func newMessageCount(c *Client, st *State, sp *Space, id string, limit int) int {
	cursor, _ := loadCursor(id)
	msgs, err := chatSince(c, sp, id, cursor, limit)
	if err != nil {
		return 0
	}
	n := 0
	for _, m := range msgs {
		if !ownAgentMessage(m, st) {
			n++
		}
	}
	return n
}

// ownAgentMessage is a message this machine's agent (or another of the
// account's agents) wrote: own identity with the agent envelope. The
// account's human typing from a phone has no envelope and counts as new.
func ownAgentMessage(m map[string]any, st *State) bool {
	if creatorOf(m) != st.Identity {
		return false
	}
	_, agent := m["agent"].(map[string]any)
	return agent
}

// --- chat reads ---

// chatSince reads messages after a version cursor, oldest first.
func chatSince(c *Client, sp *Space, objectId, cursor string, limit int) ([]map[string]any, error) {
	var filter map[string]any
	if cursor != "" {
		filter = map[string]any{"_ver.id": map[string]any{"$gt": cursor}}
	}
	return c.QueryRecords(sp.Id, objectId, "chat_messages", filter, []string{"_ver.id"}, limit)
}

// chatTail reads the newest n messages, oldest first.
func chatTail(c *Client, sp *Space, objectId string, n int) ([]map[string]any, error) {
	rows, err := c.QueryRecords(sp.Id, objectId, "chat_messages", nil, []string{"-_ver.id"}, n)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows, nil
}

func verOf(m map[string]any) string {
	v, _ := m["_ver"].(map[string]any)
	id, _ := v["id"].(string)
	return id
}

func creatorOf(m map[string]any) string {
	s, _ := m["creator"].(string)
	return s
}

func printMessages(msgs []map[string]any, members map[string]string) {
	for _, m := range msgs {
		who := members[creatorOf(m)]
		if who == "" {
			who = shortId(creatorOf(m))
		}
		if a, ok := m["agent"].(map[string]any); ok {
			if n, _ := a["name"].(string); n != "" {
				who = n
			}
		}
		text, _ := m["text"].(string)
		at := time.Unix(int64(recordFloat(m, "createdAt")), 0).Format("01-02 15:04")
		fmt.Printf("[%s] %s: %s\n", at, who, strings.TrimSpace(text))
	}
}

// agentEnvelope marks a chat message as written by this account's agent;
// the owner's own devices then count it unread, as any's chat contract
// says for agent-authored messages.
func agentEnvelope(c *Client, st *State, sp *Space) map[string]any {
	name := currentTool().Label()
	if me, err := c.MemberMe(sp.Id); err == nil && me.Name != "" {
		name += " (" + me.Name + ")"
	} else {
		name += " (" + shortId(st.Identity) + ")"
	}
	return map[string]any{"name": name, "done": true}
}

// --- members ---

// memberIndex maps identity → display name for a space.
func memberIndex(c *Client, sp *Space) map[string]string {
	out := map[string]string{}
	members, err := c.Members(sp.Id)
	if err != nil {
		return out
	}
	for _, m := range members {
		name := m.Name
		if name == "" {
			name = shortId(m.Identity)
		}
		out[m.Identity] = name
	}
	return out
}

// resolveMembers turns comma-separated names or identities into member
// links, matched against the space's roster.
func resolveMembers(c *Client, sp *Space, csv string) ([]string, error) {
	members, err := c.Members(sp.Id)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, s := range strings.Split(csv, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		want := identityOf(s)
		found := ""
		for _, m := range members {
			if m.Identity == want || strings.EqualFold(m.Name, s) {
				found = m.Identity
				break
			}
		}
		if found == "" {
			return nil, fmt.Errorf("%q is not a member of %s", s, sp.Name)
		}
		out = addToLinks(out, identityLink(found))
	}
	return out, nil
}

// identityOf strips the any://u/ prefix.
func identityOf(link string) string { return strings.TrimPrefix(link, "any://u/") }

// membersOf reads a member property's identity links.
func membersOf(row map[string]any, ownerId, propId string) []string {
	return linksOf(row, ownerId, propId)
}

func firstMember(row map[string]any, ownerId, propId string) string {
	if ids := membersOf(row, ownerId, propId); len(ids) > 0 {
		return ids[0]
	}
	return ""
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// --- cursors ---

func cursorPath(objectId string) string {
	return filepath.Join(stateDir(), "collab", objectId+".json")
}

// loadCursor returns the last seen message version and whether this
// machine has a cursor at all: an empty cursor that exists means
// "nothing seen yet" (the collab was just opened here), no cursor means
// another machine's collab.
func loadCursor(objectId string) (string, bool) {
	var cur struct {
		Last string `json:"last"`
	}
	b, err := os.ReadFile(cursorPath(objectId))
	if err != nil {
		return "", false
	}
	_ = json.Unmarshal(b, &cur)
	return cur.Last, true
}

func saveCursor(objectId, last string) {
	_ = os.MkdirAll(filepath.Dir(cursorPath(objectId)), 0o755)
	b, _ := json.Marshal(map[string]string{"last": last})
	_ = os.WriteFile(cursorPath(objectId), b, 0o644)
}

// doneWhenHeading matches a "Done when" / "Definition of done" / "DoD"
// heading in a request body.
var doneWhenHeading = regexp.MustCompile(`(?mi)^#{1,6}\s*(done when|definition of done|dod)\s*:?\s*$`)

// doneWhenOf is the paragraph under the request's definition-of-done
// heading, joined into one line, or "".
func doneWhenOf(body string) string {
	loc := doneWhenHeading.FindStringIndex(body)
	if loc == nil {
		return ""
	}
	var lines []string
	for _, l := range strings.Split(body[loc[1]:], "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "#") {
			break
		}
		if l == "" {
			if len(lines) > 0 {
				break
			}
			continue
		}
		lines = append(lines, strings.TrimLeft(l, "-* "))
	}
	return trunc(strings.Join(lines, " "), 600)
}
