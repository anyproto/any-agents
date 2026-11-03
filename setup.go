package main

import (
	"fmt"
	"os"
	"os/user"
	"runtime"
	"slices"
	"strings"
	"time"
)

func getenv(k string) string { return os.Getenv(k) }

// ensureSetup resolves the account and the private space (creating the
// space on the machine that asks for it) and sets the space up. Collab
// spaces are set up by `space add` and re-checked by the session hook.
func ensureSetup(c *Client, st *State, createSpace bool) error {
	if sp := st.private(); sp != nil && sp.ready() && sp.SetupVersion == setupVersion && sp.MachineObjectId != "" && st.Identity != "" {
		return nil
	}
	acc, err := c.Account()
	if err != nil {
		return err
	}
	st.Identity, st.TechSpaceId = acc.Id, acc.TechSpaceId
	if st.Private == "" {
		id, name, err := resolvePrivateSpace(c, createSpace)
		if err != nil {
			return err
		}
		st.Private = id
		sp := st.addSpace(id, name, kindPrivate)
		sp.Created = createSpace && !existed(c, id, st)
		if err := st.save(); err != nil {
			return err
		}
	}
	return ensureSpaceSetup(c, st.private())
}

// existed reports whether a space was already in the account's list
// before this install (then its registry may hold other machines' roots).
func existed(c *Client, id string, st *State) bool {
	_, synced, err := c.ListBundles(id)
	return err == nil && synced
}

// resolvePrivateSpace finds the private space by name, or creates it.
func resolvePrivateSpace(c *Client, create bool) (string, string, error) {
	spaces, err := c.ListSpaces()
	if err != nil {
		return "", "", err
	}
	name := spaceName()
	for _, s := range spaces {
		if s.Name != name {
			continue
		}
		switch s.OwnRole {
		case "owner":
		case "", "none":
			return "", "", fmt.Errorf("space %q is not loaded on this device yet — wait for sync and run again", name)
		default:
			return "", "", fmt.Errorf("space %q is shared with this account (%s), not its own — set ANY_AGENTS_SPACE to another name", name, s.OwnRole)
		}
		if ours, err := ownsSpace(c, s.Id); err != nil {
			return "", "", err
		} else if !ours {
			return "", "", fmt.Errorf("space %q already holds other data — set ANY_AGENTS_SPACE to a fresh name", name)
		}
		return s.Id, name, nil
	}
	if !create {
		return "", "", fmt.Errorf("space %q not found — on a second machine wait for the account to sync; only the first machine runs `any-agents install --create-space`", name)
	}
	id, err := c.CreateSpace(name, "Private context of this account's coding agents (managed by any-agents)")
	return id, name, err
}

// awaitRegistry waits (bounded) until a space's bundle registry has
// synced before anything is installed into it: an install against an
// unsynced registry can mint a second root next to the one another
// machine or member already registered. A space this machine just
// created has nothing to wait for.
func awaitRegistry(c *Client, sp *Space) {
	if sp.Created {
		return
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		_, synced, err := c.ListBundles(sp.Id)
		if err != nil || synced {
			return
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "any-agents: %s: bundle registry still syncing after 60 s — installing anyway; run `any-agents install` again once online\n", sp.Name)
			return
		}
		time.Sleep(2 * time.Second)
	}
}

// ownsSpace reports whether a space is this tool's (its app bundle is
// registered) or still empty; anything else is somebody's data.
func ownsSpace(c *Client, spaceId string) (bool, error) {
	bundles, _, err := c.ListBundles(spaceId)
	if err != nil {
		return false, err
	}
	for _, b := range bundles {
		if b.Id == bundleApp {
			return true, nil
		}
	}
	rows, err := c.QueryObjects(spaceId, Query{Filter: map[string]any{"any.type": map[string]any{"$nin": []string{"__type__", "__collection__", "dataview"}}}, Limit: 1})
	if err != nil {
		return false, err
	}
	return len(rows) == 0, nil
}

