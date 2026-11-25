package main

import (
	"strings"
	"testing"
)

func TestSchema(t *testing.T) {
	if _, err := loadSchema(schemaSrc); err != nil {
		t.Fatal(err)
	}
	drafts := bundleDrafts()
	if len(drafts) != len(bundleNames) {
		t.Fatalf("%d drafts for %d bundle names", len(drafts), len(bundleNames))
	}
	for _, name := range bundleNames {
		d := drafts[name]
		prev := ""
		for _, p := range d.Properties {
			pos := p.XFormat["pos"].(string)
			if pos <= prev {
				t.Errorf("%s.%s: pos %q not after %q", name, p.XKey, pos, prev)
			}
			prev = pos
		}
		prev = ""
		for _, part := range d.Parts {
			pos := part["pos"].(string)
			if pos <= prev {
				t.Errorf("%s part %v: pos %q not after %q", name, part["key"], pos, prev)
			}
			prev = pos
		}
	}
	records := map[string]bool{}
	for _, r := range schema.bundles["app"].Records {
		records[r.Key] = true
	}
	for _, ds := range []string{dsMemories, dsConfig, dsStats, dsFeedback} {
		if !records[ds] {
			t.Errorf("app declares no %q records", ds)
		}
	}
}

func TestLoadSchemaRejects(t *testing.T) {
	const app = "---\nkey: \"app\"\nid: \"a/v1\"\nname: \"A\"\nxKey: \"a\"\nicon: {glyph: \"bot\", color: \"grey\"}\n"
	cases := map[string]struct{ src, want string }{
		"unknown key":   {app + "hidenEmpty: true\n---\n", "unknown field"},
		"unknown type":  {app + "properties:\n    - x: {name: \"X\", type: \"txt\"}\n---\n", `unknown type "txt"`},
		"no choices":    {app + "properties:\n    - x: {name: \"X\", type: \"choice\"}\n---\n", "choices"},
		"stray choices": {app + "properties:\n    - x: {name: \"X\", type: \"text\"}\nchoices.x:\n    - a: {name: \"A\", color: \"red\"}\n---\n", "choices"},
		"bad target":    {app + "properties:\n    - x: {name: \"X\", type: \"relation\", to: [\"nope\"]}\n---\n", `to "nope"`},
		"bad index":     {app + "properties:\n    - x: {name: \"X\", type: \"text\", index: \"full\"}\n---\n", `unknown index "full"`},
		"duplicate":     {app + "properties:\n    - x: {name: \"X\", type: \"text\"}\n    - x: {name: \"Y\", type: \"text\"}\n---\n", "declared twice"},
		"unknown part":  {app + "parts: [\"bodyy\"]\n---\n", `unknown part "bodyy"`},
		"no app":        {strings.Replace(app, `key: "app"`, `key: "other"`, 1) + "---\n", `no "app" bundle`},
	}
	for name, c := range cases {
		if _, err := loadSchema([]byte(c.src)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, c.want)
		}
	}
}

func BenchmarkLoadSchema(b *testing.B) {
	for b.Loop() {
		if _, err := loadSchema(schemaSrc); err != nil {
			b.Fatal(err)
		}
	}
}
