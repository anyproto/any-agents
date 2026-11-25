package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestSchema(t *testing.T) {
	drafts := bundleDrafts()
	if len(drafts) != len(bundleNames) {
		t.Fatalf("%d drafts for %d bundle names", len(drafts), len(bundleNames))
	}
	for _, name := range bundleNames {
		prev := ""
		for _, p := range drafts[name].Properties {
			if pos := p.XFormat["pos"].(string); pos <= prev {
				t.Errorf("%s.%s: pos %q not after %q", name, p.XKey, pos, prev)
			} else {
				prev = pos
			}
		}
	}
	var parts []string
	for _, p := range drafts["app"].Parts {
		parts = append(parts, p["key"].(string))
	}
	for _, ds := range []string{dsMemories, dsConfig, dsStats, dsFeedback} {
		if !slices.Contains(parts, ds) {
			t.Errorf("app declares no %q records", ds)
		}
	}
	// Drafts are the caller's to change.
	drafts["app"].RootProperties["miniapp"].(map[string]any)["bundle"] = "changed"
	drafts["session"].Properties[2].XFormat["relation"].(map[string]any)["targetTypes"].([]string)[0] = "changed"
	if b, _ := json.Marshal(bundleDrafts()); strings.Contains(string(b), "changed") {
		t.Error("a change to one draft reached the next bundleDrafts call")
	}
}

func TestLoadSchemaRejects(t *testing.T) {
	bundle := func(key string) string {
		return "---\n" + key + ":\n    id: \"" + key + "/v1\"\n    name: \"N\"\n    xKey: \"" + key + "\"\n    icon: {glyph: \"bot\", color: \"grey\"}\n"
	}
	app := bundle("app")
	prop := func(p string) string { return app + "    properties:\n        - x: " + p + "\n" }
	choices := func(key string, opts ...string) string {
		return "    choices." + key + ":\n        - " + strings.Join(opts, "\n        - ") + "\n"
	}
	record := func(lines ...string) string {
		return app + "    records:\n        ---\n        r:\n            name: \"R\"\n            " + strings.Join(lines, "\n            ") + "\n        ---\n"
	}
	cases := map[string]struct{ src, want string }{
		"unknown key":       {app + "    hidenEmpty: true\n", `app: json: unknown field "hidenEmpty"`},
		"layout typo":       {app + "    layout.typ: \"page\"\n", `unknown field "typ"`},
		"no xKey":           {strings.Replace(app, "    xKey: \"app\"\n", "", 1), "xKey are required"},
		"no icon color":     {strings.Replace(app, ", color: \"grey\"", "", 1), "icon needs a glyph and a color"},
		"no app":            {bundle("other"), `no "app" bundle`},
		"duplicate bundle":  {app + app, `"app" listed twice`},
		"duplicate id":      {app + strings.Replace(bundle("b"), "b/v1", "app/v1", 1), `bundle id "app/v1" declared twice`},
		"duplicate xKey":    {app + strings.Replace(bundle("b"), `xKey: "b"`, `xKey: "app"`, 1), `bundle xKey "app" declared twice`},
		"collection parts":  {app + bundle("b") + "    collection: true\n    parts: [\"body\"]\n", "b: a collection has no parts"},
		"unknown part":      {app + "    parts: [\"bodyy\"]\n", `unknown part "bodyy"`},
		"not a single pair": {app + "    properties:\n        - {x: {name: \"X\", type: \"text\"}, y: {name: \"Y\", type: \"text\"}}\n", "one `key: {...}` pair"},
		"unknown type":      {prop(`{name: "X", type: "txt"}`), `app: x: unknown type "txt"`},
		"no choices":        {prop(`{name: "X", type: "choice"}`), "needs choices.<xKey>"},
		"stray choices":     {prop(`{name: "X", type: "text"}`) + choices("x", `a: {name: "A", color: "red"}`), "choices are for a choice property"},
		"orphan choices":    {prop(`{name: "X", type: "text"}`) + choices("y", `a: {name: "A", color: "red"}`), "choices.y: no such property"},
		"duplicate option":  {prop(`{name: "X", type: "choice"}`) + choices("x", `a: {name: "A", color: "red"}`, `a: {name: "B", color: "blue"}`), `"a" listed twice`},
		"blank option":      {prop(`{name: "X", type: "choice"}`) + choices("x", `a: {name: "A"}`), "needs a name and a color"},
		"bad target":        {prop(`{name: "X", type: "relation", to: ["nope"]}`), `to "nope": no bundle has that xKey`},
		"bad index":         {prop(`{name: "X", type: "text", index: "full"}`), `unknown index "full"`},
		"duplicate prop":    {prop(`{name: "X", type: "text"}`) + "        - x: {name: \"Y\", type: \"text\"}\n", `"x" listed twice`},
		"field kind":        {record("fields:", "    - t: {kind: \"strng\"}"), `r: t: unknown kind "strng"`},
		"no field kind":     {record("fields:", "    - t: {}"), `t: unknown kind ""`},
		"stamp field":       {record("fields:", "    - author: {kind: \"string\"}"), "author: every dataset gets that stamp"},
		"duplicate field":   {record("fields:", "    - t: {kind: \"string\"}", "    - t: {kind: \"number\"}"), `"t" listed twice`},
		"search typo":       {record("search: {titel: \"t\"}", "fields:", "    - t: {kind: \"string\"}"), `unknown field "titel"`},
		"search no field":   {record("search: {title: \"u\"}", "fields:", "    - t: {kind: \"string\"}"), `search names "u"`},
		"record is a part":  {app + "    parts: [\"body\"]\n    records:\n        ---\n        body:\n            name: \"R\"\n        ---\n", `part "body" declared twice`},
	}
	for name, c := range cases {
		if _, err := loadSchema([]byte(c.src + "---\n")); err == nil || !strings.Contains(err.Error(), c.want) {
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