// ensureSpaceSetup makes one space usable: usecases, bundles, this
// machine's object (private space only), views. Idempotent; gated by
// setupVersion so the hook fast path is one stat.
func ensureSpaceSetup(c *Client, sp *Space) error {
	if sp.ready() && sp.SetupVersion == setupVersion && (sp.Kind != kindPrivate || sp.MachineObjectId != "") {
		return nil
	}
	if err := ensureUsecases(c, sp); err != nil {
		return err
	}
	complete, err := ensureBundles(c, sp)
	if err != nil {
		return err
	}
	if sp.Kind == kindPrivate {
		if _, err := ensureMachine(c, sp); err != nil {
			return err
		}
	}
	if err := ensureViews(c, sp); err != nil {
		fmt.Fprintf(os.Stderr, "any-agents: views: %v\n", err) // cosmetic, never fatal
	}
	_ = c.RegisterApp("any-agents", version)
	// A schema this member could not complete (a reader waiting for a
	// writer's property) is re-checked next time.
	if complete {
		sp.SetupVersion = setupVersion
	}
	return sp.save()
}

// ensureUsecases installs the catalog usecases the space relies on:
// collections, the feature switch that shows the types library in clients.
func ensureUsecases(c *Client, sp *Space) error {
	if sp.Usecases == nil {
		sp.Usecases = map[string]bool{}
	}
	if !sp.Usecases["collections"] {
		// A just-joined space answers bundle.not_ready until its registry
		// has synced; wait for it instead of failing the first `space add`.
		awaitRegistry(c, sp)
		deadline := time.Now().Add(60 * time.Second)
		for {
			_, err := c.CatalogSetup(sp.Id, "collections")
			if err == nil {
				break
			}
			if !isCode(err, "bundle.not_ready") || time.Now().After(deadline) {
				return fmt.Errorf("collections setup: %w", err)
			}
			time.Sleep(time.Second)
		}
		sp.Usecases["collections"] = true
	}
	return sp.save()
}

// ensureBundles adopts-or-installs the bundles and caches rootId, xKey →
// propId and part storage collections for each. Collab is skipped on a
// server that still reserves the chat module.
func ensureBundles(c *Client, sp *Space) (complete bool, err error) {
	drafts := bundleDrafts()
	waited := false
	complete = true
	for _, name := range bundleNames {
		bs := sp.bundle(name)
		if bs.RootId != "" && len(bs.Props) >= len(drafts[name].Properties) && sp.SetupVersion == setupVersion {
			continue
		}
		if !waited {
			awaitRegistry(c, sp)
			waited = true
		}
		b, installed, err := c.EnsureBundle(sp.Id, drafts[name])
		if err != nil && optionalBundles[name] && isCode(err, "dataset.module_reserved") {
			// This server cannot install a chat-bearing type, but a member
			// may have registered it already: adopting is a pure read.
			if reg, gerr := c.GetBundle(sp.Id, drafts[name].Id); gerr == nil && reg.RootId != "" {
				b, installed, err = reg, false, nil
				fmt.Fprintf(os.Stderr, "any-agents: %s adopted from another member; this any server predates chat on user types, so it can join and talk in collabs but not open them\n", drafts[name].Id)
			} else {
				fmt.Fprintf(os.Stderr, "any-agents: %s needs an any server that lets a type declare the chat module — collabs are off in %s\n", drafts[name].Id, sp.Name)
				delete(sp.Bundles, name)
				continue
			}
		}
		if err != nil {
			return false, fmt.Errorf("bundle %s: %w", drafts[name].Id, err)
		}
		bs.RootId = b.RootId
		var props []PropertyInfo
		if collectionBundles[name] {
			props, err = c.ListCollectionProperties(sp.Id, b.RootId)
		} else {
			props, err = c.ListProperties(sp.Id, b.RootId)
		}
		if err != nil {
			return false, err
		}
		bs.Props = map[string]string{}
		for _, p := range props {
			bs.Props[p.XKey] = p.Id
		}
		// Additive schema evolution: a draft property the registered
		// bundle lacks is added to its root; ids derive from (root, xKey),
		// so members converge on the same property.
		for _, d := range drafts[name].Properties {
			if _, ok := bs.Props[d.XKey]; ok {
				continue
			}
			var pid string
			if collectionBundles[name] {
				pid, err = c.AddCollectionProperty(sp.Id, b.RootId, d)
			} else {
				pid, err = c.AddProperty(sp.Id, b.RootId, d)
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "any-agents: %s: property %s not added (%v); a writer's any-agents adds it, this machine retries\n", drafts[name].Id, d.XKey, err)
				complete = false
				continue
			}
			bs.Props[d.XKey] = pid
		}
		var parts []PartInfo
		if !collectionBundles[name] {
			if parts, err = c.ListParts(sp.Id, b.RootId); err != nil {
				return false, err
			}
		}
		bs.Collections = map[string]string{}
		for _, p := range parts {
			for _, d := range p.Datasets {
				bs.Collections[p.Key] = d.Collection
			}
		}
		if installed {
			icon := typeIcons[name]
			_ = c.SetProperties(sp.Id, b.RootId, "any", map[string]any{"icon": iconOf(icon[0], icon[1])})
			if collectionBundles[name] {
				// Members are plain pages: tell clients what to create.
				_ = c.PatchCollection(sp.Id, b.RootId, map[string]any{"meta": map[string]any{"defaultType": pageType}})
			}
			if name == "app" {
				_ = c.MarkdownPut(sp.Id, b.RootId, "Status page maintained by any-agents. Nothing synced yet.\n")
			}
		}
		if err := sp.save(); err != nil {
			return false, err
		}
	}
	return complete, nil
}

