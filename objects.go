package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Objects: every doc, session, project, machine and Linear issue is a
// built-in `page` filed under its collection; a task or plan is a Doc
// also filed under Task / Plan. A collab is the one typed object (its
// type adds a chat part).

// typeFilter selects the live objects of a type, optionally AND-ed with
// extra conditions.
func typeFilter(typeId string, extra ...map[string]any) map[string]any {
	return scopeFilter(map[string]any{"any.type": typeId}, extra)
}

// collectionFilter selects the live members of a collection.
func collectionFilter(collectionId string, extra ...map[string]any) map[string]any {
	return scopeFilter(map[string]any{"any.collections": collectionId}, extra)
}

func scopeFilter(scope map[string]any, extra []map[string]any) map[string]any {
	and := []any{scope, map[string]any{"any.collections": map[string]any{"$nin": []string{"bin"}}}}
	for _, e := range extra {
		and = append(and, e)
	}
	return map[string]any{"$and": and}
}

// bundleFilter scopes a query to a bundle's objects: by membership for
// the collection bundles, by type otherwise.
func bundleFilter(sp *Space, bundle string, extra ...map[string]any) map[string]any {
	if collectionBundles[bundle] {
		return collectionFilter(sp.owner(bundle), extra...)
	}
	return typeFilter(sp.owner(bundle), extra...)
}

// linkAttachment is a chat attachment that opens a page or a URL.
func linkAttachment(link string) map[string]any {
	return map[string]any{"type": "link", "link": link}
}

func propPath(sp *Space, bundle, xkey string) string {
	return sp.owner(bundle) + "." + sp.prop(bundle, xkey)
}

// objectsOf lists a bundle's objects, newest first unless sorted otherwise.
func objectsOf(c *Client, sp *Space, bundle string, sortKeys []string, extra ...map[string]any) ([]map[string]any, error) {
	if sortKeys == nil {
		sortKeys = []string{"-modifiedAt"}
	}
	return c.QueryObjectsAll(sp.Id, Query{Filter: bundleFilter(sp, bundle, extra...), Sort: sortKeys})
}

// newObject creates an object of one type, filed under collections, with
// name, property groups (keyed by owner) and body.
func newObject(c *Client, sp *Space, typeId string, collections []string, name string, groups map[string]map[string]any, body string) (string, error) {
	initial := map[string]any{"any": map[string]any{"name": name}}
	for t, g := range groups {
		if len(g) > 0 {
			initial[t] = g
		}
	}
	id, err := c.CreateObject(sp.Id, typeId, collections, initial)
	if err != nil {
		return "", err
	}
	if body != "" {
		if err := putBody(c, sp, id, name, body); err != nil {
			return id, err
		}
	}
	return id, nil
}

// nameIndex maps object id → name for one bundle (for listings).
func nameIndex(c *Client, sp *Space, bundle string) map[string]string {
	out := map[string]string{}
	rows, err := c.QueryObjects(sp.Id, Query{Filter: bundleFilter(sp, bundle), Limit: 1000,
		Projection: map[string]any{"any": 1}})
	if err != nil {
		return out
	}
	for _, r := range rows {
		id, _ := r["id"].(string)
		out[id] = nameOf(r)
	}
	return out
}

func firstLink(row map[string]any, sp *Space, bundle, xkey string) string {
	ids := linksOf(row, sp.owner(bundle), sp.prop(bundle, xkey))
	if len(ids) > 0 {
		return ids[0]
	}
	return ""
}

// findByName returns an existing object of a bundle with this name.
func findByName(c *Client, sp *Space, bundle, name string) (map[string]any, error) {
	rows, err := c.QueryObjects(sp.Id, Query{Filter: bundleFilter(sp, bundle, map[string]any{"any.name": name}), Limit: 2})
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}

// currentContext resolves the project page in the target space and the
// session object (private space) for a command run from inside an agent
// session (best effort, never fatal).
type ctxInfo struct {
	scope     string
	projectId string
	sessionId string // session object id, in the private space
}

func currentContext(c *Client, st *State, sp *Space, sessionFlag, projectFlag string) ctxInfo {
	var ci ctxInfo
	cwd, _ := os.Getwd()
	ci.scope = projectFlag
	if sid, exact := resolveSession(st, sessionFlag); sid != "" {
		if row := sessionById(c, st.private(), sid); row != nil {
			scope := ""
			if pid := firstLink(row, st.private(), "session", "project"); pid != "" {
				scope = projectKey(c, st.private(), pid)
			}
			// The last-seen session may belong to another agent session on
			// this machine: trust it only when it works in the cwd's repo.
			if exact || scope == "" || scope == resolveScope(cwd) {
				ci.sessionId, _ = row["id"].(string)
				if ci.scope == "" {
					ci.scope = scope
				}
			}
		}
	}
	if ci.scope == "" {
		ci.scope = resolveScope(cwd)
	}
	ci.projectId, _ = ensureProject(c, sp, ci.scope)
	return ci
}

