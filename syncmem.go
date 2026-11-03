package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type localMem struct {
	item *MemItem
	file string // basename within the memory dir
	hash string // hash of the normalized rendering
}

// scanMemoryDir parses every memory file, keyed by record id. Files that
// fail to parse are skipped (never deleted, never pushed).
func scanMemoryDir(dir, scope string) (map[string]*localMem, error) {
	out := map[string]*localMem{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") || e.Name() == "MEMORY.md" {
			continue
		}
		item, err := parseMemFile(filepath.Join(dir, e.Name()), scope)
		if err != nil {
			fmt.Fprintf(os.Stderr, "any-agents: skip %s: %v\n", e.Name(), err)
			continue
		}
		if prev, dup := out[item.Id()]; dup {
			fmt.Fprintf(os.Stderr, "any-agents: %s and %s share id %s — keeping %s\n",
				prev.file, e.Name(), item.Id(), prev.file)
			continue
		}
		out[item.Id()] = &localMem{item: item, file: e.Name(), hash: contentHash(renderMemFile(item))}
	}
	return out, nil
}

// fetchScope reads a scope's live memory records; a retracted record (a
// deleted file, an unshared memory) counts as absent. A record id can
// never be reused once deleted, so nothing here ever deletes one.
func fetchScope(c *Client, sp *Space, scope string) (map[string]map[string]any, error) {
	out, err := fetchScopeRaw(c, sp, scope)
	if err != nil {
		return nil, err
	}
	for id, r := range out {
		if kind, _ := r["kind"].(string); kind == kindRetracted {
			delete(out, id)
		}
	}
	return out, nil
}

func fetchScopeRaw(c *Client, sp *Space, scope string) (map[string]map[string]any, error) {
	rows, err := c.QueryRecordsAll(sp.Id, sp.owner("app"), sp.coll(dsMemories),
		map[string]any{"scope": scope}, []string{"slug"})
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]any{}
	for _, r := range rows {
		if id, ok := r["id"].(string); ok {
			out[id] = r
		}
	}
	return out, nil
}

// safeFile maps a record's slug (another member's free text) to a file
// name that stays inside the memory dir.
func safeFile(slug string) string {
	s := slugify(slug)
	s = strings.ReplaceAll(strings.ReplaceAll(s, "/", "-"), "\\", "-")
	s = strings.Trim(s, ".-")
	if s == "" {
		s = "untitled"
	}
	return s + ".md"
}

func syncStateName(memDir string) string {
	return "mem-" + filepath.Base(filepath.Dir(memDir))
}