// ensureMachine resolves this host's Machine object and stamps liveness.
func ensureMachine(c *Client, sp *Space) (string, error) {
	host, _ := os.Hostname()
	if sp.MachineObjectId == "" {
		id, err := c.BundleChild(sp.Id, bundleApp, "machine:"+host, pageType, []string{sp.owner("machine")})
		if err != nil {
			return "", fmt.Errorf("machine object: %w", err)
		}
		sp.MachineObjectId = id
		if err := c.SetProperties(sp.Id, id, "any", map[string]any{"name": host}); err != nil {
			return "", err
		}
		u, _ := user.Current()
		username := ""
		if u != nil {
			username = u.Username
		}
		_ = c.SetProperties(sp.Id, id, sp.owner("machine"), props(sp, "machine", map[string]any{
			"hostname": host, "os": runtime.GOOS + "/" + runtime.GOARCH, "user": username,
		}))
		if err := c.MarkdownPut(sp.Id, id, fmt.Sprintf("Machine running any-agents (%s, user %s).\n", runtime.GOOS, username)); err != nil {
			return "", err
		}
		if err := sp.save(); err != nil {
			return "", err
		}
	}
	// Several harnesses can use one machine. Register the active one on
	// setup, without replacing the others on every session end.
	row, err := c.GetObject(sp.Id, sp.MachineObjectId)
	if err != nil {
		return "", err
	}
	installed := strings.Fields(strOf(row, sp.owner("machine"), sp.prop("machine", "tools")))
	if tool := currentTool().Name(); !slices.Contains(installed, tool) {
		installed = append(installed, tool)
		slices.Sort(installed)
		if err := c.SetProperties(sp.Id, sp.MachineObjectId, sp.owner("machine"), props(sp, "machine", map[string]any{
			"tools": strings.Join(installed, " "),
		})); err != nil {
			return "", err
		}
	}
	return sp.MachineObjectId, touchMachine(c, sp, "online")
}

func touchMachine(c *Client, sp *Space, status string) error {
	return c.SetProperties(sp.Id, sp.MachineObjectId, sp.owner("machine"), props(sp, "machine", map[string]any{
		"lastSeen": dateVal(time.Now()), "status": []string{status}, "bridgeVersion": version,
	}))
}

// ensureProject resolves the Project page for a scope in a space
// (deterministic child, so every machine and member converges on it),
// naming it on first sight.
func ensureProject(c *Client, sp *Space, scope string) (string, error) {
	id, err := c.BundleChild(sp.Id, bundleApp, "project:"+scope, pageType, []string{sp.owner("project")})
	if err != nil {
		return "", err
	}
	row, err := c.GetObject(sp.Id, id)
	if err != nil {
		return "", err
	}
	if name, _ := group(row, "any")["name"].(string); name != "" {
		return id, nil
	}
	if err := c.SetProperties(sp.Id, id, "any", map[string]any{"name": scope}); err != nil {
		return "", err
	}
	_ = c.SetProperties(sp.Id, id, sp.owner("project"), props(sp, "project", map[string]any{
		"key": scope, "status": []string{"active"},
	}))
	_ = c.MarkdownPut(sp.Id, id, "Project page maintained by any-agents.\n")
	return id, nil
}

