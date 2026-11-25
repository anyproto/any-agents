package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/canonical/go-cnf/cnfembed"
)

// The space schema is declared in schema.cnf; this file checks it and
// expands it into the bundle ensure requests.

const (
	version      = "0.4.10"
	setupVersion = 8 // bump to force a full re-ensure on every machine

	// pageType is any's built-in document type every collection member has.
	pageType = "page"

	// Records datasets on the app root (part key = dataset key).
	dsMemories = "memories"
	dsConfig   = "config"
	dsStats    = "stats"
	dsFeedback = "feedback"
)

//go:embed schema.cnf
var schemaSrc []byte

var (
	// schema is schema.cnf's bundles, in install order. A bad edit
	// panics at start, which fails every test in this package.
	schema = mustLoadSchema(schemaSrc)

	// bundleNames are the state keys, in install order.
	bundleNames = schema.keys()
	bundleIds   = schemaIndex(func(b bundleSpec) string { return b.Id })
	bundleApp   = bundleIds["app"]
	// collectionBundles declare a collection: their objects are pages
	// filed under it.
	collectionBundles = schemaIndex(func(b bundleSpec) bool { return b.Collection })
	// optionalBundles may be absent from a space.
	optionalBundles = schemaIndex(func(b bundleSpec) bool { return b.Optional })
	typeIcons       = schemaIndex(func(b bundleSpec) iconSpec { return b.Icon })
)

var defaultSpaceName = "Agents"

func spaceName() string {
	if v := getenv("ANY_AGENTS_SPACE"); v != "" {
		return v
	}
	return defaultSpaceName
}

// --- schema.cnf, decoded ---
//
// schema.cnf's header documents each key. Keys match field names
// case-insensitively; unknown keys are errors.

type bundleSpec struct {
	Id, Name, XKey               string
	Hidden, Collection, Optional bool
	Icon                         iconSpec
	Miniapp                      *struct{ Pos string }
	Layout                       *struct{ Type string }
	Parts                        []string
	Records                      list[recordSpec]
	Properties                   list[propSpec]
	Choices                      map[string]list[optionSpec]
}

type iconSpec struct{ Glyph, Color string }

type propSpec struct {
	Name, Type, Index   string
	To                  []string
	Multiple, HideEmpty bool
}

type optionSpec struct{ Name, Color string }

type recordSpec struct {
	Name, DisplayName string
	SkipHistory       bool
	Search            struct {
		Title string `json:"title,omitempty"`
		Text  string `json:"text,omitempty"`
	}
	Fields list[struct {
		Kind     string
		Required bool
	}]
}

// propKinds maps a property type (xFormat.type) to its storage kind.
var propKinds = map[string]string{
	"text": "string", "longtext": "string", "url": "string",
	"datetime": "datetime", "date": "datetime", "checkbox": "boolean", "number": "number",
	"choice": "array", "relation": "array", "member": "array",
}

var fieldKinds = []string{"string", "number", "boolean", "array", "object", "datetime"}

// moduleParts are the parts a type can declare, each a module's
// canonical dataset.
var moduleParts = map[string]struct{ name, ui, module string }{
	"body": {"Body", "document", "editor"}, // the editor body a page has, so retyping keeps it
	"chat": {"Chat", "chat", "chat"},       // one transcript per object
}

// stamps are the fields every records dataset gets: key, stamp.
var stamps = [][2]string{{"author", "creator"}, {"createdAt", "createTime"}, {"updatedAt", "modifyTime"}}

// list is a cnf list of `key: {...}` items, in order, keys unique.
type list[T any] []entry[T]

type entry[T any] struct {
	key string
	val T
}

func (l *list[T]) UnmarshalJSON(data []byte) error {
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(data, &items); err != nil {
		return err
	}
	for _, item := range items {
		if len(item) != 1 {
			return errors.New("each list item is one `key: {...}` pair")
		}
		for key, raw := range item {
			e := entry[T]{key: key}
			if err := decodeStrict(raw, &e.val); err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			*l = append(*l, e)
		}
	}
	if d := repeated(l.keys()); d != "" {
		return fmt.Errorf("%q listed twice", d)
	}
	return nil
}

func (l list[T]) keys() (keys []string) {
	for _, e := range l {
		keys = append(keys, e.key)
	}
	return keys
}

// --- loading and checks ---