// pullMemory mirrors the remote scope into the project's memory dir.
// Remote wins over locally-unchanged files; locally-changed files are
// left for push; genuine two-sided conflicts resolve newest-wins with a
// .conflict backup of the losing local version.
func pullMemory(c *Client, sp *Space, projectDir string, allowDeletions bool, shared []SharedSet) (int, error) {
	scope := resolveScope(projectDir)
	memDir := currentTool().MemoryDir(projectDir)
	ss, err := loadSyncState(syncStateName(memDir))
	if err != nil {
		return 0, err
	}
	local, err := scanMemoryDir(memDir, scope)
	if err != nil {
		return 0, err
	}
	remote, err := fetchScope(c, sp, scope)
	if err != nil {
		return 0, err
	}
	bumpStat("pulls", 1)
	if len(remote) > 0 {
		if err := os.MkdirAll(memDir, 0o755); err != nil {
			return 0, err
		}
	}
	indexItems := make([]*MemItem, 0, len(remote))
	indexFiles := map[string]string{}
	writeLocal := func(id, file, content string, updatedAt float64) error {
		if err := os.WriteFile(filepath.Join(memDir, file), []byte(content), 0o644); err != nil {
			return err
		}
		ss.Records[id] = &SyncEntry{File: file, Hash: contentHash(content), RemoteUpdatedAt: updatedAt}
		return nil
	}
	for id, r := range remote {
		item := memItemFromRecord(r)
		updatedAt := recordFloat(r, "updatedAt")
		want := renderMemFile(item)
		wantHash := contentHash(want)
		indexItems = append(indexItems, item)
		l, hasLocal := local[id]
		s, hasState := ss.Records[id]
		switch {
		case !hasLocal && !hasState: // new from remote
			file := safeFile(item.Slug)
			if err := writeLocal(id, file, want, updatedAt); err != nil {
				return 0, err
			}
			indexFiles[id] = file
		case !hasLocal && hasState: // deleted locally since last sync
			if updatedAt > s.RemoteUpdatedAt+0.5 { // remote edited after our delete → resurrect
				if err := writeLocal(id, s.File, want, updatedAt); err != nil {
					return 0, err
				}
				indexFiles[id] = s.File
			}
			// else: pending local delete — push will remove the record
		default:
			indexFiles[id] = l.file
			if l.hash == wantHash {
				ss.Records[id] = &SyncEntry{File: l.file, Hash: wantHash, RemoteUpdatedAt: updatedAt}
				continue
			}
			localChanged := !hasState || l.hash != s.Hash
			remoteChanged := !hasState || updatedAt > s.RemoteUpdatedAt+0.5
			switch {
			case !localChanged: // remote wins
				if err := writeLocal(id, l.file, want, updatedAt); err != nil {
					return 0, err
				}
			case !remoteChanged: // local wins — push later
			default: // both changed: newest wins, keep the loser as .conflict
				bumpStat("conflicts", 1)
				fi, _ := os.Stat(filepath.Join(memDir, l.file))
				if fi != nil && float64(fi.ModTime().Unix()) >= updatedAt {
					fmt.Fprintf(os.Stderr, "any-agents: conflict on %s — keeping newer local copy\n", id)
					continue
				}
				_ = os.Rename(filepath.Join(memDir, l.file), filepath.Join(memDir, l.file+".conflict"))
				fmt.Fprintf(os.Stderr, "any-agents: conflict on %s — remote newer, local saved as %s.conflict\n", id, l.file)
				if err := writeLocal(id, l.file, want, updatedAt); err != nil {
					return 0, err
				}
			}
		}
	}
	// Remote deletions: previously-synced ids gone from the space.
	// SAFETY VALVE: a remote that lost most of its records is a broken
	// remote (wiped data dir, missing dataset definition, un-converged
	// space), not a user deleting everything. Honoring it would delete
	// the local originals — which is exactly how the 2026-08 wipe
	// destroyed two scopes. Skip the deletion pass and keep the files;
	// a genuine bulk delete is re-applied once the remote is healthy
	// (or with `any-agents pull --allow-deletions`).
	if len(ss.Records) > 0 && len(remote)*2 < len(ss.Records) && !allowDeletions {
		fmt.Fprintf(os.Stderr,
			"any-agents: remote has %d record(s) but %d were tracked in scope %q — refusing to delete local files (pass --allow-deletions if this is intentional)\n",
			len(remote), len(ss.Records), scope)
		if len(indexItems) > 0 || len(shared) > 0 {
			idx := renderMemoryIndex(indexItems, indexFiles, shared)
			_ = os.WriteFile(filepath.Join(memDir, "MEMORY.md"), []byte(idx), 0o644)
		}
		return len(indexItems), nil // keep sync state intact for the next healthy pull
	}
	for id, s := range ss.Records {
		if _, ok := remote[id]; ok {
			continue
		}
		if l, ok := local[id]; ok {
			if l.hash == s.Hash {
				_ = os.Remove(filepath.Join(memDir, l.file))
			} else {
				fmt.Fprintf(os.Stderr, "any-agents: %s deleted remotely but changed locally — keeping local file (will re-push)\n", id)
				indexItems = append(indexItems, l.item)
				indexFiles[id] = l.file
			}
		}
		delete(ss.Records, id)
	}
	// Local-only new files stay in the index until pushed.
	for id, l := range local {
		if _, ok := remote[id]; ok {
			continue
		}
		if _, tracked := ss.Records[id]; !tracked {
			indexItems = append(indexItems, l.item)
			indexFiles[id] = l.file
		}
	}
	// The index is written when there is something to list, and rewritten
	// when it exists and the last entry went, so it never links to files
	// the pull removed.
	if _, err := os.Stat(filepath.Join(memDir, "MEMORY.md")); len(indexItems) > 0 || len(shared) > 0 || err == nil {
		if err := os.MkdirAll(memDir, 0o755); err != nil {
			return 0, err
		}
		idx := renderMemoryIndex(indexItems, indexFiles, shared)
		if err := os.WriteFile(filepath.Join(memDir, "MEMORY.md"), []byte(idx), 0o644); err != nil {
			return 0, err
		}
	}
	return len(indexItems), ss.save()
}