// projectKey is a project page's scope (its key, which a renamed page
// keeps), falling back to the name.
func projectKey(c *Client, sp *Space, projectId string) string {
	p, err := c.GetObject(sp.Id, projectId)
	if err != nil {
		return ""
	}
	if k := strOf(p, sp.owner("project"), sp.prop("project", "key")); k != "" {
		return k
	}
	return nameOf(p)
}

// sessionLink is the private session's global link, or "".
func sessionLink(st *State, ci ctxInfo) string {
	if ci.sessionId == "" {
		return ""
	}
	return globalLink(st.Private, ci.sessionId)
}

// --- doc ---

func cmdDoc(args []string) {
	if len(args) == 0 {
		fatal(fmt.Errorf("usage: doc add|list|show|append|edit|archive|link"))
	}
	c := NewClient()
	st := mustSetup(c)
	space, args := extractSpace(args)
	sp := spaceOf(st, space)
	switch args[0] {
	case "add", "save":
		docAdd(c, st, sp, args[1:], nil)
	case "list":
		docList(c, sp, args[1:])
	case "show":
		if len(args) < 2 {
			fatal(fmt.Errorf("doc show <id>"))
		}
		sp, id := objectIn(st, sp, args[1])
		showObject(c, sp, id, false)
	case "append":
		if len(args) < 3 {
			fatal(fmt.Errorf("doc append <id> <text|file|->"))
		}
		body, err := textArg(args[2:])
		if err != nil {
			fatal(err)
		}
		sp, id := objectIn(st, sp, args[1])
		if err := c.MarkdownAppend(sp.Id, id, body); err != nil {
			fatal(err)
		}
	case "edit":
		fs, pos := parseArgs("doc edit", args[1:], func(fs *flag.FlagSet) {
			fs.String("old", "", "text to replace (must be unique)")
			fs.String("new", "", "replacement")
		})
		if len(pos) < 1 || fs.Lookup("old").Value.String() == "" {
			fatal(fmt.Errorf("doc edit <id> --old S --new S"))
		}
		sp, id := objectIn(st, sp, pos[0])
		if err := c.MarkdownPatch(sp.Id, id, []MarkdownEdit{{
			OldText: fs.Lookup("old").Value.String(), NewText: fs.Lookup("new").Value.String()}}); err != nil {
			fatal(err)
		}
	case "archive":
		if len(args) < 2 {
			fatal(fmt.Errorf("doc archive <id>"))
		}
		sp, id := objectIn(st, sp, args[1])
		if err := c.SetProperties(sp.Id, id, sp.owner("doc"), props(sp, "doc", map[string]any{"archived": true})); err != nil {
			fatal(err)
		}
	case "link":
		if len(args) < 3 {
			fatal(fmt.Errorf("doc link <id> <objectId>"))
		}
		sp, id := objectIn(st, sp, args[1])
		targets, err := sameSpaceIds(sp, args[2])
		if err != nil || len(targets) != 1 {
			fatal(fmt.Errorf("doc link <id> <objectId in the same space>: %v", err))
		}
		if err := addRelation(c, sp, id, "doc", "related", targets[0]); err != nil {
			fatal(err)
		}
	default:
		fatal(fmt.Errorf("unknown doc command %q", args[0]))
	}
}

// extractSpace pulls --space out of a subcommand's args so the space is
// known before the subcommand parses its own flags.
func extractSpace(args []string) (string, []string) {
	var rest []string
	space := ""
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--space" || args[i] == "-space":
			if i+1 < len(args) {
				space = args[i+1]
				i++
			}
		case strings.HasPrefix(args[i], "--space=") || strings.HasPrefix(args[i], "-space="):
			space = args[i][strings.Index(args[i], "=")+1:]
		default:
			rest = append(rest, args[i])
		}
	}
	return space, rest
}

// sameSpaceIds parses comma-separated ids or links that must live in sp:
// relations never cross spaces.
func sameSpaceIds(sp *Space, csv string) ([]string, error) {
	var ids []string
	for _, ref := range strings.Split(csv, ",") {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		if spaceId := parseSpaceId(ref); spaceId != ref && spaceId != sp.Id {
			return nil, fmt.Errorf("%s is in another space; a relation cannot cross spaces (put the link in the body instead)", ref)
		}
		ids = append(ids, parseObjectId(ref))
	}
	return ids, nil
}

// objectIn resolves an id argument: a link names its space, a bare id
// is in the given one.
func objectIn(st *State, sp *Space, ref string) (*Space, string) {
	if spaceId := parseSpaceId(ref); spaceId != ref {
		if other := st.space(spaceId); other != nil {
			return other, parseObjectId(ref)
		}
		fatal(fmt.Errorf("space %s of %s is not set up here", spaceId, ref))
	}
	return sp, parseObjectId(ref)
}

