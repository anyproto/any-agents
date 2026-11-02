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
// mirrored read-only into every member's memory/shared/<space>/ for that
// project at session start, listed in MEMORY.md and found by recall. A
// record belongs to whoever published it: another member's publish of
// the same slug is refused by the dataset's author rule. Unshare retracts
// (kind "retracted", empty body) rather than deletes: a deleted record id
// can never be reused, a retracted one can be published again.

// kindRetracted marks an unshared record: skipped by every reader, its
// id stays reusable by the publisher. The title is cleared too, so the
// text index no longer matches it.
const (
	kindRetracted  = "retracted"
	retractedTitle = "(retracted)"
)

func cmdMemory(args []string) {
	if len(args) == 0 {
		fatal(fmt.Errorf("usage: memory share <slug|file.md> --space S | list [--space S] | unshare <slug> --space S"))
	}
	c := NewClient()
	st := mustSetup(c)
	fs, pos := parseArgs("memory "+args[0], args[1:], func(fs *flag.FlagSet) {
		spaceFlag(fs)
		fs.String("scope", "", "project scope (default: the cwd repo)")
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
			fatal(fmt.Errorf("memory share <slug|file.md> --space <collab space>"))
		}
		sp := spaceOf(st, get("space"))
		if sp.Kind != kindCollab {
			fatal(fmt.Errorf("%s is the private space — memories live there already", sp.Name))
		}
		path := pos[0]
		if fi, err := os.Stat(path); err != nil || !fi.Mode().IsRegular() {
			path = filepath.Join(currentTool().MemoryDir(cwd), strings.TrimSuffix(pos[0], ".md")+".md")
		}
		item, err := parseMemFile(path, scope)
		if err != nil {
			fatal(fmt.Errorf("%s: %v", path, err))
		}
		if home, _ := os.UserHomeDir(); home != "" && strings.Contains(item.Body, home) {
			fmt.Fprintf(os.Stderr, "any-agents: note: %s mentions your home directory; every member of %s will read it\n", item.Slug, sp.Name)
		}
		res, err := c.Upsert(sp.Id, sp.owner("app"), sp.coll(dsMemories), []UpsertRecord{item.upsertRecord(shortId(st.Identity))})
		if err != nil {
			fatal(err)
		}
		for _, rej := range res.Rejections {
			hint := ""
			if strings.Contains(rej.Reason, "author") {
				hint = " — a memory with this slug was published by another member"
			}
			fatal(fmt.Errorf("%s: %s%s", rej.Id, rej.Reason, hint))
		}
		fmt.Printf("%s shared to %s as %s\n", item.Slug, sp.Name, item.Id())
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
		res, err := c.Upsert(sp.Id, sp.owner("app"), sp.coll(dsMemories), []UpsertRecord{{Id: id, Fields: map[string]any{
			"kind": kindRetracted, "title": retractedTitle, "body": "", "links": []string{}, "origin": shortId(st.Identity)}}})
		if err != nil {
			fatal(err)
		}
		for _, rej := range res.Rejections {
			fatal(fmt.Errorf("%s: %s", rej.Id, rej.Reason))
		}
		fmt.Printf("%s retracted from %s\n", id, sp.Name)
	default:
		fatal(fmt.Errorf("unknown memory command %q", args[0]))
	}
}
