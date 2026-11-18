package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// A collab is a help request between agents: one object of type
// agent_collab in a collab space, whose body states the request and
// whose chat is the thread. The requester's agent opens it; helpers'
// agents join and talk; whoever finishes closes it. The bridge keeps its
// own cursor on the chat per collab and session (never the account's
// read state, so the humans' unread badges stay intact).

var collabOpen = []string{"request", "in_progress"}

// resolveCollab turns <id|link> into the space and object id: a link
// names its space; otherwise --space; otherwise every collab space is tried.
func resolveCollab(c *Client, st *State, ref, spaceFlag string) (*Space, string, error) {
	id := parseObjectId(ref)
	if spaceId := parseSpaceId(ref); spaceId != "" && spaceId != ref {
		if sp := st.space(spaceId); sp != nil {
			catchUp(st, sp)
			return sp, id, nil
		}
		return nil, "", fmt.Errorf("space %s of %s is not set up here", spaceId, ref)
	}
	if spaceFlag != "" {
		sp, err := st.pick(spaceFlag)
		if err != nil {
			return nil, "", err
		}
		catchUp(st, sp)
		return sp, id, nil
	}
	for _, sp := range st.collabs() {
		if _, err := c.GetObject(sp.Id, id); err == nil {
			catchUp(st, sp)
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
		warnHeldElsewhere(id)
		markEngaged(id)
		collabShow(c, st, sp, id, atoi(fs.Lookup("n").Value.String()))
	case "join", "done", "cancel", "reopen", "say", "wait":
		fs, pos := parseArgs("collab "+args[0], args[1:], func(fs *flag.FlagSet) {
			spaceFlag(fs)
			fs.Duration("timeout", 90*time.Second, "how long wait blocks for a reply; the default fits a foreground command, use 60m in a background one (exit 2 on timeout: run it again)")
			fs.Bool("all", false, "wait: return on any message, own ones included")
			fs.String("met", "", "done: how the definition of done was met; cancel/reopen: the reason (posted to the chat)")
			fs.String("user", "", "done/cancel: close at once on the user's decision, quoting them; nobody else's agreement is needed")
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
		warnHeldElsewhere(id)
		markEngaged(id)
		switch args[0] {
		case "join":
			if err := collabJoin(c, st, sp, id); err != nil {
				fatal(err)
			}
			_ = renderStatusPage(c, sp)
		case "reopen":
			// A closed request comes back as in_progress (request when
			// nobody has joined) and the agreements to close are dropped;
			// the requester or a helper may do it.
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
			pp := map[string]any{"status": []string{status}, "resolvedAt": nil, "agreed": nil}
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
			met := strings.TrimSpace(fs.Lookup("met").Value.String())
			user := strings.TrimSpace(fs.Lookup("user").Value.String())
			if err := collabClose(c, st, sp, id, args[0], met, user); err != nil {
				fatal(err)
			}
		case "say":
			text, err := textArg(pos[1:])
			if err != nil {
				fatal(err)
			}
			if strings.TrimSpace(text) == "" {
				fatal(fmt.Errorf("collab say <id> <text|file|->"))
			}
			// Talking in a collab makes me a participant whose agreement
			// the close needs.
			row, err := c.GetObject(sp.Id, id)
			if err != nil {
				fatal(err)
			}
			if !contains(collabParties(sp, row), st.me()) {
				if err := collabJoin(c, st, sp, id); err != nil {
					fatal(err)
				}
			}
			if err := collabSay(c, st, sp, id, text); err != nil {
				fatal(err)
			}
			fmt.Fprintf(os.Stderr, "any-agents: posted; now wait for the reply with `any-agents collab wait %s --timeout 60m` in the background (a foreground command is cut at the harness's limit; exit 2 = none yet, run it again) until the collab closes; once your part is done, `collab done <link> --met \"<evidence>\"` records your agreement\n", globalLink(sp.Id, id))
		case "wait":
			timeout, _ := time.ParseDuration(fs.Lookup("timeout").Value.String())
			msgs, closedAs, err := collabWait(c, st, sp, id, timeout, fs.Lookup("all").Value.String() == "true")
			var down *serverDownError
			if errors.As(err, &down) {
				fmt.Fprintf(os.Stderr, "any-agents: %v; the Any app is not running or not unlocked: do not rerun the wait in a loop, tell the user and rerun only once they say it is back\n", down)
				os.Exit(2)
			}
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
			printMessages(msgs, memberIndex(c, sp), st.Identity)
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
		ids, err := sameSpaceIds(c, sp, v)
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
	saveCursor(id, currentSessionId(), "")
	markEngaged(id)
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
	_, err = c.ChatSend(sp.Id, res[0].Bundle.RootId, text, agentEnvelope(c, st, sp), linkAttachment(globalLink(sp.Id, id)))
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
			if n := newMessageCount(c, st, sp, id, currentSessionId(), 10); n > 0 {
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
			if agreed := membersOf(r, t, sp.prop("collab", "agreed")); len(agreed) > 0 && contains(collabOpen, choiceOf(r, t, sp.prop("collab", "status"))) {
				fmt.Printf("  closing, waiting for %s", strings.Join(memberNames(members, collabWaiting(sp, r, agreed)), ", "))
			}
			if heldElsewhere(id, currentSessionId()) {
				fmt.Printf("  (another session of yours)")
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
	if agreed := membersOf(row, t, sp.prop("collab", "agreed")); len(agreed) > 0 {
		fmt.Printf("agreed to close: %s", strings.Join(memberNames(members, agreed), ","))
		if waiting := collabWaiting(sp, row, agreed); len(waiting) > 0 {
			fmt.Printf("  waiting for: %s", strings.Join(memberNames(members, waiting), ","))
		}
		fmt.Println()
	}
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
	printMessages(msgs, members, st.Identity)
	// An empty chat saves an empty cursor: what is posted after the show
	// counts, even once the session joins.
	saveCursor(id, currentSessionId(), lastVer(msgs))
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
	// A session that read the collab keeps its cursor, so nothing posted
	// since `collab show` is lost; on a machine that never read it the
	// cursor starts here. Taken first: a failed read leaves no half join.
	if err := ensureCursor(c, st, sp, id, currentSessionId()); err != nil {
		return err
	}
	if err := c.SetProperties(sp.Id, id, t, props(sp, "collab", pp)); err != nil {
		return err
	}
	fmt.Printf("%s  joined\n", globalLink(sp.Id, id))
	return nil
}

// collabSay posts one message as this account's agent and records it as
// this session's. The cursor stays: others' messages that arrived before
// this one are still unseen, and wait skips the session's own messages.
// A first word on a machine that never read the collab starts the
// cursor here, so a reply that lands before the wait starts is caught.
// The send is recorded before it goes out, so a wait polling meanwhile
// (or after a send that failed once the server took it) knows it.
func collabSay(c *Client, st *State, sp *Space, id, text string) error {
	sid := currentSessionId()
	if err := ensureCursor(c, st, sp, id, sid); err != nil {
		return err
	}
	key := textKey(text)
	recordPosted(id, sid, key, strconv.FormatInt(time.Now().Unix(), 10))
	res, err := c.ChatSend(sp.Id, id, text, agentEnvelope(c, st, sp))
	if err != nil {
		return err
	}
	if len(res.RecordIds) > 0 {
		recordPosted(id, sid, res.RecordIds[0], key)
	}
	fmt.Printf("%s  sent\n", globalLink(sp.Id, id))
	return nil
}

// collabClose records my agreement to close the collab as done or
// canceled, with the evidence posted to the chat, and closes it once every
// participant (the requester and each assignee) agreed. A user's decision
// (--user, quoting them) closes it at once: a human may end a collab
// alone, an agent may not.
func collabClose(c *Client, st *State, sp *Space, id, verb, met, user string) error {
	row, err := c.GetObject(sp.Id, id)
	if err != nil {
		return err
	}
	t := sp.owner("collab")
	if cur := choiceOf(row, t, sp.prop("collab", "status")); cur == "done" || cur == "canceled" {
		return fmt.Errorf("collab is already %s (`collab reopen` undoes that)", cur)
	}
	status := map[string]string{"done": "done", "cancel": "canceled"}[verb]
	label := map[string]string{"done": "Done", "cancel": "Canceled"}[verb]
	link := globalLink(sp.Id, id)
	if user != "" {
		if err := collabSay(c, st, sp, id, label+" on the user's decision: "+user); err != nil {
			return err
		}
		return closeCollab(c, sp, id, status, nil)
	}
	if met == "" {
		hint := ""
		if verb == "done" {
			if dod := strOf(row, t, sp.prop("collab", "doneWhen")); dod != "" {
				hint = " (done when: " + dod + ")"
			}
			return fmt.Errorf("collab done needs --met \"<how the definition of done was met>\"%s — or --user \"<their words>\" when your user decided to close it", hint)
		}
		return fmt.Errorf("collab cancel needs --met \"<why>\" — or --user \"<their words>\" when your user decided to close it")
	}
	if sp.prop("collab", "agreed") == "" {
		return fmt.Errorf("this machine's setup of %s predates collab votes: run `any-agents install`, then vote again", sp.Name)
	}
	parties := collabParties(sp, row)
	if !contains(parties, st.me()) {
		return fmt.Errorf("you are not part of this collab: `collab join %s` first — or --user \"<their words>\" when your user decided to close it", link)
	}
	prior := membersOf(row, t, sp.prop("collab", "agreed"))
	if contains(prior, st.me()) {
		// Another session of this member (or an earlier vote) agreed: a
		// second vote message would only wake the member's other sessions.
		if waiting := collabWaiting(sp, row, prior); len(waiting) > 0 {
			fmt.Printf("%s  already agreed; closes once %s agreed too\n", link, strings.Join(memberNames(memberIndex(c, sp), waiting), ", "))
			return nil
		}
	}
	agreed := addToLinks(prior, st.me())
	waiting := collabWaiting(sp, row, agreed)
	text := label + ": " + met
	if len(waiting) > 0 {
		names := memberNames(memberIndex(c, sp), waiting)
		verbs := "agrees"
		if len(names) > 1 {
			verbs = "agree"
		}
		text += fmt.Sprintf("\nCloses once %s %s: `any-agents collab %s %s --met \"…\"` — or say what is still open.", strings.Join(names, ", "), verbs, verb, link)
	}
	if err := collabSay(c, st, sp, id, text); err != nil {
		return err
	}
	if len(waiting) == 0 {
		return closeCollab(c, sp, id, status, agreed)
	}
	if err := c.SetProperties(sp.Id, id, t, props(sp, "collab", map[string]any{"agreed": agreed})); err != nil {
		return err
	}
	_ = renderStatusPage(c, sp)
	fmt.Printf("%s  agreed; closes once %s agreed too — keep `collab wait <link> --timeout 60m` running in the background\n", link, strings.Join(memberNames(memberIndex(c, sp), waiting), ", "))
	return nil
}

func closeCollab(c *Client, sp *Space, id, status string, agreed []string) error {
	pp := map[string]any{"status": []string{status}, "resolvedAt": dateVal(time.Now())}
	if agreed != nil {
		pp["agreed"] = agreed
	}
	if err := c.SetProperties(sp.Id, id, sp.owner("collab"), props(sp, "collab", pp)); err != nil {
		return err
	}
	_ = renderStatusPage(c, sp)
	fmt.Printf("%s  %s\n", globalLink(sp.Id, id), status)
	return nil
}

// collabParties is everyone whose agreement a close needs: the requester
// and each assignee.
func collabParties(sp *Space, row map[string]any) []string {
	t := sp.owner("collab")
	parties := membersOf(row, t, sp.prop("collab", "assignees"))
	if r := firstMember(row, t, sp.prop("collab", "requester")); r != "" {
		parties = addToLinks(parties, r)
	}
	return parties
}

// collabWaiting is the parties not in agreed.
func collabWaiting(sp *Space, row map[string]any, agreed []string) []string {
	var out []string
	for _, p := range collabParties(sp, row) {
		if !contains(agreed, p) {
			out = append(out, p)
		}
	}
	return out
}

// memberNames maps identity links to member names (the identity when
// the member is unknown).
func memberNames(members map[string]string, links []string) []string {
	var out []string
	for _, l := range links {
		if n := members[identityOf(l)]; n != "" {
			out = append(out, n)
		} else {
			out = append(out, identityOf(l))
		}
	}
	return out
}

// maxOutage bounds one continuous stretch of unavailable server a wait
// retries through.
var maxOutage = 30 * time.Second

// pollTimeout bounds one request of a wait.
var pollTimeout = 15 * time.Second

// serverDownError: the server stayed unavailable until the wait's timeout.
type serverDownError struct{ last error }

func (e *serverDownError) Error() string {
	return "server unavailable: " + e.last.Error()
}

// rediscoverAddr finds where my own account's server now listens, "" to
// keep the current address: an environment or pinned address is
// authoritative, and a server of another account is never taken.
func rediscoverAddr(st *State) string {
	if os.Getenv("ANY_AGENTS_ADDR") != "" || st.Addr != "" || st.Identity == "" {
		return ""
	}
	if pin, err := readPin(); err != nil || pin != "" {
		return ""
	}
	for _, s := range discoverServers(anyDataRoot(), 500*time.Millisecond) {
		if s.Account == st.Identity {
			return s.Addr
		}
	}
	return ""
}

// collabWait polls the chat until a message newer than the session's
// cursor arrives (by default one this session did not write: the
// member's other sessions count as others), or the timeout passes.
func collabWait(c *Client, st *State, sp *Space, id string, timeout time.Duration, all bool) ([]map[string]any, string, error) {
	deadline := time.Now().Add(timeout)
	sid := currentSessionId()
	closed := func(c *Client) string {
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
	// No cursor of its own: the session copies where the machine is; on a
	// machine that never read the collab the backlog is `collab show`'s
	// job, so wait starts from now and names what it skipped. A collab
	// opened here has an empty cursor and every reply counts. Taken inside
	// the loop so a server that is down at the start is waited out instead
	// of recording an empty cursor.
	_, haveCursor := readCursor(cursorPath(id, sid))
	var cursor string
	cursorRead := false
	// A restarting server (network error, 502-504, 401 auth.required) is
	// waited out silently: exiting would wake the agent for nothing. Backoff
	// 2s doubling to 30s with jitter, reset on success. The outage is given
	// maxOutage in all (and never past the deadline): a restart is over by
	// then, a longer one means the app is closed and the agent should know.
	// The attempt after the last sleep still runs; only its failure gives up.
	var downSince time.Time
	delay := 2 * time.Second
	down := func(err error) error {
		if downSince.IsZero() {
			downSince = time.Now()
		}
		giveUp := downSince.Add(maxOutage)
		if deadline.Before(giveUp) {
			giveUp = deadline
		}
		left := time.Until(giveUp)
		if left <= 0 {
			return &serverDownError{err}
		}
		sleep := delay + time.Duration(rand.Int63n(int64(delay)/5+1))
		if sleep > left {
			sleep = left
		}
		time.Sleep(sleep)
		if delay *= 2; delay > 30*time.Second {
			delay = 30 * time.Second
		}
		return nil
	}
	// fail handles a transport failure: false when err is not transient.
	fail := func(err error) (bool, error) {
		if !transient(err) {
			return false, nil
		}
		if _, isAPI := err.(*apiError); !isAPI {
			// The app may have come back on another port, but only the
			// server of my own account will do.
			if addr := rediscoverAddr(st); addr != "" {
				c.base = "http://" + addr
			}
		}
		return true, down(err)
	}
	for {
		// One poll never outlives the wait or a stuck server: a hung app
		// (not a restart) would otherwise hold a request for the 90s client
		// timeout.
		pollFor := pollTimeout
		if left := time.Until(deadline); left < pollFor {
			pollFor = left
		}
		if pollFor < time.Second {
			pollFor = time.Second
		}
		pc := *c
		pc.hc = &http.Client{Transport: c.hc.Transport, Timeout: pollFor}
		if !haveCursor {
			if err := ensureCursor(&pc, st, sp, id, sid); err != nil {
				retry, derr := fail(err)
				if !retry {
					return nil, "", err
				}
				if derr != nil {
					return nil, "", derr
				}
				continue
			}
			haveCursor = true
		}
		if !cursorRead {
			// Read once: another wait of this session that advances the
			// cursor must not hide the message from this one.
			cursor, _ = loadCursor(id, sid)
			cursorRead = true
		}
		msgs, err := chatSince(&pc, sp, id, cursor, 50)
		if err != nil {
			retry, derr := fail(err)
			if !retry {
				return nil, "", err
			}
			if derr != nil {
				return nil, "", derr
			}
			continue
		}
		if !downSince.IsZero() {
			// Back up: resume only once it is verified to be my account. The
			// chat answer is dropped and fetched again after the check.
			acc, aerr := pc.Account()
			if aerr != nil {
				retry, derr := fail(aerr)
				if !retry {
					return nil, "", aerr
				}
				if derr != nil {
					return nil, "", derr
				}
				continue
			}
			if st.Identity != "" && acc.Id != st.Identity {
				return nil, "", fmt.Errorf("the server now runs another account (%s, expected %s); not resuming", shortId(acc.Id), shortId(st.Identity))
			}
			downSince = time.Time{}
			delay = 2 * time.Second
			continue
		}
		out := msgs
		if !all {
			out = unseen(msgs, ownPostsOf(id, sid), st.Identity)
		}
		if len(msgs) > 0 {
			cursor = lastVer(msgs)
			if saved, _ := loadCursor(id, sid); cursor > saved {
				saveCursor(id, sid, cursor)
			}
		}
		if len(out) > 0 {
			return out, "", nil
		}
		if s := closed(&pc); s != "" {
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
func collabDigest(c *Client, st *State, sessionId string) string {
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
		var requests, forMe, fresh, replies, held []string
		for _, r := range rows {
			id, _ := r["id"].(string)
			requester := firstMember(r, t, sp.prop("collab", "requester"))
			assignees := membersOf(r, t, sp.prop("collab", "assignees"))
			mine := requester == st.me() || contains(assignees, st.me())
			if heldElsewhere(id, sessionId) {
				if len(held) < 5 {
					held = append(held, fmt.Sprintf("%q %s", nameOf(r), globalLink(sp.Id, id)))
				}
				continue
			}
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
				if n := newMessageCount(c, st, sp, id, sessionId, 10); n > 0 {
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
			fmt.Fprintf(&b, "Collab space %s has %d help request(s) addressed to you: %s — `any-agents collab show <link>` reads one, `collab join` takes it; report in its chat; when your part is done, `any-agents collab done <link> --met \"<evidence>\"` records your agreement, and the collab closes once every participant agreed or a user decides (`--user \"<their words>\"`). On your own: answer questions, check bugs, debug, run tests, review. A new feature, an API or architecture change, or a decision that binds the team: ask your user first and say in the chat that you are checking.\n", sp.Name, len(forMe), strings.Join(forMe, "; "))
		}
		if len(replies) > 0 {
			fmt.Fprintf(&b, "Replies on your request(s) in %s: %s — read them with `collab show`; answer with `collab say`; once the definition of done holds, `any-agents collab done <link> --met \"<evidence>\"` records your agreement, and the collab closes once every participant agreed; `--user \"<their words>\"` closes it at once when your user decides.\n", sp.Name, strings.Join(replies, "; "))
		}
		if len(requests) > 0 {
			fmt.Fprintf(&b, "Collab space %s has %d open help request(s) nobody took: %s — `any-agents collab show <link>` reads one, `collab join` takes it.\n", sp.Name, len(requests), strings.Join(requests, "; "))
		}
		if len(held) > 0 {
			fmt.Fprintf(&b, "Collab(s) in %s another session of yours already handles: %s — leave them to it; if that session is gone, `any-agents collab show <link>` takes one over.\n", sp.Name, strings.Join(held, "; "))
		}
		if len(fresh) > 0 {
			fmt.Fprintf(&b, "New messages on your collabs in %s: %s — `any-agents collab wait <link>` reads them; then keep `collab wait <link> --timeout 60m` running in the background.\n", sp.Name, strings.Join(fresh, "; "))
		}
	}
	return b.String()
}

// newMessageCount counts the messages after the session's cursor that it
// did not write itself (capped at limit).
func newMessageCount(c *Client, st *State, sp *Space, id, sid string, limit int) int {
	cursor, _ := loadCursor(id, sid)
	// Read past the session's own messages: a run of them must not hide a
	// reply behind the limit.
	msgs, err := chatSince(c, sp, id, cursor, 200)
	if err != nil {
		return 0
	}
	return min(len(unseen(msgs, ownPostsOf(id, sid), st.Identity)), limit)
}

func msgIdOf(m map[string]any) string {
	s, _ := m["id"].(string)
	return s
}

// --- chat reads ---

// chatSince reads messages after a version cursor, oldest first.
func chatSince(c *Client, sp *Space, objectId, cursor string, limit int) ([]map[string]any, error) {
	var filter map[string]any
	if cursor != "" {
		// _ver.id is the creation version; an edit moves _ver.text only.
		filter = map[string]any{"$or": []any{
			map[string]any{"_ver.id": map[string]any{"$gt": cursor}},
			map[string]any{"_ver.text": map[string]any{"$gt": cursor}},
		}}
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

// printMessages prints one message per line start, tagged with the
// signer's id: the member name and the agent label are free text anyone
// can set, the signer is not. Free text cannot fake a line or a tag:
// names lose newlines and brackets, and a message's further lines are
// indented. The tag is the short id unless another identity in sight
// (a member, a signer, me) shares it, then the full id.
func printMessages(msgs []map[string]any, members map[string]string, me string) {
	known := map[string]bool{me: true}
	for id := range members {
		known[id] = true
	}
	for _, m := range msgs {
		known[creatorOf(m)] = true
	}
	tag := func(id string) string {
		for other := range known {
			if other != id && shortId(other) == shortId(id) {
				return "[" + id + "]"
			}
		}
		return "[" + shortId(id) + "]"
	}
	for _, m := range msgs {
		who := plainName(members[creatorOf(m)])
		if a, ok := m["agent"].(map[string]any); ok {
			n, _ := a["name"].(string)
			if who = plainName(n); who == "" {
				who = "agent" // never printed like the member typing
			}
		}
		if who != "" {
			who += " "
		}
		who += tag(creatorOf(m))
		text, _ := m["text"].(string)
		text = plainText(text)
		at := time.Unix(int64(recordFloat(m, "createdAt")), 0).Format("01-02 15:04")
		if maxVerOf(m) != verOf(m) {
			at += ", edited"
		}
		fmt.Printf("[%s] %s: %s\n", at, who, strings.ReplaceAll(text, "\n", "\n    "))
	}
}

// plainName keeps a free-text name on one line and clear of the tag's
// brackets and the line's colon.
func plainName(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '[' || r == ']' || r == ':':
			return -1
		case unicode.IsControl(r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r):
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// plainText turns every line break (\r, \v, \f, NEL, U+2028/9) into \n
// and drops other control and format runes (terminal escapes, bidi
// overrides), so a message cannot draw a line of its own.
func plainText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r == '\r' || r == '\v' || r == '\f' || r == 0x85 || r == 0x2028 || r == 0x2029:
			return '\n'
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(s)
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

// Each harness session keeps its own cursor per collab, so one session
// reading a collab never hides a message from another. A call outside a
// session writes the collab's plain cursor.
func cursorPath(objectId, sid string) string {
	name := objectId
	if sid != "" {
		name += "." + fileSafe(sid)
	}
	return filepath.Join(stateDir(), "collab", name+".json")
}

func readCursor(path string) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var cur struct {
		Last string `json:"last"`
	}
	_ = json.Unmarshal(b, &cur)
	return cur.Last, true
}

// loadCursor returns the last message version the session saw and
// whether a cursor exists at all: an empty cursor that exists means
// "nothing seen yet" (the collab was just opened here), no cursor means
// a collab this machine never read. A session without a cursor of its
// own (a new one, or one that took over) and a call outside a session
// start from the newest cursor on this machine, where the member is.
func loadCursor(objectId, sid string) (string, bool) {
	if sid != "" {
		if v, ok := readCursor(cursorPath(objectId, sid)); ok {
			return v, true
		}
	}
	paths, _ := filepath.Glob(filepath.Join(stateDir(), "collab", objectId+".*.json"))
	paths = append(paths, cursorPath(objectId, ""))
	best, found := "", false
	for _, p := range paths {
		if v, ok := readCursor(p); ok {
			if !found || v > best {
				best = v
			}
			found = true
		}
	}
	return best, found
}

// ensureCursor gives a session that takes part in a collab a cursor of
// its own: a copy of where the machine is (so another session's reads no
// longer move it), or on a machine that never read the collab a seed.
func ensureCursor(c *Client, st *State, sp *Space, id, sid string) error {
	if _, own := readCursor(cursorPath(id, sid)); own {
		return nil
	}
	if v, ok := loadCursor(id, sid); ok {
		saveCursor(id, sid, v)
		return nil
	}
	return seedCursor(c, st, sp, id, sid)
}

// seedCursor starts the cursor of a collab this machine never read at
// the chat's newest message. Every message counts for the agent (its
// user may be steering it from the app), so the skipped ones are named
// with the command that prints them.
func seedCursor(c *Client, st *State, sp *Space, id, sid string) error {
	msgs, err := chatTail(c, sp, id, 50)
	if err != nil {
		return err
	}
	saveCursor(id, sid, lastVer(msgs))
	if n := len(unseen(msgs, ownPostsOf(id, sid), st.Identity)); n > 0 {
		count := fmt.Sprint(n)
		if len(msgs) == 50 {
			count += "+"
		}
		fmt.Fprintf(os.Stderr, "any-agents: this machine had not read this collab: %s earlier message(s) are not shown here — `any-agents collab show %s` prints them\n", count, globalLink(sp.Id, id))
	}
	return nil
}

func saveCursor(objectId, sid, last string) {
	p := cursorPath(objectId, sid)
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	b, _ := json.Marshal(map[string]string{"last": last})
	_ = writePrivateFile(p, b) // a rename: a concurrent reader never sees half a cursor
}

// Posted messages: what each session wrote, one tab-separated line per
// event — "<session> #<text key> <unix time>" before a send, "<session>
// <message id> #<text key>" once the server returned the id. The chat's agent envelope
// names the account, not the session, so this is how a session tells its
// own messages from those of the member's other sessions.
func postedPath(objectId string) string {
	return filepath.Join(stateDir(), "collab", objectId+".posted")
}

func recordPosted(objectId, sid string, fields ...string) {
	appendLine(postedPath(objectId), strings.Join(append([]string{sid}, fields...), "\t"))
}

// textKey identifies a message by its text while its id is unknown.
func textKey(text string) string {
	sum := sha256.Sum256([]byte(text))
	return "#" + hex.EncodeToString(sum[:12])
}

// ownPosts is what one session wrote in a collab: message ids, and the
// text keys and times of sends whose id is not recorded (in flight, or a
// send that failed, maybe after the server took it).
type ownPosts struct {
	ids     map[string]bool
	pending map[string][]int64
}

// pendingWindow bounds how long after an unrecorded send a message of
// the same text counts as that send: a send that never landed must not
// hide a later message for good.
const pendingWindow = 5 * 60

// ownPostsOf reads the session's posts; outside a session (no id) every
// post of this machine's sessions counts as own.
func ownPostsOf(objectId, sid string) ownPosts {
	o := ownPosts{ids: map[string]bool{}, pending: map[string][]int64{}}
	b, err := os.ReadFile(postedPath(objectId))
	if err != nil {
		return o
	}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Split(l, "\t")
		if len(f) < 2 || sid != "" && f[0] != sid {
			continue
		}
		switch {
		case strings.HasPrefix(f[1], "#"):
			var at int64
			if len(f) > 2 {
				at, _ = strconv.ParseInt(f[2], 10, 64)
			}
			o.pending[f[1]] = append(o.pending[f[1]], at)
		default:
			o.ids[f[1]] = true
			if len(f) > 2 && len(o.pending[f[2]]) > 0 {
				o.pending[f[2]] = o.pending[f[2]][1:]
			}
		}
	}
	return o
}

// wrote reports whether the session wrote m: by id, or for a send still
// unrecorded by its text and time, which only an agent message of this
// account can match.
func (o ownPosts) wrote(m map[string]any, identity string) bool {
	if o.ids[msgIdOf(m)] {
		return true
	}
	if creatorOf(m) != identity {
		return false
	}
	if _, agent := m["agent"].(map[string]any); !agent {
		return false
	}
	text, _ := m["text"].(string)
	created := int64(recordFloat(m, "createdAt"))
	for _, at := range o.pending[textKey(text)] {
		if created >= at-30 && created <= at+pendingWindow {
			return true
		}
	}
	return false
}

// unseen drops the messages the session wrote itself.
func unseen(msgs []map[string]any, own ownPosts, identity string) []map[string]any {
	var out []map[string]any
	for _, m := range msgs {
		if !own.wrote(m, identity) {
			out = append(out, m)
		}
	}
	return out
}

// fileSafe keeps a session id usable as part of a file name.
func fileSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			return r
		}
		return '_'
	}, s)
}

// Engagement: the harness sessions of this member that took part in a
// collab (opened, joined, read or wrote it). The Stop and prompt hooks
// nag only those sessions; another session of the same member stays
// out of a collab a sibling is handling.
func engagedPath(objectId string) string {
	return filepath.Join(stateDir(), "collab", objectId+".sessions")
}

func markEngaged(objectId string) {
	sid := os.Getenv(currentTool().SessionEnv())
	if sid == "" || engaged(objectId, sid) {
		return
	}
	appendLine(engagedPath(objectId), sid)
}

// appendLine adds one line to a state file (O_APPEND: safe across
// concurrent calls).
func appendLine(path, line string) {
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = f.WriteString(line + "\n")
	_ = f.Close()
}

// engaged reports whether a session took part in the collab; no session
// (a plain CLI call) counts as engaged everywhere.
func engaged(objectId, sid string) bool {
	if sid == "" {
		return true
	}
	b, err := os.ReadFile(engagedPath(objectId))
	if err != nil {
		return false
	}
	for _, l := range strings.Fields(string(b)) {
		if l == sid {
			return true
		}
	}
	return false
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

// maxVerOf is the newest version of a message: its creation version or,
// after an edit, the version of its text.
func maxVerOf(m map[string]any) string {
	v := verOf(m)
	if ver, ok := m["_ver"].(map[string]any); ok {
		if t, _ := ver["text"].(string); t > v {
			v = t
		}
	}
	return v
}

// lastVer is the newest version among messages, the cursor to save.
func lastVer(msgs []map[string]any) string {
	last := ""
	for _, m := range msgs {
		if v := maxVerOf(m); v > last {
			last = v
		}
	}
	return last
}

func currentSessionId() string { return os.Getenv(currentTool().SessionEnv()) }

// engagedSessions lists the harness sessions of this member that took
// part in a collab.
func engagedSessions(objectId string) []string {
	b, err := os.ReadFile(engagedPath(objectId))
	if err != nil {
		return nil
	}
	return strings.Fields(string(b))
}

// heldElsewhere reports whether another session of this member took part
// in the collab and this one did not: a collab belongs to the session
// that joined or answered it.
func heldElsewhere(objectId, sid string) bool {
	if sid == "" {
		return false
	}
	others := engagedSessions(objectId)
	return len(others) > 0 && !contains(others, sid)
}

func warnHeldElsewhere(objectId string) {
	if heldElsewhere(objectId, currentSessionId()) {
		fmt.Fprintln(os.Stderr, "any-agents: another session of yours already took part in this collab; carry on only if that session is gone")
	}
}