// sharedDirName is the mirror dir of a collab space under memory/shared/:
// the name folded to [a-z0-9-] plus the id's tail, so two spaces never
// share a dir and no name can escape the tree.
func sharedDirName(sp *Space) string {
	name := strings.ToLower(sp.Name)
	var b strings.Builder
	for _, r := range name {
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
	id := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return -1
	}, sp.Id)
	if len(id) > 6 {
		id = id[len(id)-6:]
	}
	return folded + "-" + id
}

func sharedStateName(memDir, dir string) string {
	return syncStateName(memDir) + "-shared-" + dir
}

// ownFile reports whether a mirror file is this member's own entry:
// own entries are written writable, other members' entries read-only.
func ownFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode()&0o200 != 0
}

// pullSharedMemory mirrors every collab space's memories for the project
// scope into memory/shared/<space>/. Another member's record is a
// read-only file the space always wins; this member's own records are
// editable files synced like private ones (remote wins over an unchanged
// file, a changed file waits for push, two-sided edits newest-wins with a
// .conflict copy). Files whose records are gone are removed, except an
// own file never pushed yet. A record that also exists as a private file
// is skipped: the private copy wins. Returns the sets for the index.
// Never fatal: a space that fails is skipped.
func pullSharedMemory(c *Client, st *State, projectDir string) []SharedSet {
	scope := resolveScope(projectDir)
	memDir := currentTool().MemoryDir(projectDir)
	private, _ := scanMemoryDir(memDir, scope)
	var out []SharedSet
	for _, sp := range st.collabs() {
		if !sp.ready() {
			continue
		}
		remote, err := fetchScope(c, sp, scope)
		if err != nil {
			fmt.Fprintf(os.Stderr, "any-agents: shared memories %s: %v\n", sp.Name, err)
			continue
		}
		set := SharedSet{Space: sp.Name, Dir: sharedDirName(sp)}
		dir := filepath.Join(memDir, "shared", set.Dir)
		ss, err := loadSyncState(sharedStateName(memDir, set.Dir))
		if err != nil {
			fmt.Fprintf(os.Stderr, "any-agents: shared memories %s: %v\n", sp.Name, err)
			continue
		}
		tracked := map[string]bool{}
		for _, s := range ss.Records {
			tracked[s.File] = true
		}
		want := map[string]bool{}
		if len(remote) > 0 {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				continue
			}
		}
		dup := 0
		for id, r := range remote {
			item := memItemFromRecord(r)
			if item.Kind == kindRetracted {
				continue
			}
			file := safeFile(item.Slug)
			target := filepath.Join(dir, file)
			if _, ok := private[id]; ok {
				dup++
				// an own mirror file with unpushed edits is never dropped
				if s, tracked := ss.Records[id]; tracked && ownFile(target) {
					if cur, err := parseMemFile(target, scope); err == nil && contentHash(renderSharedMemFile(cur, sp.Name)) != s.Hash {
						want[file] = true
						fmt.Fprintf(os.Stderr, "any-agents: shared %s has unpushed edits and a private copy; keeping both\n", id)
					}
				}
				continue
			}
			set.Items = append(set.Items, item)
			want[file] = true
			content := renderSharedMemFile(item, sp.Name)
			if author, _ := r["author"].(string); author == st.Identity {
				syncOwnShared(ss, id, target, content, recordFloat(r, "updatedAt"), scope, sp.Name)
				continue
			}
			delete(ss.Records, id)
			if cur, err := os.ReadFile(target); err != nil || string(cur) != content {
				_ = os.Chmod(target, 0o644) // a previous mirror is read-only
				_ = os.WriteFile(target, []byte(content), 0o444)
			}
			// WriteFile keeps an existing file's mode, and a writable file
			// counts as this member's own (pushed, editable)
			_ = os.Chmod(target, 0o444)
		}
		for id := range ss.Records {
			if r, ok := remote[id]; !ok || memItemFromRecord(r).Kind == kindRetracted {
				delete(ss.Records, id)
			}
		}
		if entries, err := os.ReadDir(dir); err == nil {
			pending := 0
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") || want[e.Name()] {
					continue
				}
				p := filepath.Join(dir, e.Name())
				if ownFile(p) && !tracked[e.Name()] {
					pending++ // this member's own file, not pushed yet
					if item, err := parseMemFile(p, scope); err == nil && safeFile(item.Slug) == e.Name() {
						set.Items = append(set.Items, item) // listed before its push, like a private file
					}
					continue
				}
				_ = os.Remove(p)
			}
			if len(set.Items) == 0 && pending == 0 {
				_ = os.Remove(dir)
			}
		}
		if len(ss.Records) > 0 || len(tracked) > 0 {
			_ = ss.save()
		}
		if dup > 0 {
			fmt.Fprintf(os.Stderr, "any-agents: %d shared memories in %s also exist as private files; the private copies win (`memory share --move` keeps one)\n", dup, sp.Name)
		}
		if len(set.Items) > 0 {
			out = append(out, set)
		}
	}
	return out
}

