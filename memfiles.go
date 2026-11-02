package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// MemItem is one memory record / one memory .md file.
type MemItem struct {
	Scope string
	Slug  string
	Kind  string
	Title string
	Body  string
	Links []string
}

func (m *MemItem) Id() string { return m.Scope + ":" + m.Slug }

var (
	slugOK      = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)
	slugBad     = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
	fmSplit     = regexp.MustCompile(`(?s)^---\n(.*?)\n---\n?(.*)$`)
	fmName      = regexp.MustCompile(`(?m)^name:\s*(.+)$`)
	fmDesc      = regexp.MustCompile(`(?m)^description:\s*(.+)$`)
	fmKind      = regexp.MustCompile(`(?m)^\s*type:\s*(.+)$`)
	linkPattern = regexp.MustCompile(`\[\[([^\]]+)\]\]`)
)

// slugify maps a frontmatter name to a record-id-safe slug. Names that
// already match the dataset id pattern pass through untouched (keeps ids
// stable with pre-existing records); everything else is folded.
func slugify(name string) string {
	name = strings.TrimSpace(name)
	if slugOK.MatchString(name) && len(name) <= 100 {
		return name
	}
	s := strings.ToLower(strings.Trim(slugBad.ReplaceAllString(name, "-"), "-"))
	if len(s) > 100 {
		s = s[:100]
	}
	if s == "" {
		s = "untitled"
	}
	return s
}

func parseMemFile(path, scope string) (*MemItem, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m := fmSplit.FindStringSubmatch(string(b))
	if m == nil {
		return nil, fmt.Errorf("no frontmatter")
	}
	fm, body := m[1], strings.TrimSpace(m[2])
	item := &MemItem{Scope: scope, Kind: "project", Body: body}
	if n := fmName.FindStringSubmatch(fm); n != nil {
		item.Slug = slugify(n[1])
	} else {
		item.Slug = slugify(strings.TrimSuffix(filepath.Base(path), ".md"))
	}
	if d := fmDesc.FindStringSubmatch(fm); d != nil {
		item.Title = strings.TrimSpace(d[1])
	} else {
		item.Title = item.Slug
	}
	if k := fmKind.FindStringSubmatch(fm); k != nil {
		item.Kind = strings.TrimSpace(k[1])
	}
	links := linkPattern.FindAllStringSubmatch(body, -1)
	seen := map[string]bool{}
	for _, l := range links {
		if !seen[l[1]] {
			seen[l[1]] = true
			item.Links = append(item.Links, l[1])
		}
	}
	sort.Strings(item.Links)
	return item, nil
}

// renderMemFile is the canonical on-disk form; hashing runs over this
// rendering on both sides so local and remote fingerprints compare.
func renderMemFile(m *MemItem) string {
	return fmt.Sprintf("---\nname: %s\ndescription: %s\nmetadata:\n  type: %s\n---\n\n%s\n",
		m.Slug, m.Title, m.Kind, strings.TrimSpace(m.Body))
}

func contentHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func memItemFromRecord(r map[string]any) *MemItem {
	str := func(k string) string { s, _ := r[k].(string); return s }
	m := &MemItem{Scope: str("scope"), Slug: str("slug"), Kind: str("kind"), Title: str("title"), Body: str("body")}
	if arr, ok := r["links"].([]any); ok {
		for _, v := range arr {
			if s, ok := v.(string); ok {
				m.Links = append(m.Links, s)
			}
		}
	}
	if m.Kind == "" {
		m.Kind = "project"
	}
	return m
}

// recordFloat reads a numeric field. Timestamps are any-store instants
// as any-store instants ({"$date": "...ISO..."} or {"$date": millis}), so accept
// both the legacy unix-seconds number and the ext-JSON instant.
func recordFloat(r map[string]any, key string) float64 {
	switch v := r[key].(type) {
	case float64:
		return v
	case map[string]any:
		switch d := v["$date"].(type) {
		case float64:
			return d / 1000
		case string:
			if t, err := time.Parse(time.RFC3339, d); err == nil {
				return float64(t.Unix())
			}
		}
	}
	return 0
}

func (m *MemItem) upsertRecord(origin string) UpsertRecord {
	links := m.Links
	if links == nil {
		links = []string{}
	}
	return UpsertRecord{Id: m.Id(), Fields: map[string]any{
		"scope": m.Scope, "slug": m.Slug, "kind": m.Kind,
		"title": m.Title, "body": m.Body, "links": links, "origin": origin,
	}}
}

// --- project → scope / memory-dir mapping ---

// resolveScope names the project: git origin repo name when available
// (so worktrees and renamed checkouts of one repo share memory),
// directory basename otherwise.
var scopeCache = map[string]string{}

func resolveScope(dir string) string {
	if s, ok := scopeCache[dir]; ok {
		return s
	}
	s := resolveScopeUncached(dir)
	scopeCache[dir] = s
	return s
}

func resolveScopeUncached(dir string) string {
	out, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Output()
	if err == nil {
		u := strings.TrimSpace(string(out))
		u = strings.TrimSuffix(u, ".git")
		if i := strings.LastIndexAny(u, "/:"); i >= 0 {
			u = u[i+1:]
		}
		if u != "" {
			return slugify(u)
		}
	}
	// Fall back to the git toplevel basename, then the dir itself.
	if top, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output(); err == nil {
		if t := strings.TrimSpace(string(top)); t != "" {
			return slugify(filepath.Base(t))
		}
	}
	return slugify(filepath.Base(dir))
}

var kindOrder = map[string]int{"user": 0, "feedback": 1, "project": 2, "reference": 3}

// SharedSet is one collab space's memories for a project: the space's
// dir under memory/shared/ and the items.
type SharedSet struct {
	Space string
	Dir   string
	Items []*MemItem
}

// renderSharedMemFile is the on-disk form of a memory pulled from a
// collab space: the private rendering plus the space it came from.
func renderSharedMemFile(m *MemItem, space string) string {
	return fmt.Sprintf("---\nname: %s\ndescription: %s\nmetadata:\n  type: %s\n  shared: %s\n---\n\n%s\n",
		m.Slug, m.Title, m.Kind, space, strings.TrimSpace(m.Body))
}

// renderMemoryIndex regenerates MEMORY.md from the synced set and the
// shared sets.
func renderMemoryIndex(items []*MemItem, files map[string]string, shared []SharedSet) string {
	sorted := append([]*MemItem(nil), items...)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		ka, kb := 9, 9
		if v, ok := kindOrder[a.Kind]; ok {
			ka = v
		}
		if v, ok := kindOrder[b.Kind]; ok {
			kb = v
		}
		if ka != kb {
			return ka < kb
		}
		return a.Slug < b.Slug
	})
	var b strings.Builder
	b.WriteString("# Memory index\n\n")
	for _, m := range sorted {
		file := files[m.Id()]
		if file == "" {
			file = m.Slug + ".md"
		}
		fmt.Fprintf(&b, "- [%s](%s) — %s\n", m.Slug, file, m.Title)
	}
	for _, ss := range shared {
		if len(ss.Items) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n## Shared by %s (your own entries are editable and sync on push; other members' are read-only)\n\n", ss.Space)
		sorted := append([]*MemItem(nil), ss.Items...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].Slug < sorted[j].Slug })
		for _, m := range sorted {
			file := safeFile(m.Slug)
			fmt.Fprintf(&b, "- [%s](shared/%s/%s) — %s\n", strings.TrimSuffix(file, ".md"), ss.Dir, file, m.Title)
		}
	}
	return b.String()
}