// ensureViews resolves one dataview object per browsable collection or
// type and upserts the well-known views so every machine converges on
// the same tables. layoutSettings column ids are bare property ids
// (any-ui's convention); query paths are the server's <ownerId>.<propId>.
func ensureViews(c *Client, sp *Space) error {
	type view struct {
		id, name string
		sort     []string
		filter   map[string]any
		cols     []string
	}
	open := map[string]any{propPath(sp, "task", "status"): map[string]any{"$in": []string{"todo", "in_progress"}}}
	views := map[string]struct {
		name  string
		views []view
	}{
		"doc": {"Docs", []view{{"default", "Table", []string{"-" + propPath(sp, "doc", "date")}, nil, []string{"kind", "project", "author", "linear", "pr", "originSession", "date"}}}},
		"task": {"Tasks", []view{
			{"default", "Open", []string{"-modifiedAt"}, open, []string{"status", "priority", "assignee", "linearKey", "project", "target", "due"}},
			{"all", "All", []string{"-" + propPath(sp, "doc", "date")}, nil, []string{"status", "priority", "assignee", "linearKey", "handledBy", "handledIn"}}}},
		"session": {"Sessions", []view{{"default", "Table", []string{"-" + propPath(sp, "session", "startedAt")}, nil, []string{"status", "tool", "project", "machine", "owner", "linear", "startedAt", "summary"}}}},
		"project": {"Projects", []view{{"default", "Table", []string{"any.name"}, nil, []string{"status", "memories", "lastActivity"}}}},
		"machine": {"Machines", []view{{"default", "Table", []string{"any.name"}, nil, []string{"status", "lastSeen", "os", "tools"}}}},
		"linear":  {"Linear issues", []view{{"default", "Table", []string{propPath(sp, "linear", "key")}, nil, []string{"key", "status", "priority", "team", "assignee", "dueDate", "syncedAt"}}}},
	}
	if sp.has("collab") {
		active := map[string]any{propPath(sp, "collab", "status"): map[string]any{"$in": []string{"request", "in_progress"}}}
		views["collab"] = struct {
			name  string
			views []view
		}{"Collabs", []view{
			{"default", "Open", []string{"-" + propPath(sp, "collab", "date")}, active, []string{"status", "requester", "assignees", "project", "pr", "date"}},
			{"all", "All", []string{"-" + propPath(sp, "collab", "date")}, nil, []string{"status", "requester", "assignees", "project", "pr", "date", "resolvedAt"}}}}
	}
	if sp.has("release") {
		views["release"] = struct {
			name  string
			views []view
		}{"Releases", []view{{"default", "Table", []string{"-createdAt"}, nil,
			[]string{"project", "version", "released", "breaking", "apiChanges", "url", "includes", "author"}}}}
	}
	for name, v := range views {
		dv, err := ensureDataview(c, sp, name)
		if err != nil {
			return err
		}
		set := func(ds, id string, value map[string]any) error {
			res, err := c.Modify(sp.Id, dv, ds, []ModifyRecord{{Id: id, Upsert: true,
				Ops: []ModifyOp{{Type: "$set", Path: "", Value: value}}}})
			if err != nil {
				return err
			}
			for _, r := range res.Rejections {
				return fmt.Errorf("%s %s: %s", ds, id, r.Reason)
			}
			return nil
		}
		if err := set("dataviews", "default", map[string]any{"name": v.name, "pos": "a0"}); err != nil {
			return err
		}
		for i, vw := range v.views {
			visible := []string{"name"}
			for _, x := range vw.cols {
				if id := sp.prop(name, x); id != "" {
					visible = append(visible, id)
				}
			}
			var extra []map[string]any
			if vw.filter != nil {
				extra = append(extra, vw.filter)
			}
			if err := set("views", vw.id, map[string]any{
				"dataview": "default", "name": vw.name, "layout": "table", "pos": lexPos(i),
				"query":          map[string]any{"type": "plain", "filter": bundleFilter(sp, name, extra...), "sort": vw.sort},
				"layoutSettings": map[string]any{"visible": visible, "order": visible},
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// ensureDataview resolves the dataview object hosted by a bundle's root
// (the type or collection), creating it once; the id is cached in state.
func ensureDataview(c *Client, sp *Space, name string) (string, error) {
	bs := sp.bundle(name)
	if bs.Dataview != "" {
		return bs.Dataview, nil
	}
	host := sp.owner(name)
	rows, err := c.QueryObjects(sp.Id, Query{Filter: map[string]any{"any.type": "dataview", "dataview.host": host},
		Sort: []string{"id"}, Limit: 1})
	if err != nil {
		return "", err
	}
	if len(rows) > 0 {
		bs.Dataview, _ = rows[0]["id"].(string)
	} else {
		bs.Dataview, err = c.CreateObject(sp.Id, "dataview", nil, map[string]any{"dataview": map[string]any{"host": host}})
		if err != nil {
			return "", err
		}
	}
	return bs.Dataview, sp.save()
}

// addCollabSpace registers a space the account is a member of as a
// collab space and sets it up (adopt-or-install; a reader adopts only).
func addCollabSpace(c *Client, st *State, ref string) (*Space, error) {
	if st.private() == nil {
		return nil, fmt.Errorf("not installed — run `any-agents install` first")
	}
	var found *SpaceInfo
	id := parseSpaceId(ref)
	// A just-joined space lists before its metadata has synced; wait for
	// the name so the state does not record an empty one.
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(time.Second) {
		spaces, err := c.ListSpaces()
		if err != nil {
			return nil, err
		}
		found = nil
		for i := range spaces {
			s := &spaces[i]
			if s.Id == id || strings.EqualFold(s.Name, ref) {
				found = s
				break
			}
		}
		if found == nil {
			return nil, fmt.Errorf("space %q not found among this account's spaces (join it first)", ref)
		}
		if found.Name != "" || time.Now().After(deadline) {
			break
		}
	}
	if found.Name == "" {
		found.Name = shortId(found.Id)
	}
	if found.Id == st.Private {
		return nil, fmt.Errorf("%q is the private space", found.Name)
	}
	fresh := st.space(found.Id) == nil
	sp := st.addSpace(found.Id, found.Name, kindCollab)
	sp.SetupVersion = 0 // re-check every bundle, Collab included, on every add
	if err := st.save(); err != nil {
		return nil, err
	}
	if err := ensureSpaceSetup(c, sp); err != nil {
		if fresh {
			delete(st.Spaces, found.Id) // a space that failed to set up is not registered
			_ = st.save()
		}
		return nil, err
	}
	return sp, nil
}

// putBody writes an object's markdown body without its leading H1:
// clients render the object's name as the title, and a document's first
// H1 is that title (doc add derives the name from it).
func putBody(c *Client, sp *Space, objectId, name, body string) error {
	if name == "" {
		return c.MarkdownPut(sp.Id, objectId, body)
	}
	return c.MarkdownPut(sp.Id, objectId, stripTitle(body))
}

func stripTitle(body string) string {
	trimmed := strings.TrimLeft(body, "\n")
	first, rest, _ := strings.Cut(trimmed, "\n")
	if strings.HasPrefix(first, "# ") {
		return strings.TrimLeft(rest, "\n")
	}
	return body
}

// props maps xKey → value into propId → value for a bundle's owner.
func props(sp *Space, bundle string, kv map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range kv {
		if id := sp.prop(bundle, k); id != "" {
			out[id] = v
		}
	}
	return out
}

// group reads one owner's value group off an object row.
func group(row map[string]any, ownerId string) map[string]any {
	g, _ := row[ownerId].(map[string]any)
	if g == nil {
		return map[string]any{}
	}
	return g
}

func dateVal(t time.Time) map[string]any {
	return map[string]any{"$date": t.UTC().Format(time.RFC3339)}
}
