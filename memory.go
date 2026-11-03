package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Shared project memories: a memory file published into a collab space's
// memories records (same `<scope>:<slug>` ids as the private ones) is
// mirrored into every member's memory/shared/<space>/ for that project at
// session start, listed in MEMORY.md and found by recall. A record
// belongs to whoever published it: the publisher's mirror file is
// editable and pushed like a private one, every other member's copy is
// read-only, and another member's publish of the same slug is refused by
// the dataset's author rule. `share --move` makes the collab space the
// memory's home: the private record is retracted and the file lives on
// under memory/shared/. Unshare retracts (kind "retracted", empty body)
// rather than deletes: a deleted record id can never be reused, a
// retracted one can be published again.

// kindRetracted marks an unshared record: skipped by every reader, its
// id stays reusable by the publisher. The title is cleared too, so the
// text index no longer matches it.
const (
	kindRetracted  = "retracted"
	retractedTitle = "(retracted)"
)

func cmdMemory(args []string) {
	if len(args) == 0 {
		fatal(fmt.Errorf("usage: memory share <slug|file.md>... --space S [--move] | list [--space S] | unshare <slug> --space S [--delete]"))
	}
	c := NewClient()
	st := mustSetup(c)
	fs, pos := parseArgs("memory "+args[0], args[1:], func(fs *flag.FlagSet) {
		spaceFlag(fs)
		fs.String("scope", "", "project scope (default: the cwd repo)")
		fs.Bool("move", false, "share: retract the private record and keep the memory under memory/shared/")
		fs.Bool("delete", false, "unshare: drop the memory instead of moving it back to the private space")
	})
	get := func(k string) string { return fs.Lookup(k).Value.String() }
	cwd, _ := os.Getwd()
	scope := get("scope")
	if scope == "" {
		scope = resolveScope(cwd)
	}
	switch args[0] {
	case "list":
		sp := spaceOf(st, get("space"))
		rows, err := fetchScope(c, sp, scope)
		if err != nil {
			fatal(err)
		}
		members := map[string]string{}
		if sp.Kind == kindCollab {
			members = memberIndex(c, sp)
		}
		for _, r := range rows {
			m := memItemFromRecord(r)
			if m.Kind == kindRetracted {
				continue
			}
			who, _ := r["author"].(string)
			if n := members[who]; n != "" {
				who = n
			} else {
				who = shortId(who)
			}
			fmt.Printf("%-40s %-9s %-10s %s\n", pad(m.Slug, 40), m.Kind, pad(who, 10), trunc(m.Title, 80))
		}
	case "share":
		if len(pos) < 1 || get("space") == "" {
			fatal(fmt.Errorf("memory share <slug|file.md>... --space <collab space> [--move]"))
		}
		sp := spaceOf(st, get("space"))
		if sp.Kind != kindCollab {
			fatal(fmt.Errorf("%s is the private space — memories live there already", sp.Name))
		}
		memDir := currentTool().MemoryDir(cwd)
		home, _ := os.UserHomeDir()
		var items []shareItem
		var records []UpsertRecord
		for _, p := range pos {
			path := p
			if fi, err := os.Stat(path); err != nil || !fi.Mode().IsRegular() {
				path = filepath.Join(memDir, strings.TrimSuffix(p, ".md")+".md")
			}
			item, err := parseMemFile(path, scope)
			if err != nil {
				fatal(fmt.Errorf("%s: %v", path, err))
			}
			if home != "" && strings.Contains(item.Body, home) {
				fmt.Fprintf(os.Stderr, "any-agents: note: %s mentions your home directory; every member of %s will read it\n", item.Slug, sp.Name)
			}
			if abs, err := filepath.Abs(path); err == nil {
				path = abs
			}
			items = append(items, shareItem{item: item, path: path})
			records = append(records, item.upsertRecord(shortId(st.Identity)))
		}
		res, err := c.Upsert(sp.Id, sp.owner("app"), sp.coll(dsMemories), records)
		if err != nil {
			fatal(err)
		}
		rejected := map[string]bool{}
		for _, rej := range res.Rejections {
			hint := ""
			if strings.Contains(rej.Reason, "author") {
				hint = " — a memory with this slug was published by another member"
			}
			fmt.Fprintf(os.Stderr, "any-agents: %s: %s%s\n", rej.Id, rej.Reason, hint)
			rejected[rej.Id] = true
		}
		var ok []shareItem
		for _, it := range items {
			if !rejected[it.item.Id()] {
				ok = append(ok, it)
			}
		}
		if get("move") == "true" {
			if err := moveToShared(c, st, cwd, scope, sp, ok); err != nil {
				fatal(err)
			}
		} else {
			for _, it := range ok {
				fmt.Printf("%s shared to %s as %s\n", it.item.Slug, sp.Name, it.item.Id())
			}
		}
		refreshProjectCounts(c, sp)
		if len(rejected) > 0 {
			os.Exit(1)
		}
	case "unshare":
		if len(pos) < 1 || get("space") == "" {
			fatal(fmt.Errorf("memory unshare <slug> --space <collab space>"))
		}
		sp := spaceOf(st, get("space"))
		if sp.Kind != kindCollab {
			fatal(fmt.Errorf("%s is the private space — delete the file instead", sp.Name))
		}
		id := scope + ":" + slugify(strings.TrimSuffix(pos[0], ".md"))
		rows, err := c.QueryRecords(sp.Id, sp.owner("app"), sp.coll(dsMemories), map[string]any{"id": id}, nil, 1)
		if err != nil {
			fatal(err)
		}
		if len(rows) == 0 {
			fatal(fmt.Errorf("%s is not shared in %s", id, sp.Name))
		}
		if memItemFromRecord(rows[0]).Kind == kindRetracted {
			fatal(fmt.Errorf("%s is already retracted", id))
		}
		if author, _ := rows[0]["author"].(string); author != "" && author != st.Identity {
			fatal(fmt.Errorf("%s was published by %s; only the publisher can retract it", id, memberIndex(c, sp)[author]))
		}
		// Unsharing moves the memory back home unless told to drop it:
		// after `share --move` the team copy is the only one, and a bare
		// retraction would lose it.
		memDir := currentTool().MemoryDir(cwd)
		item := memItemFromRecord(rows[0])
		target := filepath.Join(memDir, safeFile(item.Slug))
		restored := false
		if get("delete") != "true" {
			if _, err := os.Stat(target); os.IsNotExist(err) {
				if err := os.MkdirAll(memDir, 0o755); err != nil {
					fatal(err)
				}
				if err := os.WriteFile(target, []byte(renderMemFile(item)), 0o644); err != nil {
					fatal(err)
				}
				restored = true
			}
		}
		res, err := c.Upsert(sp.Id, sp.owner("app"), sp.coll(dsMemories), []UpsertRecord{retractRecord(id, shortId(st.Identity))})
		if err != nil {
			fatal(err)
		}
		for _, rej := range res.Rejections {
			fatal(fmt.Errorf("%s: %s", rej.Id, rej.Reason))
		}
		if get("delete") != "true" {
			if err := pushMemory(c, st.private(), cwd); err != nil {
				fatal(err)
			}
		}
		shared := pullSharedMemory(c, st, cwd)
		_, _ = pullMemory(c, st.private(), cwd, false, shared)
		switch {
		case restored:
			fmt.Printf("%s moved back to the private space (%s)\n", id, target)
		case get("delete") == "true":
			fmt.Printf("%s retracted from %s\n", id, sp.Name)
		default:
			fmt.Printf("%s retracted from %s; the private copy stays\n", id, sp.Name)
		}
	default:
		fatal(fmt.Errorf("unknown memory command %q", args[0]))
	}
}

