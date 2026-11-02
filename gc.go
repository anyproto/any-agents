package main

import (
	"flag"
	"fmt"
	"strings"
)

// gc reconciles the space's file attachments against what objects and
// records reference: an orphan transcript is relinked to its session
// object when the object lost the link (a SessionEnd hook killed between
// attach and property write), everything else unreferenced is deleted.
// Dry-run by default; --apply executes.
func cmdGC(args []string) {
	fs, _ := parseArgs("gc", args, func(fs *flag.FlagSet) {
		fs.Bool("apply", false, "execute (default: dry-run report)")
	})
	apply := fs.Lookup("apply").Value.String() == "true"
	c := NewClient()
	sp := mustSetup(c).private()
	files, err := c.FileList(sp.Id)
	if err != nil {
		fatal(err)
	}
	referenced := map[string]bool{}
	sessions, err := objectsOf(c, sp, "session", nil)
	if err != nil {
		fatal(err)
	}
	byShort := map[string]map[string]any{}
	t := sp.owner("session")
	for _, r := range sessions {
		if fid := fileIdOf(strOf(r, t, sp.prop("session", "transcript"))); fid != "" {
			referenced[fid] = true
		}
		if sid := strOf(r, t, sp.prop("session", "sessionId")); sid != "" {
			byShort[shortId(sid)] = r
		}
	}
	rows, err := c.QueryRecordsAll(sp.Id, sp.owner("app"), sp.coll(dsConfig), nil, nil)
	if err != nil {
		fatal(err)
	}
	for _, r := range rows {
		if fid, _ := r["fileId"].(string); fid != "" {
			referenced[fid] = true
		}
	}
	// Files linked from doc bodies (attachments) are referenced by URI.
	docs, _ := objectsOf(c, sp, "doc", nil)
	for _, d := range docs {
		body, err := c.MarkdownGet(sp.Id, d["id"].(string))
		if err != nil {
			continue
		}
		for _, f := range files {
			if strings.Contains(body, f.FileId) {
				referenced[f.FileId] = true
			}
		}
	}
	relinked, deleted, kept := 0, 0, 0
	for _, f := range files {
		if referenced[f.FileId] {
			kept++
			continue
		}
		if strings.HasPrefix(f.Name, "transcript-") {
			short := strings.TrimSuffix(strings.TrimPrefix(f.Name, "transcript-"), ".jsonl.gz")
			if r, ok := byShort[short]; ok && strOf(r, t, sp.prop("session", "transcript")) == "" {
				fmt.Printf("relink  %s (%s) → session %s\n", f.Name, f.FileId, nameOf(r))
				if apply {
					if err := c.SetProperties(sp.Id, r["id"].(string), t, props(sp, "session",
						map[string]any{"transcript": fileLink(sp.Id, f.FileId)})); err != nil {
						fmt.Printf("  relink failed: %v\n", err)
						continue
					}
				}
				relinked++
				continue
			}
		}
		if !strings.HasPrefix(f.Name, "transcript-") {
			kept++ // a human's attachment (an image in a page): never ours to delete
			continue
		}
		fmt.Printf("delete  %s (%s, %d B) — unreferenced\n", f.Name, f.FileId, f.Size)
		if apply {
			if err := c.DeleteFile(sp.Id, f.FileId); err != nil {
				fmt.Printf("  delete failed: %v\n", err)
				continue
			}
		}
		deleted++
	}
	mode := "dry-run (pass --apply to execute)"
	if apply {
		mode = "applied"
	}
	fmt.Printf("gc %s: %d files kept, %d relinked, %d deleted\n", mode, kept, relinked, deleted)
}