// syncOwnShared brings one of this member's own shared memories down with
// the private three-way rule: remote wins over an unchanged file, a
// changed file waits for push, two-sided edits newest-wins with a
// .conflict copy of the loser.
func syncOwnShared(ss *SyncState, id, target, want string, updatedAt float64, scope, space string) {
	file := filepath.Base(target)
	wantHash := contentHash(want)
	write := func() {
		_ = os.Chmod(target, 0o644)
		if err := os.WriteFile(target, []byte(want), 0o644); err == nil {
			ss.Records[id] = &SyncEntry{File: file, Hash: wantHash, RemoteUpdatedAt: updatedAt}
		}
	}
	cur, err := parseMemFile(target, scope)
	if err != nil {
		write() // missing or unparseable: the space wins
		return
	}
	_ = os.Chmod(target, 0o644) // mirrored read-only before it was synced as own
	curHash := contentHash(renderSharedMemFile(cur, space))
	if curHash == wantHash {
		ss.Records[id] = &SyncEntry{File: file, Hash: wantHash, RemoteUpdatedAt: updatedAt}
		return
	}
	s, hasState := ss.Records[id]
	localChanged := !hasState || curHash != s.Hash
	remoteChanged := !hasState || updatedAt > s.RemoteUpdatedAt+0.5
	switch {
	case !localChanged:
		write()
	case !remoteChanged: // local wins — push later
	default:
		bumpStat("conflicts", 1)
		if fi, _ := os.Stat(target); fi != nil && float64(fi.ModTime().Unix()) >= updatedAt {
			fmt.Fprintf(os.Stderr, "any-agents: conflict on shared %s — keeping newer local copy\n", id)
			return
		}
		_ = os.Rename(target, target+".conflict")
		fmt.Fprintf(os.Stderr, "any-agents: conflict on shared %s — remote newer, local saved as %s.conflict\n", id, file)
		write()
	}
}