type shareItem struct {
	item *MemItem
	path string // absolute path of the file that was shared
}

// retractRecord is the upsert that unshares or deletes a memory in place.
func retractRecord(id, origin string) UpsertRecord {
	return UpsertRecord{Id: id, Fields: map[string]any{
		"kind": kindRetracted, "title": retractedTitle, "body": "", "links": []string{}, "origin": origin}}
}

// moveToShared completes `memory share --move`: the private records are
// retracted, the private files removed and forgotten by the sync state
// (so no push resurrects them), and the mirror refreshed so the memories
// reappear under memory/shared/<space>/ as this member's editable entries.
func moveToShared(c *Client, st *State, cwd, scope string, to *Space, items []shareItem) error {
	priv := st.private()
	memDir := currentTool().MemoryDir(cwd)
	ss, err := loadSyncState(syncStateName(memDir))
	if err != nil {
		return err
	}
	remote, err := fetchScopeRaw(c, priv, scope)
	if err != nil {
		return err
	}
	origin := shortId(st.Identity)
	var retract []UpsertRecord
	for _, it := range items {
		if r, ok := remote[it.item.Id()]; ok && memItemFromRecord(r).Kind != kindRetracted {
			retract = append(retract, retractRecord(it.item.Id(), origin))
		}
	}
	if len(retract) > 0 {
		res, err := c.Upsert(priv.Id, priv.owner("app"), priv.coll(dsMemories), retract)
		if err != nil {
			return err
		}
		for _, rej := range res.Rejections {
			fmt.Fprintf(os.Stderr, "any-agents: retract %s: %s\n", rej.Id, rej.Reason)
		}
	}
	sharedRoot := filepath.Join(memDir, "shared") + string(filepath.Separator)
	for _, it := range items {
		if strings.HasPrefix(it.path, memDir+string(filepath.Separator)) && !strings.HasPrefix(it.path, sharedRoot) {
			_ = os.Remove(it.path)
		}
		delete(ss.Records, it.item.Id())
		fmt.Printf("%s moved to %s as %s\n", it.item.Slug, to.Name, it.item.Id())
	}
	if err := ss.save(); err != nil {
		return err
	}
	refreshProjectCounts(c, priv)
	shared := pullSharedMemory(c, st, cwd)
	_, err = pullMemory(c, priv, cwd, false, shared)
	return err
}
