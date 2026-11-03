package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config sync mirrors the tool's global config files (its ConfigRoots
// under its Home) through the app root's `config` records: text files
// ride the record body (searchable), binary/large files ride files-v2
// attachments referenced by fileId (kind "blob", no body, so they never
// enter the text index). Records are keyed `<tool>:<path>`, so several
// tools' bundles coexist; settings files with machine-specific hook
// paths are not among the roots.

func configSkipped(rel string) bool {
	for _, d := range currentTool().ConfigSkipDirs() {
		if rel == d || strings.HasPrefix(rel, d+"/") {
			return true
		}
	}
	return false
}

var textExt = map[string]bool{
	".md": true, ".txt": true, ".json": true, ".yaml": true, ".yml": true,
	".sh": true, ".py": true, ".js": true, ".ts": true, ".toml": true, ".css": true, ".html": true,
}

const (
	maxTextSize   = 256 << 10 // beyond this a text file rides as an attachment
	maxBinarySize = 20 << 20
)

func configId(rel string) string {
	return configPrefix() + strings.ReplaceAll(rel, "/", ":")
}

// Keep Claude's existing on-disk baseline. Each other harness compares
// against its own files before propagating changes or deletions.
func configStateName() string {
	if name := currentTool().Name(); name != "claude" {
		return "config-" + name
	}
	return "config"
}

func configPrefix() string { return currentTool().Name() + ":" }

// maxRecordId is the server's record id limit; a config file whose path
// does not fit is not mirrored.
const maxRecordId = 128

type localConf struct {
	rel  string
	data []byte
	text bool
}

// Resolve only the adapter's declared roots on pull as well as push. In
// Codex the same home also contains credentials, transcripts and hooks.
func configPathAllowed(tool Tool, rel string) bool {
	if !filepath.IsLocal(rel) || filepath.ToSlash(filepath.Clean(rel)) != rel {
		return false
	}
	for _, root := range tool.ConfigRoots() {
		if rel == root || strings.HasPrefix(rel, root+"/") {
			return true
		}
	}
	return false
}

// A personal Codex skill may link into a different part of the home,
// including auth.json. Neither upload the target nor write through it.
func codexConfigLink(tool Tool, rel string) bool {
	if tool.Name() != "codex" {
		return false
	}
	for p := filepath.FromSlash(rel); p != "."; p = filepath.Dir(p) {
		fi, err := os.Lstat(filepath.Join(tool.Home(), p))
		if err == nil && fi.Mode()&os.ModeSymlink != 0 {
			return true
		}
	}
	return false
}