// addRelation appends target to a relation property (read-modify-write).
func addRelation(c *Client, sp *Space, objectId, bundle, xkey, target string) error {
	row, err := c.GetObject(sp.Id, objectId)
	if err != nil {
		return err
	}
	cur := linksOf(row, sp.owner(bundle), sp.prop(bundle, xkey))
	return c.SetProperties(sp.Id, objectId, sp.owner(bundle),
		map[string]any{sp.prop(bundle, xkey): addToLinks(cur, target)})
}

// docAdd files (or re-saves by title) a document. taskProps lets task
// add reuse it, filing the doc under Task.
func docAdd(c *Client, st *State, sp *Space, args []string, taskProps map[string]any) {
	fs, pos := parseArgs("doc add", args, func(fs *flag.FlagSet) {
		fs.String("title", "", "object name (default: first H1, else file name)")
		fs.String("kind", "", "note | report | spec | retro | prompt | plan | other")
		fs.String("project", "", "project scope (default: the session's project, else the cwd repo)")
		fs.String("linear", "", "Linear issue keys, comma-separated (ABC-123)")
		fs.String("related", "", "comma-separated object ids or any:// links to relate")
		fs.String("pr", "", "GitHub pull request URL (default: the first PR link in the body)")
		fs.String("priority", "normal", "task priority: low | normal | high (task add only)")
		fs.String("assignee", "", "task assignee: a member name or identity (task add only)")
		fs.String("target", "", "task target machine hostname (task add only)")
		fs.Bool("new", false, "always create, even when a doc with this title exists")
		fs.String("s", "", "session id (default: the tool's session env var)")
	})
	get := func(k string) string { return fs.Lookup(k).Value.String() }
	if len(pos) < 1 {
		fatal(fmt.Errorf("doc add <file.md|->"))
	}
	src := pos[0]
	data, err := readSource(src)
	if err != nil {
		fatal(err)
	}
	isText := src == "-" || map[string]bool{".md": true, ".markdown": true, ".txt": true}[strings.ToLower(filepath.Ext(src))]
	body := ""
	if isText {
		body = string(data)
	}
	name := titleOf(get("title"), body, filepath.Base(src))
	if src == "-" && get("title") == "" && !mdHeading.MatchString(body) {
		fatal(fmt.Errorf("pass --title or start the document with a '# Heading'"))
	}
	ci := currentContext(c, st, sp, get("s"), get("project"))
	kind := get("kind")
	if kind == "" {
		kind = "note"
		if taskProps != nil {
			kind = "prompt"
		}
	}
	docP := map[string]any{"kind": []string{kind}, "date": dateVal(time.Now()), "author": []string{st.me()}}
	if ci.projectId != "" {
		docP["project"] = linkList([]string{ci.projectId})
	}
	if ci.sessionId != "" && sp.Kind == kindPrivate {
		docP["originSession"] = linkList([]string{ci.sessionId})
	}
	if pr := firstNonEmpty(get("pr"), prLink(body)); pr != "" {
		docP["pr"] = pr
	}
	if v := get("linear"); v != "" {
		links, err := linearLinks(c, sp, v)
		if err != nil {
			fatal(err)
		}
		docP["linear"] = links
		noteSessionLinear(c, st, ci.sessionId, links, sp)
	}
	if v := get("related"); v != "" {
		ids, err := sameSpaceIds(sp, v)
		if err != nil {
			fatal(err)
		}
		docP["related"] = linkList(ids)
	}
	groups := map[string]map[string]any{sp.owner("doc"): props(sp, "doc", docP)}
	collections := []string{sp.owner("doc")}
	if taskProps != nil {
		tp := map[string]any{"status": []string{"todo"}, "priority": []string{get("priority")}}
		if v := get("linear"); v != "" {
			tp["linearKey"] = strings.ToUpper(strings.TrimSpace(v))
		}
		if h := get("target"); h != "" {
			if id := machineByHost(c, sp, h); id != "" {
				tp["target"] = linkList([]string{id})
			} else {
				fmt.Fprintf(os.Stderr, "any-agents: no machine %q in %s — target ignored\n", h, sp.Name)
			}
		}
		if a := get("assignee"); a != "" {
			links, err := resolveMembers(c, sp, a)
			if err != nil {
				fatal(err)
			}
			tp["assignee"] = links
		}
		for k, v := range taskProps {
			tp[k] = v
		}
		groups[sp.owner("task")] = props(sp, "task", tp)
		collections = append(collections, sp.owner("task"))
	}
	var objectId string
	if fs.Lookup("new").Value.String() != "true" {
		// Re-saving by title updates only a doc of mine (or one without an
		// author, migrated); another member's doc of the same name stays.
		if row, _ := findByName(c, sp, "doc", name); row != nil && ownDoc(row, sp, st) {
			objectId, _ = row["id"].(string)
			for _, coll := range collections {
				if !inCollection(row, coll) {
					_ = c.AttachCollection(sp.Id, objectId, coll)
				}
			}
			for t, g := range groups {
				if err := c.SetProperties(sp.Id, objectId, t, g); err != nil {
					fatal(err)
				}
			}
			if body != "" {
				if err := putBody(c, sp, objectId, name, body); err != nil {
					fatal(err)
				}
			}
			fmt.Fprintf(os.Stderr, "updated existing %s\n", name)
		}
	}
	if objectId == "" {
		objectId, err = newObject(c, sp, pageType, collections, name, groups, body)
		if err != nil {
			fatal(err)
		}
	}
	if !isText {
		fid, err := c.AttachFile(sp.Id, objectId, filepath.Base(src), data)
		if err != nil {
			fatal(err)
		}
		_ = c.MarkdownAppend(sp.Id, objectId, fmt.Sprintf("Attached file `%s` (%d bytes): any://f/%s/%s\n", filepath.Base(src), len(data), sp.Id, fid))
	}
	fmt.Printf("%s  %s\n", globalLink(sp.Id, objectId), name)
}