func mustLoadSchema(src []byte) list[bundleSpec] {
	bundles, err := loadSchema(src)
	if err != nil {
		panic("schema.cnf: " + err.Error())
	}
	return bundles
}

func loadSchema(src []byte) (list[bundleSpec], error) {
	js, err := cnfembed.ToJSON(src, nil)
	if err != nil {
		return nil, err
	}
	var bundles list[bundleSpec]
	if err := json.Unmarshal(js, &bundles); err != nil {
		return nil, err
	}
	var ids, xkeys []string
	for _, b := range bundles {
		ids, xkeys = append(ids, b.val.Id), append(xkeys, b.val.XKey)
	}
	var c checker
	for _, b := range bundles {
		c.in(b.key, b.val.check(xkeys))
	}
	c.need(slices.Contains(bundles.keys(), "app"), `no "app" bundle`)
	c.none(repeated(ids), "bundle id %q declared twice")
	c.none(repeated(xkeys), "bundle xKey %q declared twice")
	return bundles, c.err
}

func (b bundleSpec) check(xkeys []string) error {
	var c checker
	for _, p := range b.Properties {
		c.in(p.key, p.val.check(b.Choices[p.key], xkeys))
	}
	for _, r := range b.Records {
		c.in(r.key, r.val.check())
	}
	parts := append(slices.Clone(b.Parts), b.Records.keys()...)
	c.need(b.Id != "" && b.Name != "" && b.XKey != "", "id, name and xKey are required")
	c.need(b.Icon.Glyph != "" && b.Icon.Color != "", "icon needs a glyph and a color")
	c.need(!b.Collection || len(parts) == 0 && b.Layout == nil, "a collection has no parts, records or layout")
	c.none(missing(b.Parts, slices.Collect(maps.Keys(moduleParts))), "unknown part %q")
	c.none(repeated(parts), "part %q declared twice")
	c.none(missing(slices.Collect(maps.Keys(b.Choices)), b.Properties.keys()), "choices.%s: no such property")
	return c.err
}

func (p propSpec) check(options list[optionSpec], xkeys []string) error {
	var c checker
	c.need(p.Name != "", "name is required")
	c.need(propKinds[p.Type] != "", "unknown type %q", p.Type)
	c.need(p.Type != "choice" || len(options) > 0, "a choice property needs choices.<xKey>")
	c.need(p.Type == "choice" || len(options) == 0, "choices are for a choice property")
	for _, o := range options {
		c.need(o.val.Name != "" && o.val.Color != "", "choice %s needs a name and a color", o.key)
	}
	c.need(p.Type == "relation" || len(p.To) == 0, "to is for relations")
	c.none(missing(p.To, xkeys), "to %q: no bundle has that xKey")
	c.need(p.Type == "relation" || p.Type == "member" || !p.Multiple, "multiple is for relations and members")
	c.need(p.Index == "" || p.Index == "none" || p.Index == "basic", "unknown index %q", p.Index)
	return c.err
}

func (r recordSpec) check() error {
	var c checker
	c.need(r.Name != "", "name is required")
	for _, f := range r.Fields {
		c.need(slices.Contains(fieldKinds, f.val.Kind), "%s: unknown kind %q", f.key, f.val.Kind)
		c.need(!slices.ContainsFunc(stamps, func(s [2]string) bool { return s[0] == f.key }), "%s: every dataset gets that stamp", f.key)
	}
	c.none(missing([]string{r.Search.Title, r.Search.Text}, r.Fields.keys()), "search names %q, which is no field")
	return c.err
}

// --- expansion ---

// bundleDrafts expands schema.cnf into the ensure requests, built fresh
// per call.
func bundleDrafts() map[string]*BundleEnsure {
	out := map[string]*BundleEnsure{}
	for _, b := range schema {
		out[b.key] = b.val.expand()
	}
	return out
}

func (b bundleSpec) expand() *BundleEnsure {
	e := &BundleEnsure{Id: b.Id, Name: b.Name, XKey: b.XKey, Hidden: b.Hidden, Collection: b.Collection}
	if b.Layout != nil {
		e.Layout = map[string]any{"type": b.Layout.Type}
	}
	if b.Miniapp != nil {
		e.RootCollections = []string{"miniapp"}
		e.RootProperties = map[string]any{"miniapp": map[string]any{"bundle": b.Id, "pos": b.Miniapp.Pos}}
	}
	for i, p := range b.Properties {
		e.Properties = append(e.Properties, p.val.expand(p.key, lexPos(i), b.Choices[p.key]))
	}
	for i, key := range b.Parts {
		m := moduleParts[key]
		e.Parts = append(e.Parts, map[string]any{"key": key, "name": m.name, "pos": lexPos(i),
			"ui": map[string]any{"type": m.ui}, "datasets": []any{map[string]any{"module": m.module}}})
	}
	for i, r := range b.Records {
		e.Parts = append(e.Parts, r.val.expand(r.key, lexPos(len(b.Parts)+i)))
	}
	return e
}