// pushSharedMemory publishes this member's own files under
// memory/shared/<space>/: entries changed since the last sync and new
// files written there (a file dropped into the mirror is a share). Other
// members' read-only files are never pushed. Deleting an own file does
// not retract its record (pull brings it back; `memory unshare` retracts).
func pushSharedMemory(c *Client, st *State, projectDir string) error {
	scope := resolveScope(projectDir)
	memDir := currentTool().MemoryDir(projectDir)
	origin := shortId(st.Identity)
	var firstErr error
	for _, sp := range st.collabs() {
		if !sp.ready() {
			continue
		}
		dirName := sharedDirName(sp)
		dir := filepath.Join(memDir, "shared", dirName)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		ss, err := loadSyncState(sharedStateName(memDir, dirName))
		if err != nil {
			return err
		}
		var records []UpsertRecord
		files := map[string]string{}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			p := filepath.Join(dir, e.Name())
			if !ownFile(p) {
				continue
			}
			item, err := parseMemFile(p, scope)
			if err != nil {
				fmt.Fprintf(os.Stderr, "any-agents: skip %s: %v\n", p, err)
				continue
			}
			if s, ok := ss.Records[item.Id()]; ok && s.Hash == contentHash(renderSharedMemFile(item, sp.Name)) {
				continue
			}
			records = append(records, item.upsertRecord(origin))
			files[item.Id()] = e.Name()
		}
		if len(records) == 0 {
			continue
		}
		res, err := c.Upsert(sp.Id, sp.owner("app"), sp.coll(dsMemories), records)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		rejected := map[string]bool{}
		for _, rej := range res.Rejections {
			rejected[rej.Id] = true
			hint := ""
			if strings.Contains(rej.Reason, "author") {
				hint = " — published by another member"
			}
			fmt.Fprintf(os.Stderr, "any-agents: shared push %s: %s%s\n", rej.Id, rej.Reason, hint)
		}
		bumpStat("memSynced", res.Created+res.Updated)
		bumpStat("memRejected", len(res.Rejections))
		remote, err := fetchScope(c, sp, scope)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for id, file := range files {
			if r, ok := remote[id]; ok && !rejected[id] {
				ss.Records[id] = &SyncEntry{File: file,
					Hash: contentHash(renderSharedMemFile(memItemFromRecord(r), sp.Name)), RemoteUpdatedAt: recordFloat(r, "updatedAt")}
			}
		}
		if err := ss.save(); err != nil {
			return err
		}
	}
	return firstErr
}