// ownDoc reports whether a doc's author is me, or unset.
func ownDoc(row map[string]any, sp *Space, st *State) bool {
	a := firstMember(row, sp.owner("doc"), sp.prop("doc", "author"))
	return a == "" || a == st.me()
}

func machineByHost(c *Client, sp *Space, host string) string {
	rows, err := c.QueryObjects(sp.Id, Query{Filter: bundleFilter(sp, "machine", map[string]any{"any.name": host}), Limit: 1})
	if err != nil || len(rows) == 0 {
		return ""
	}
	id, _ := rows[0]["id"].(string)
	return id
}

func docList(c *Client, sp *Space, args []string) {
	fs, _ := parseArgs("doc list", args, func(fs *flag.FlagSet) {
		fs.String("kind", "", "filter by kind")
		fs.String("project", "", "filter by project scope")
		fs.Bool("all", false, "include archived")
		fs.Int("n", 50, "max rows")
	})
	get := func(k string) string { return fs.Lookup(k).Value.String() }
	var extra []map[string]any
	if k := get("kind"); k != "" {
		extra = append(extra, map[string]any{propPath(sp, "doc", "kind"): k})
	}
	if get("all") != "true" {
		extra = append(extra, map[string]any{propPath(sp, "doc", "archived"): map[string]any{"$ne": true}})
	}
	rows, err := objectsOf(c, sp, "doc", nil, extra...)
	if err != nil {
		fatal(err)
	}
	projects := nameIndex(c, sp, "project")
	issues := linearKeys(c, sp)
	n := 0
	for _, r := range rows {
		if get("kind") == "" && get("all") != "true" && sp.isA(r, "task") {
			continue // tasks have their own listing
		}
		pid := firstLink(r, sp, "doc", "project")
		if p := get("project"); p != "" && projects[pid] != p {
			continue
		}
		if n++; n > atoi(get("n")) {
			fmt.Printf("… %d more\n", len(rows)-n+1)
			break
		}
		facets := ""
		if sp.isA(r, "task") {
			facets += " task:" + choiceOf(r, sp.owner("task"), sp.prop("task", "status"))
		}
		if sp.isA(r, "plan") {
			facets += " plan:" + choiceOf(r, sp.owner("plan"), sp.prop("plan", "status"))
		}
		if pr := strOf(r, sp.owner("doc"), sp.prop("doc", "pr")); pr != "" {
			facets += " " + prShort(pr)
		}
		for _, lid := range linksOf(r, sp.owner("doc"), sp.prop("doc", "linear")) {
			facets += " " + issues[lid]
		}
		fmt.Printf("%s  %-7s %-16s %s%s\n", globalLink(sp.Id, r["id"].(string)),
			choiceOf(r, sp.owner("doc"), sp.prop("doc", "kind")), pad(projects[pid], 16), nameOf(r), facets)
	}
}