func (p propSpec) expand(xkey, pos string, choices list[optionSpec]) PropertyDraft {
	xf := map[string]any{"type": p.Type, "pos": pos}
	d := PropertyDraft{XKey: xkey, Name: p.Name, Kind: propKinds[p.Type], XFormat: xf}
	switch p.Type {
	case "choice":
		options := map[string]any{}
		for i, o := range choices {
			options[o.key] = map[string]any{"name": o.val.Name, "color": o.val.Color, "pos": lexPos(i)}
		}
		xf["config"], xf["options"] = map[string]any{"multiple": false}, options
	case "relation", "member":
		xf["config"] = map[string]any{"multiple": p.Multiple}
		if len(p.To) > 0 {
			xf["relation"] = map[string]any{"targetTypes": slices.Clone(p.To)}
		}
		d.Meta = map[string]any{"index": "none"} // ids and identities; the link index has the edges
	}
	if p.Index != "" {
		d.Meta = map[string]any{"index": p.Index}
	}
	if p.HideEmpty {
		xf["anyui"] = map[string]any{"headerVisibility": "hideWhenEmpty"}
	}
	return d
}

func (r recordSpec) expand(key, pos string) map[string]any {
	var fields []any
	for _, f := range r.Fields {
		fd := map[string]any{"key": f.key, "kind": f.val.Kind, "mutableBy": "author"}
		if f.val.Required {
			fd["required"] = true
		}
		fields = append(fields, fd)
	}
	for _, s := range stamps {
		fd := map[string]any{"key": s[0], "stamp": s[1]}
		if s[1] != "creator" {
			fd["xFormat"] = map[string]any{"type": "datetime"}
		}
		fields = append(fields, fd)
	}
	ds := map[string]any{"key": key, "idRule": "user", "deleteBy": "author", "displayName": r.DisplayName, "fields": fields}
	if r.Search.Title != "" || r.Search.Text != "" {
		ds["search"] = r.Search
	}
	if r.SkipHistory {
		ds["skipHistory"] = true
	}
	return map[string]any{"key": key, "name": r.Name, "pos": pos, "ui": map[string]any{"type": "table"}, "datasets": []any{ds}}
}

// --- helpers ---

// checker keeps the first failed check.
type checker struct{ err error }

func (c *checker) need(ok bool, format string, args ...any) {
	if !ok && c.err == nil {
		c.err = fmt.Errorf(format, args...)
	}
}

// none fails when found is non-empty, naming it.
func (c *checker) none(found, format string) { c.need(found == "", format, found) }

// in keeps a nested check's error under key.
func (c *checker) in(key string, err error) { c.need(err == nil, "%s: %w", key, err) }

// repeated returns the first value that occurs twice, or "".
func repeated(vals []string) string {
	for i, v := range vals {
		if slices.Contains(vals[:i], v) {
			return v
		}
	}
	return ""
}

// missing returns the first non-empty key absent from have, or "".
func missing(keys, have []string) string {
	for _, k := range keys {
		if k != "" && !slices.Contains(have, k) {
			return k
		}
	}
	return ""
}

// decodeStrict decodes JSON, rejecting keys v does not declare.
func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func schemaIndex[V any](f func(bundleSpec) V) map[string]V {
	m := map[string]V{}
	for _, b := range schema {
		m[b.key] = f(b.val)
	}
	return m
}

// lexPos yields short byte-ordered position keys: a0 … a9, b0 … .
func lexPos(i int) string {
	return string(rune('a'+i/10)) + string(rune('0'+i%10))
}

// iconOf renders the any-ui icon:v2 value for a lucide glyph.
func iconOf(icon iconSpec) string {
	return `icon:v2:{"assignment":{"kind":"pack","ref":{"packId":"io.anyproto.lucide","glyphId":"` + icon.Glyph +
		`"}},"color":"` + icon.Color + `"}`
}