// scanConfigBundle collects the sync bundle, keyed by record id.
func scanConfigBundle() (map[string]*localConf, error) {
	out := map[string]*localConf{}
	tool := currentTool()
	base := tool.Home()
	add := func(path string) error {
		rel, _ := filepath.Rel(base, path)
		rel = filepath.ToSlash(rel)
		if codexConfigLink(tool, rel) {
			return nil
		}
		if strings.Contains(rel, ":") { // ':' is our path separator in ids
			fmt.Fprintf(os.Stderr, "any-agents: skip %s (colon in name)\n", rel)
			return nil
		}
		id := configId(rel)
		if len(id) > maxRecordId {
			fmt.Fprintf(os.Stderr, "any-agents: skip %s (path longer than a record id allows)\n", rel)
			return nil
		}
		fi, err := os.Stat(path)
		if err != nil || fi.IsDir() || fi.Size() > maxBinarySize {
			return nil // a symlinked dir is walked as a file: skip it
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := textExt[strings.ToLower(filepath.Ext(path))] && len(data) <= maxTextSize
		out[id] = &localConf{rel: rel, data: data, text: text}
		return nil
	}
	for _, root := range tool.ConfigRoots() {
		p := filepath.Join(base, root)
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if !fi.IsDir() {
			if err := add(p); err != nil {
				return nil, err
			}
			continue
		}
		err = filepath.WalkDir(p, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if rel, _ := filepath.Rel(base, path); configSkipped(filepath.ToSlash(rel)) {
					return filepath.SkipDir
				}
				return nil
			}
			return add(path)
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func fetchConfigRecords(c *Client, sp *Space) (map[string]map[string]any, error) {
	rows, err := c.QueryRecordsAll(sp.Id, sp.owner("app"), sp.coll(dsConfig), nil, []string{"path"})
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]any{}
	for _, r := range rows {
		if kind, _ := r["kind"].(string); kind == kindRetracted {
			continue // a removed file; the id stays reusable
		}
		rel, _ := r["path"].(string)
		if id, ok := r["id"].(string); ok && configPathAllowed(currentTool(), rel) && id == configId(rel) && !codexConfigLink(currentTool(), rel) {
			out[id] = r
		}
	}
	return out, nil
}

// retractConfig marks records of removed files; records are never
// deleted because a deleted id can never be reused.
func retractConfig(c *Client, sp *Space, ids []string) error {
	var recs []UpsertRecord
	for _, id := range ids {
		recs = append(recs, UpsertRecord{Id: id, Fields: map[string]any{"kind": kindRetracted, "body": "", "fileId": "", "sha256": ""}})
	}
	res, err := c.Upsert(sp.Id, sp.owner("app"), sp.coll(dsConfig), recs)
	if err != nil {
		return err
	}
	for _, rej := range res.Rejections {
		fmt.Fprintf(os.Stderr, "any-agents: config retract %s: %s\n", rej.Id, rej.Reason)
	}
	return nil
}

// splitSkipped untracks skipped paths, leaving their local files alone,
// and moves their records out of remote.
func splitSkipped(ss *SyncState, remote map[string]map[string]any) map[string]map[string]any {
	for id, s := range ss.Records {
		if !configPathAllowed(currentTool(), s.File) || configSkipped(s.File) || codexConfigLink(currentTool(), s.File) {
			delete(ss.Records, id)
		}
	}
	skipped := map[string]map[string]any{}
	for id, r := range remote {
		if rel, _ := r["path"].(string); configSkipped(rel) {
			skipped[id] = r
			delete(remote, id)
		}
	}
	return skipped
}

func remoteConfigBytes(c *Client, sp *Space, r map[string]any) ([]byte, error) {
	if body, ok := r["body"].(string); ok && body != "" {
		return []byte(body), nil
	}
	if fid, ok := r["fileId"].(string); ok && fid != "" {
		return c.DownloadFile(sp.Id, fid)
	}
	return []byte{}, nil
}

// pullConfig applies remote config files locally (hash-gated, remote wins
// only over locally-unchanged files — mirror of the memory rules).
func pullConfig(c *Client, sp *Space) (int, error) {
	ss, err := loadSyncState(configStateName())
	if err != nil {
		return 0, err
	}
	local, err := scanConfigBundle()
	if err != nil {
		return 0, err
	}
	remote, err := fetchConfigRecords(c, sp)
	if err != nil {
		return 0, err
	}
	splitSkipped(ss, remote)
	applied := 0
	base := currentTool().Home()
	for id, r := range remote {
		rel, _ := r["path"].(string)
		if rel == "" || strings.Contains(rel, "..") || strings.HasPrefix(rel, "/") {
			continue // never write outside the tool's home
		}
		wantHash, _ := r["sha256"].(string)
		updatedAt := recordFloat(r, "updatedAt")
		l, hasLocal := local[id]
		s, hasState := ss.Records[id]
		localHash := ""
		if hasLocal {
			localHash = contentHash(string(l.data))
		}
		if hasLocal && localHash == wantHash {
			ss.Records[id] = &SyncEntry{File: rel, Hash: wantHash, RemoteUpdatedAt: updatedAt}
			continue
		}
		localChanged := hasLocal && (!hasState || localHash != s.Hash)
		if !hasLocal && hasState && updatedAt <= s.RemoteUpdatedAt+0.5 {
			continue // deleted locally, remote unchanged — push will delete
		}
		if localChanged && hasState && updatedAt <= s.RemoteUpdatedAt+0.5 {
			continue // local edit pending push
		}
		data, err := remoteConfigBytes(c, sp, r)
		if err != nil {
			fmt.Fprintf(os.Stderr, "any-agents: fetch %s: %v\n", rel, err)
			continue
		}
		target := filepath.Join(base, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return applied, err
		}
		if localChanged {
			if codexConfigLink(currentTool(), rel+".conflict") {
				return applied, fmt.Errorf("refusing to write through symlink %s.conflict", rel)
			}
			// The space wins, the local version is kept next to it.
			_ = os.WriteFile(target+".conflict", l.data, 0o644)
			fmt.Fprintf(os.Stderr, "any-agents: config conflict on %s — taking the space's copy, local saved as %s.conflict\n", rel, rel)
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return applied, err
		}
		ss.Records[id] = &SyncEntry{File: rel, Hash: contentHash(string(data)), RemoteUpdatedAt: updatedAt}
		applied++
	}
	// Remote deletions — same safety valve as pullMemory: never let a
	// collapsed remote delete the local config originals.
	if len(ss.Records) > 0 && len(remote)*2 < len(ss.Records) {
		fmt.Fprintf(os.Stderr,
			"any-agents: config remote has %d record(s) but %d were tracked — refusing to delete local config files\n",
			len(remote), len(ss.Records))
		return applied, nil
	}
	for id, s := range ss.Records {
		if _, ok := remote[id]; ok {
			continue
		}
		if l, ok := local[id]; ok && contentHash(string(l.data)) == s.Hash {
			_ = os.Remove(filepath.Join(base, filepath.FromSlash(s.File)))
		}
		delete(ss.Records, id)
	}
	return applied, ss.save()
}

// pushConfig upserts changed bundle files (text inline, binary via
// files-v2 attach) and deletes records for locally-removed files.
func pushConfig(c *Client, sp *Space) error {
	ss, err := loadSyncState(configStateName())
	if err != nil {
		return err
	}
	local, err := scanConfigBundle()
	if err != nil {
		return err
	}
	remote, err := fetchConfigRecords(c, sp)
	if err != nil {
		return err
	}
	root, coll := sp.owner("app"), sp.coll(dsConfig)
	if skipped := splitSkipped(ss, remote); len(skipped) > 0 {
		var ids, files []string
		for id, r := range skipped {
			ids = append(ids, id)
			if fid, _ := r["fileId"].(string); fid != "" {
				files = append(files, fid)
			}
		}
		if err := retractConfig(c, sp, ids); err != nil {
			fmt.Fprintf(os.Stderr, "any-agents: config retract: %v\n", err)
		} else {
			for _, fid := range files {
				_ = c.DeleteFile(sp.Id, fid)
			}
		}
	}
	var records []UpsertRecord
	var staleFiles []string
	attached := map[string]string{} // record id → file attached for it
	for id, l := range local {
		h := contentHash(string(l.data))
		r, known := remote[id]
		if known {
			if rh, _ := r["sha256"].(string); rh == h {
				ss.Records[id] = &SyncEntry{File: l.rel, Hash: h, RemoteUpdatedAt: recordFloat(r, "updatedAt")}
				continue
			}
			if st, tracked := ss.Records[id]; tracked && st.Hash == h {
				continue // unchanged here; the space moved on, pull brings it
			}
		}
		fields := map[string]any{"path": l.rel, "tool": currentTool().Name(), "sha256": h, "kind": "text"}
		if l.text {
			fields["body"] = string(l.data)
			fields["fileId"] = ""
		} else {
			fields["kind"] = "blob"
			fields["body"] = ""
			fid, err := c.AttachFile(sp.Id, root, filepath.Base(l.rel), l.data)
			if err != nil {
				fmt.Fprintf(os.Stderr, "any-agents: attach %s: %v\n", l.rel, err)
				continue
			}
			fields["fileId"] = fid
			attached[id] = fid
			if known {
				if old, _ := r["fileId"].(string); old != "" {
					staleFiles = append(staleFiles, old)
				}
			}
		}
		records = append(records, UpsertRecord{Id: id, Fields: fields})
	}
	if len(records) > 0 {
		res, err := c.Upsert(sp.Id, root, coll, records)
		if err != nil {
			return err
		}
		for _, rej := range res.Rejections {
			fmt.Fprintf(os.Stderr, "any-agents: config push rejected %s: %s\n", rej.Id, rej.Reason)
			if fid := attached[rej.Id]; fid != "" {
				staleFiles = append(staleFiles, fid) // nothing references it
			}
		}
		for _, fid := range staleFiles {
			_ = c.DeleteFile(sp.Id, fid)
		}
	}
	var gone []string
	for id := range ss.Records {
		if _, ok := local[id]; !ok {
			gone = append(gone, id)
		}
	}
	if len(ss.Records) > 0 && len(gone)*2 > len(ss.Records) && len(gone) > 1 {
		fmt.Fprintf(os.Stderr,
			"any-agents: %d of %d tracked config files are missing locally — refusing to delete their records\n",
			len(gone), len(ss.Records))
		gone = nil
	}
	if len(gone) > 0 {
		if err := retractConfig(c, sp, gone); err != nil {
			fmt.Fprintf(os.Stderr, "any-agents: config retract: %v\n", err)
		} else {
			for _, id := range gone {
				delete(ss.Records, id)
			}
		}
	}
	remote, err = fetchConfigRecords(c, sp)
	if err != nil {
		return err
	}
	for id, l := range local {
		if r, ok := remote[id]; ok {
			ss.Records[id] = &SyncEntry{File: l.rel, Hash: contentHash(string(l.data)), RemoteUpdatedAt: recordFloat(r, "updatedAt")}
		}
	}
	return ss.save()
}