// showObject prints an object's header (name, bundles, key properties)
// and body; withLinked also renders related docs' bodies.
func showObject(c *Client, sp *Space, id string, withLinked bool) {
	row, err := c.GetObject(sp.Id, id)
	if err != nil {
		fatal(err)
	}
	if sp.isA(row, "task") {
		resolveTaskLinear(c, sp, row)
	}
	fmt.Printf("# %s\n%s\n", nameOf(row), globalLink(sp.Id, id))
	var meta []string
	issueNames := func(ids []string) string {
		issues := linearKeys(c, sp)
		var keys []string
		for _, id := range ids {
			keys = append(keys, issues[id])
		}
		return strings.Join(keys, ",")
	}
	if sp.isA(row, "doc") {
		meta = append(meta, "kind="+choiceOf(row, sp.owner("doc"), sp.prop("doc", "kind")))
		if pr := strOf(row, sp.owner("doc"), sp.prop("doc", "pr")); pr != "" {
			meta = append(meta, "pr="+pr)
		}
		if ids := linksOf(row, sp.owner("doc"), sp.prop("doc", "linear")); len(ids) > 0 {
			meta = append(meta, "linear="+issueNames(ids))
		}
	}
	if sp.isA(row, "task") {
		meta = append(meta, "task="+choiceOf(row, sp.owner("task"), sp.prop("task", "status")),
			"priority="+choiceOf(row, sp.owner("task"), sp.prop("task", "priority")))
		if a := firstMember(row, sp.owner("task"), sp.prop("task", "assignee")); a != "" {
			meta = append(meta, "assignee="+memberIndex(c, sp)[identityOf(a)])
		}
	}
	if sp.isA(row, "plan") {
		meta = append(meta, "plan="+choiceOf(row, sp.owner("plan"), sp.prop("plan", "status")))
	}
	if sp.isA(row, "session") {
		meta = append(meta, "status="+choiceOf(row, sp.owner("session"), sp.prop("session", "status")))
		if ids := linksOf(row, sp.owner("session"), sp.prop("session", "linear")); len(ids) > 0 {
			meta = append(meta, "linear="+issueNames(ids))
		}
	}
	if len(meta) > 0 {
		fmt.Println(strings.Join(meta, "  "))
	}
	body, err := c.MarkdownGet(sp.Id, id)
	if err != nil {
		fatal(err)
	}
	fmt.Println()
	fmt.Println(strings.TrimSpace(body))
	if !withLinked {
		return
	}
	for _, rid := range linksOf(row, sp.owner("doc"), sp.prop("doc", "related")) {
		lr, err := c.GetObject(sp.Id, rid)
		if err != nil {
			continue
		}
		lb, _ := c.MarkdownGet(sp.Id, rid)
		fmt.Printf("\n---\n## related: %s  (%s)\n\n%s\n", nameOf(lr), globalLink(sp.Id, rid), strings.TrimSpace(lb))
	}
}

// --- task ---

func cmdTask(args []string) {
	if len(args) == 0 {
		fatal(fmt.Errorf("usage: task add|list|show|start|done|cancel|link"))
	}
	c := NewClient()
	st := mustSetup(c)
	space, args := extractSpace(args)
	sp := spaceOf(st, space)
	switch args[0] {
	case "add":
		docAdd(c, st, sp, args[1:], map[string]any{})
	case "list":
		taskList(c, st, sp, args[1:])
	case "show":
		if len(args) < 2 {
			fatal(fmt.Errorf("task show <id>"))
		}
		sp, id := objectIn(st, sp, args[1])
		showObject(c, sp, id, true)
	case "start", "done", "cancel":
		fs, pos := parseArgs("task "+args[0], args[1:], func(fs *flag.FlagSet) {
			fs.String("s", "", "session id")
		})
		if len(pos) < 1 {
			fatal(fmt.Errorf("task %s <id>", args[0]))
		}
		sp, id := objectIn(st, sp, pos[0])
		status := map[string]string{"start": "in_progress", "done": "done", "cancel": "cancelled"}[args[0]]
		if err := taskSetStatus(c, st, sp, id, status, fs.Lookup("s").Value.String()); err != nil {
			fatal(err)
		}
		fmt.Printf("%s  %s\n", globalLink(sp.Id, id), status)
	case "link":
		if len(args) < 3 {
			fatal(fmt.Errorf("task link <id> <objectId>"))
		}
		sp, id := objectIn(st, sp, args[1])
		targets, err := sameSpaceIds(sp, args[2])
		if err != nil || len(targets) != 1 {
			fatal(fmt.Errorf("task link <id> <objectId in the same space>: %v", err))
		}
		if err := addRelation(c, sp, id, "doc", "related", targets[0]); err != nil {
			fatal(err)
		}
	default:
		fatal(fmt.Errorf("unknown task command %q", args[0]))
	}
}

// taskSetStatus moves a task; starting an unassigned task assigns it to
// me, and in the private space records the machine and session.
func taskSetStatus(c *Client, st *State, sp *Space, id, status, sessionFlag string) error {
	row, err := c.GetObject(sp.Id, id)
	if err != nil {
		return err
	}
	resolveTaskLinear(c, sp, row)
	tp := map[string]any{"status": []string{status}}
	if status != "todo" {
		if firstMember(row, sp.owner("task"), sp.prop("task", "assignee")) == "" {
			tp["assignee"] = []string{st.me()}
		}
		if sp.MachineObjectId != "" {
			tp["handledBy"] = linkList([]string{sp.MachineObjectId})
		}
		if sp.Kind == kindPrivate {
			if sid, _ := resolveSession(st, sessionFlag); sid != "" {
				if srow := sessionById(c, sp, sid); srow != nil {
					tp["handledIn"] = linkList([]string{srow["id"].(string)})
					noteSessionLinear(c, st, srow["id"].(string), linkList(linksOf(row, sp.owner("doc"), sp.prop("doc", "linear"))), sp)
				}
			}
		}
	}
	if err := c.SetProperties(sp.Id, id, sp.owner("task"), props(sp, "task", tp)); err != nil {
		return err
	}
	if status == "done" && sp.isA(row, "plan") {
		_ = c.SetProperties(sp.Id, id, sp.owner("plan"), props(sp, "plan", map[string]any{"status": []string{"done"}}))
	}
	return nil
}

