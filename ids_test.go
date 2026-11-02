package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseObjectId(t *testing.T) {
	cases := map[string]string{
		"bafyabc":                      "bafyabc",
		"any://bafyabc":                "bafyabc",
		"any://o/space.x/bafyabc":      "bafyabc",
		"any://o/space.x/bafyabc/ds/r": "bafyabc",
		" any://bafyabc#turn_3 ":       "bafyabc",
	}
	for in, want := range cases {
		if got := parseObjectId(in); got != want {
			t.Errorf("parseObjectId(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLexPosOrder(t *testing.T) {
	prev := ""
	for i := 0; i < 25; i++ {
		p := lexPos(i)
		if p <= prev {
			t.Fatalf("lexPos(%d)=%q not after %q", i, p, prev)
		}
		prev = p
	}
	a := nextPos("")
	b := nextPos(a)
	if !(a < b) {
		t.Fatalf("nextPos not increasing: %q %q", a, b)
	}
}

func TestAddToLinks(t *testing.T) {
	got := addToLinks([]string{"a"}, "b")
	if len(got) != 2 || got[0] != "any://a" || got[1] != "any://b" {
		t.Fatalf("got %v", got)
	}
	if got := addToLinks([]string{"a", "b"}, "a"); len(got) != 2 {
		t.Fatalf("dedupe failed: %v", got)
	}
}

func TestMemFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	src := "---\nname: My Fact\ndescription: what it says\nmetadata:\n  type: feedback\n---\n\nBody with [[other-mem]] and [[zeta]].\n"
	path := filepath.Join(dir, "x.md")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := parseMemFile(path, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if m.Id() != "repo:my-fact" || m.Kind != "feedback" || m.Title != "what it says" {
		t.Fatalf("parsed %+v", m)
	}
	if len(m.Links) != 2 || m.Links[0] != "other-mem" {
		t.Fatalf("links %v", m.Links)
	}
	// The canonical rendering hashes the same on both sides.
	if contentHash(renderMemFile(m)) != contentHash(renderMemFile(memItemFromRecord(m.upsertRecord("h").Fields))) {
		t.Fatal("render/record hash mismatch")
	}
}

func TestTitleOf(t *testing.T) {
	if got := titleOf("", "intro\n# The Title\ntext", "fb"); got != "The Title" {
		t.Fatalf("got %q", got)
	}
	if got := titleOf("flag", "# x", "fb"); got != "flag" {
		t.Fatalf("got %q", got)
	}
	if got := titleOf("", "no heading", "fb"); got != "fb" {
		t.Fatalf("got %q", got)
	}
}

func TestObjectLinkKeepsLinks(t *testing.T) {
	for in, want := range map[string]string{
		"bafyabc":            "any://bafyabc",
		"any://bafyabc":      "any://bafyabc",
		"any://u/A9identity": "any://u/A9identity",
	} {
		if got := objectLink(in); got != want {
			t.Errorf("objectLink(%q) = %q, want %q", in, got, want)
		}
	}
	got := addToLinks([]string{"any://u/A"}, "any://u/B")
	if len(got) != 2 || got[0] != "any://u/A" || got[1] != "any://u/B" {
		t.Errorf("addToLinks member links = %v", got)
	}
	if got := addToLinks([]string{"any://u/A"}, "any://u/A"); len(got) != 1 {
		t.Errorf("addToLinks dedup = %v", got)
	}
}

func TestLinksOfKeepsMemberLinks(t *testing.T) {
	row := map[string]any{"coll": map[string]any{
		"rel": []any{"any://bafyobj"},
		"mem": []any{"any://u/A9identity"},
	}}
	if got := linksOf(row, "coll", "rel"); len(got) != 1 || got[0] != "bafyobj" {
		t.Errorf("relation = %v", got)
	}
	if got := linksOf(row, "coll", "mem"); len(got) != 1 || got[0] != "any://u/A9identity" {
		t.Errorf("member = %v", got)
	}
}

func TestExtractSpace(t *testing.T) {
	space, rest := extractSpace([]string{"add", "--space", "team", "f.md", "--kind", "note"})
	if space != "team" || len(rest) != 4 || rest[0] != "add" || rest[1] != "f.md" {
		t.Errorf("got %q %v", space, rest)
	}
	space, rest = extractSpace([]string{"list", "--space=Team"})
	if space != "Team" || len(rest) != 1 {
		t.Errorf("got %q %v", space, rest)
	}
	if space, _ := extractSpace([]string{"list"}); space != "" {
		t.Errorf("got %q", space)
	}
}

func TestParseSpaceId(t *testing.T) {
	if got := parseSpaceId("any://o/spaceA.x/objB"); got != "spaceA.x" {
		t.Errorf("link: %q", got)
	}
	if got := parseSpaceId("spaceA.x"); got != "spaceA.x" {
		t.Errorf("bare: %q", got)
	}
}

func TestRenderMemoryIndexShared(t *testing.T) {
	items := []*MemItem{{Scope: "repo", Slug: "own", Kind: "project", Title: "mine"}}
	shared := []SharedSet{{Space: "Team", Dir: "team", Items: []*MemItem{
		{Scope: "repo", Slug: "b-conv", Kind: "project", Title: "convention b"},
		{Scope: "repo", Slug: "a-conv", Kind: "project", Title: "convention a"},
	}}}
	got := renderMemoryIndex(items, map[string]string{"repo:own": "own.md"}, shared)
	for _, want := range []string{"- [own](own.md) — mine", "## Shared by Team", "- [a-conv](shared/team/a-conv.md) — convention a"} {
		if !strings.Contains(got, want) {
			t.Errorf("index lacks %q:\n%s", want, got)
		}
	}
	if strings.Index(got, "a-conv") > strings.Index(got, "b-conv") {
		t.Errorf("shared items not sorted:\n%s", got)
	}
	if got := renderSharedMemFile(shared[0].Items[0], "Team"); !strings.Contains(got, "  shared: Team\n") {
		t.Errorf("shared file lacks the space:\n%s", got)
	}
}

func TestSharedDirName(t *testing.T) {
	a := sharedDirName(&Space{Id: "bafyreiabc.1kcnn6zfwn7lg", Name: "Team A"})
	b := sharedDirName(&Space{Id: "bafyreixyz.2zzzzzzzzzzzz", Name: "team-a"})
	if a == b {
		t.Errorf("two spaces share a dir: %q", a)
	}
	for _, d := range []string{a, b, sharedDirName(&Space{Id: "x", Name: ".."}), sharedDirName(&Space{Id: "y", Name: "../../etc"})} {
		if strings.ContainsAny(d, "./\\") || d == "" {
			t.Errorf("unsafe dir name %q", d)
		}
	}
	if !strings.HasPrefix(a, "team-a-") {
		t.Errorf("dir %q should start with the folded name", a)
	}
}

func TestSafeFile(t *testing.T) {
	for in, want := range map[string]string{
		"build-trap":         "build-trap.md",
		"../../../../CLAUDE": "claude.md",
		"..":                 "untitled.md",
		"a/b":                "a-b.md",
	} {
		if got := safeFile(in); got != want || strings.ContainsAny(got, "/\\") {
			t.Errorf("safeFile(%q) = %q, want %q", in, got, want)
		}
	}
}