// pushMemory upserts every parseable local memory file (the server skips
// identical records) and deletes records whose files were removed locally.
func pushMemory(c *Client, sp *Space, projectDir string) error {
	scope := resolveScope(projectDir)
	memDir := currentTool().MemoryDir(projectDir)
	ss, err := loadSyncState(syncStateName(memDir))
	if err != nil {
		return err
	}
	local, err := scanMemoryDir(memDir, scope)
	if err != nil {
		return err
	}
	bumpStat("pushes", 1)
	origin, _ := os.Hostname()
	// Push only files this machine changed since its last sync, or never
	// synced: a tracked file whose hash still equals the fingerprint was
	// merely pulled, and pushing it would overwrite a newer edit from
	// another machine with the stale copy (pull brings the newer one).
	remoteBefore, err := fetchScope(c, sp, scope)
	if err != nil {
		return err
	}
	var records []UpsertRecord
	for id, l := range local {
		if r, ok := remoteBefore[id]; ok {
			if l.hash == contentHash(renderMemFile(memItemFromRecord(r))) {
				continue
			}
		}
		if st, tracked := ss.Records[id]; tracked && st.Hash == l.hash {
			continue // unchanged here; the remote moved on
		}
		records = append(records, l.item.upsertRecord(origin))
	}
	if len(records) > 0 {
		res, err := c.Upsert(sp.Id, sp.owner("app"), sp.coll(dsMemories), records)
		if err != nil {
			return err
		}
		bumpStat("memSynced", res.Created+res.Updated)
		bumpStat("memRejected", len(res.Rejections))
		for _, rej := range res.Rejections {
			fmt.Fprintf(os.Stderr, "any-agents: push rejected %s: %s\n", rej.Id, rej.Reason)
		}
	}
	// Deletions: tracked ids with no local file. Mirror of the pull-side
	// safety valve — if most tracked files vanished at once, the local
	// dir is broken (wiped tool home, failed pull), not a bulk delete;
	// never propagate that to the space.
	var gone []string
	for id := range ss.Records {
		if _, ok := local[id]; !ok {
			gone = append(gone, id)
		}
	}
	if len(ss.Records) > 0 && len(gone)*2 > len(ss.Records) && len(gone) > 3 {
		fmt.Fprintf(os.Stderr,
			"any-agents: %d of %d tracked memory files are missing locally in scope %q — refusing to delete their records\n",
			len(gone), len(ss.Records), scope)
		gone = nil
	}
	if len(gone) > 0 {
		// Retract rather than delete: a deleted record id can never be
		// reused, a retracted one takes the file back when it reappears.
		var retract []UpsertRecord
		for _, id := range gone {
			retract = append(retract, UpsertRecord{Id: id, Fields: map[string]any{
				"kind": kindRetracted, "title": retractedTitle, "body": "", "links": []string{}, "origin": origin}})
		}
		if res, err := c.Upsert(sp.Id, sp.owner("app"), sp.coll(dsMemories), retract); err != nil {
			fmt.Fprintf(os.Stderr, "any-agents: retract: %v\n", err)
		} else {
			for _, rej := range res.Rejections {
				fmt.Fprintf(os.Stderr, "any-agents: retract %s: %s\n", rej.Id, rej.Reason)
			}
			for _, id := range gone {
				delete(ss.Records, id)
			}
		}
	}
	// Refresh fingerprints from the space so the next pull sees us in sync.
	remote, err := fetchScope(c, sp, scope)
	if err != nil {
		return err
	}
	if pid, err := ensureProject(c, sp, scope); err == nil {
		_ = c.SetProperties(sp.Id, pid, sp.owner("project"), props(sp, "project", map[string]any{
			"memories": len(remote), "lastActivity": dateVal(time.Now())}))
	}
	for id, l := range local {
		if r, ok := remote[id]; ok {
			ss.Records[id] = &SyncEntry{File: l.file, Hash: l.hash, RemoteUpdatedAt: recordFloat(r, "updatedAt")}
		}
	}
	return ss.save()
}

// refreshProjectCounts writes every project's memory count from the
// records (after a bulk import or a share into the space).
func refreshProjectCounts(c *Client, sp *Space) {
	rows, err := c.Aggregate(sp.Id, sp.owner("app"), sp.coll(dsMemories), []map[string]any{
		{"$match": map[string]any{"kind": map[string]any{"$ne": kindRetracted}}},
		{"$group": map[string]any{"_id": "$scope", "n": map[string]any{"$sum": 1}, "last": map[string]any{"$max": "$updatedAt"}}},
	})
	if err != nil {
		return
	}
	for _, r := range rows {
		scope, _ := r["id"].(string)
		if scope == "" {
			continue
		}
		pid, err := ensureProject(c, sp, scope)
		if err != nil {
			continue
		}
		_ = c.SetProperties(sp.Id, pid, sp.owner("project"), props(sp, "project", map[string]any{
			"memories": int(recordFloat(r, "n")), "lastActivity": dateVal(time.Unix(int64(recordFloat(r, "last")), 0))}))
	}
}