// pendingTasks lists open tasks for a project id: untargeted or targeted
// at this machine, unassigned or assigned to me.
func pendingTasks(c *Client, st *State, sp *Space, projectId string) []map[string]any {
	rows, err := objectsOf(c, sp, "task", []string{"modifiedAt"},
		map[string]any{propPath(sp, "task", "status"): map[string]any{"$in": []string{"todo", "in_progress"}}})
	if err != nil {
		return nil
	}
	var out []map[string]any
	for _, r := range rows {
		pid := firstLink(r, sp, "doc", "project")
		tgt := firstLink(r, sp, "task", "target")
		who := firstMember(r, sp.owner("task"), sp.prop("task", "assignee"))
		if (pid == "" || pid == projectId) && (tgt == "" || tgt == sp.MachineObjectId) && (who == "" || who == st.me()) {
			out = append(out, r)
		}
	}
	return out
}

func taskList(c *Client, st *State, sp *Space, args []string) {
	fs, _ := parseArgs("task list", args, func(fs *flag.FlagSet) {
		fs.Bool("all", false, "include done and cancelled")
		fs.Bool("mine", false, "only tasks assigned to me or unassigned")
		fs.String("project", "", "filter by project scope")
	})
	get := func(k string) string { return fs.Lookup(k).Value.String() }
	var extra []map[string]any
	if get("all") != "true" {
		extra = append(extra, map[string]any{propPath(sp, "task", "status"): map[string]any{"$in": []string{"todo", "in_progress"}}})
	}
	rows, err := objectsOf(c, sp, "task", []string{"modifiedAt"}, extra...)
	if err != nil {
		fatal(err)
	}
	for _, r := range rows {
		resolveTaskLinear(c, sp, r)
	}
	projects := nameIndex(c, sp, "project")
	machines := nameIndex(c, sp, "machine")
	members := memberIndex(c, sp)
	issues := linearKeys(c, sp)
	for _, r := range rows {
		pid := firstLink(r, sp, "doc", "project")
		if p := get("project"); p != "" && projects[pid] != p {
			continue
		}
		who := firstMember(r, sp.owner("task"), sp.prop("task", "assignee"))
		if get("mine") == "true" && who != "" && who != st.me() {
			continue
		}
		status := choiceOf(r, sp.owner("task"), sp.prop("task", "status"))
		facets := ""
		if who != "" {
			facets += " @" + members[identityOf(who)]
		}
		if m := machines[firstLink(r, sp, "task", "handledBy")]; m != "" {
			facets += " by " + m
		}
		for _, lid := range linksOf(r, sp.owner("doc"), sp.prop("doc", "linear")) {
			facets += " " + issues[lid]
		}
		if sp.isA(r, "plan") {
			facets += " [plan " + choiceOf(r, sp.owner("plan"), sp.prop("plan", "status")) + "]"
		}
		proj := projects[pid]
		if proj == "" {
			proj = "(any project)"
		}
		fmt.Printf("%s  %-11s %-6s %-16s %s%s\n", globalLink(sp.Id, r["id"].(string)), status,
			choiceOf(r, sp.owner("task"), sp.prop("task", "priority")), pad(proj, 16), nameOf(r), facets)
	}
}

// --- plan ---

func cmdPlan(args []string) {
	if len(args) == 0 {
		fatal(fmt.Errorf("usage: plan add|list|show|approve|done"))
	}
	c := NewClient()
	st := mustSetup(c)
	space, args := extractSpace(args)
	sp := spaceOf(st, space)
	switch args[0] {
	case "add":
		planAdd(c, st, sp, args[1:])
	case "list":
		fs, _ := parseArgs("plan list", args[1:], func(fs *flag.FlagSet) { fs.Bool("all", false, "include done") })
		var extra []map[string]any
		if fs.Lookup("all").Value.String() != "true" {
			extra = append(extra, map[string]any{propPath(sp, "plan", "status"): map[string]any{"$ne": "done"}})
		}
		rows, err := objectsOf(c, sp, "plan", nil, extra...)
		if err != nil {
			fatal(err)
		}
		projects := nameIndex(c, sp, "project")
		for _, r := range rows {
			fmt.Printf("%s  %-9s %-16s %s\n", globalLink(sp.Id, r["id"].(string)),
				choiceOf(r, sp.owner("plan"), sp.prop("plan", "status")),
				pad(projects[firstLink(r, sp, "doc", "project")], 16), nameOf(r))
		}
	case "show":
		if len(args) < 2 {
			fatal(fmt.Errorf("plan show <id>"))
		}
		sp, id := objectIn(st, sp, args[1])
		showObject(c, sp, id, false)
	case "approve", "done":
		if len(args) < 2 {
			fatal(fmt.Errorf("plan %s <id>", args[0]))
		}
		sp, id := objectIn(st, sp, args[1])
		pp := map[string]any{"status": []string{map[string]string{"approve": "approved", "done": "done"}[args[0]]}}
		if args[0] == "approve" {
			pp["approvedAt"] = dateVal(time.Now())
		} else if sp.Kind == kindPrivate {
			pp["executedBy"] = linkList([]string{sp.MachineObjectId})
			if sid, _ := resolveSession(st, ""); sid != "" {
				if row := sessionById(c, sp, sid); row != nil {
					_ = addRelation(c, sp, id, "plan", "executedIn", row["id"].(string))
				}
			}
		}
		if err := c.SetProperties(sp.Id, id, sp.owner("plan"), props(sp, "plan", pp)); err != nil {
			fatal(err)
		}
	default:
		fatal(fmt.Errorf("unknown plan command %q", args[0]))
	}
}

// planAdd files a task (or any doc) under Plan and appends the plan text
// to its body; without a target it files a standalone plan doc.
func planAdd(c *Client, st *State, sp *Space, args []string) {
	fs, pos := parseArgs("plan add", args, func(fs *flag.FlagSet) {
		fs.String("status", "draft", "draft | approved")
		fs.String("title", "", "title for a standalone plan")
		fs.String("project", "", "project scope for a standalone plan")
		fs.String("linear", "", "Linear issue key")
		fs.Bool("replace", false, "replace the body instead of appending under '## Plan'")
	})
	get := func(k string) string { return fs.Lookup(k).Value.String() }
	if len(pos) < 1 {
		fatal(fmt.Errorf("plan add [<taskId>] <file.md|->"))
	}
	var target string
	src := pos[len(pos)-1]
	if len(pos) >= 2 {
		sp, target = objectIn(st, sp, pos[0])
	}
	data, err := readSource(src)
	if err != nil {
		fatal(err)
	}
	body := string(data)
	pp := props(sp, "plan", map[string]any{"status": []string{get("status")}})
	if target == "" {
		if titleOf(get("title"), body, "") == "" {
			fatal(fmt.Errorf("a standalone plan needs --title or a '# Heading'"))
		}
		extra := []string{"--kind", "plan"}
		for _, f := range []string{"title", "project", "linear"} {
			if v := get(f); v != "" {
				extra = append(extra, "--"+f, v)
			}
		}
		// A standalone plan is a Doc of kind plan filed under Plan.
		f, err := os.CreateTemp("", "any-agents-plan-*.md")
		if err != nil {
			fatal(err)
		}
		tmp := f.Name()
		_, _ = f.Write(data)
		_ = f.Close()
		defer os.Remove(tmp)
		docAdd(c, st, sp, append([]string{tmp}, extra...), nil)
		row, _ := findByName(c, sp, "doc", titleOf(get("title"), body, "plan"))
		if row == nil {
			return
		}
		target, _ = row["id"].(string)
		if err := c.AttachCollection(sp.Id, target, sp.owner("plan")); err != nil {
			fatal(err)
		}
		if err := c.SetProperties(sp.Id, target, sp.owner("plan"), pp); err != nil {
			fatal(err)
		}
		return
	}
	if err := c.AttachCollection(sp.Id, target, sp.owner("plan")); err != nil {
		fatal(err)
	}
	if err := c.SetProperties(sp.Id, target, sp.owner("plan"), pp); err != nil {
		fatal(err)
	}
	sessionObj := ""
	if sp.Kind == kindPrivate {
		if sid, _ := resolveSession(st, ""); sid != "" {
			if row := sessionById(c, sp, sid); row != nil {
				sessionObj, _ = row["id"].(string)
			}
		}
	}
	if v := get("linear"); v != "" {
		if links, err := linearLinks(c, sp, v); err == nil {
			_ = c.SetProperties(sp.Id, target, sp.owner("doc"), props(sp, "doc", map[string]any{"linear": links}))
			noteSessionLinear(c, st, sessionObj, links, sp)
		}
	}
	if get("replace") == "true" {
		err = c.MarkdownPut(sp.Id, target, stripTitle(body))
	} else {
		if !strings.HasPrefix(strings.TrimSpace(body), "#") {
			body = "## Plan\n\n" + body
		}
		err = c.MarkdownAppend(sp.Id, target, "\n"+body)
	}
	if err != nil {
		fatal(err)
	}
	if sessionObj != "" {
		_ = addRelation(c, sp, target, "doc", "sessions", sessionObj)
	}
	fmt.Printf("%s  plan %s\n", globalLink(sp.Id, target), get("status"))
}

// --- project / machine listings ---

func cmdProject(args []string) {
	c := NewClient()
	st := mustSetup(c)
	space, args := extractSpace(args)
	sp := spaceOf(st, space)
	rows, err := objectsOf(c, sp, "project", []string{"any.name"})
	if err != nil {
		fatal(err)
	}
	if len(args) > 1 && args[0] == "show" {
		for _, r := range rows {
			if nameOf(r) == args[1] {
				showObject(c, sp, r["id"].(string), false)
				return
			}
		}
		fatal(fmt.Errorf("project %q not found", args[1]))
	}
	sort.Slice(rows, func(i, j int) bool { return nameOf(rows[i]) < nameOf(rows[j]) })
	for _, r := range rows {
		fmt.Printf("%s  %-20s %4.0f memories  %s\n", globalLink(sp.Id, r["id"].(string)), pad(nameOf(r), 20),
			numOf(r, sp.owner("project"), sp.prop("project", "memories")),
			choiceOf(r, sp.owner("project"), sp.prop("project", "status")))
	}
}

func cmdMachine(args []string) {
	c := NewClient()
	sp := mustSetup(c).private()
	rows, err := objectsOf(c, sp, "machine", []string{"any.name"})
	if err != nil {
		fatal(err)
	}
	for _, r := range rows {
		t := sp.owner("machine")
		fmt.Printf("%s  %-16s %-7s %s  %s  %s\n", globalLink(sp.Id, r["id"].(string)), pad(nameOf(r), 16),
			choiceOf(r, t, sp.prop("machine", "status")), day(recordFloat(group(r, t), sp.prop("machine", "lastSeen"))),
			strOf(r, t, sp.prop("machine", "os")), strOf(r, t, sp.prop("machine", "tools")))
	}
}

func numOf(row map[string]any, ownerId, propId string) float64 {
	f, _ := group(row, ownerId)[propId].(float64)
	return f
}

func atoi(s string) int {
	n := 0
	fmt.Sscanf(s, "%d", &n)
	return n
}

// linearKeys maps Linear issue object id → key for listings.
func linearKeys(c *Client, sp *Space) map[string]string {
	out := map[string]string{}
	t := sp.owner("linear")
	rows, err := c.QueryObjects(sp.Id, Query{Filter: bundleFilter(sp, "linear"), Limit: 1000,
		Projection: map[string]any{t: 1}})
	if err != nil {
		return out
	}
	for _, r := range rows {
		id, _ := r["id"].(string)
		out[id] = strOf(r, t, sp.prop("linear", "key"))
	}
	return out
}

// resolveTaskLinear turns a Linear key a human typed into the task's
// "Linear issue" text property into the issue object and the doc's
// `linear` relation (idempotent; a key already linked is left alone).
func resolveTaskLinear(c *Client, sp *Space, row map[string]any) {
	key := strings.ToUpper(strings.TrimSpace(strOf(row, sp.owner("task"), sp.prop("task", "linearKey"))))
	if key == "" || !linearKeyRe.MatchString(key) {
		return
	}
	id, _ := row["id"].(string)
	issues := linearKeys(c, sp)
	for _, lid := range linksOf(row, sp.owner("doc"), sp.prop("doc", "linear")) {
		if issues[lid] == key {
			return
		}
	}
	issueId, err := ensureLinearIssue(c, sp, key)
	if err != nil {
		fmt.Fprintf(os.Stderr, "any-agents: %s: %v\n", key, err)
		return
	}
	if err := addRelation(c, sp, id, "doc", "linear", issueId); err != nil {
		fmt.Fprintf(os.Stderr, "any-agents: link %s: %v\n", key, err)
	}
	row[sp.owner("doc")] = group(row, sp.owner("doc")) // keep caller's view fresh
	g := group(row, sp.owner("doc"))
	g[sp.prop("doc", "linear")] = addToLinks(linksOf(row, sp.owner("doc"), sp.prop("doc", "linear")), issueId)
}

// noteSessionLinear records the issues a session touched (relation on
// the private Session object) so an issue's backlinks include the
// sessions. Issues from a collab space are re-resolved in the private
// one, since relations cannot cross spaces.
func noteSessionLinear(c *Client, st *State, sessionObjId string, issueLinks []string, from *Space) {
	if sessionObjId == "" {
		return
	}
	priv := st.private()
	keys := linearKeys(c, from)
	for _, l := range issueLinks {
		id := parseObjectId(l)
		if from.Id != priv.Id {
			key := keys[id]
			if key == "" {
				continue
			}
			var err error
			if id, err = ensureLinearIssue(c, priv, key); err != nil {
				continue
			}
		}
		_ = addRelation(c, priv, sessionObjId, "session", "linear", id)
	}
}
