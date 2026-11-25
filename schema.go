package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/canonical/go-cnf/cnfjson"
)

// The space schema lives in schema.cnf; this file loads, checks and
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

// schema is schema.cnf, parsed and checked. A bad edit panics at start;
// TestSchema loads the file, so `go test` reports it first.
var schema = mustLoadSchema(schemaSrc)

var (
	// bundleNames are the state keys, in install order.
	bundleNames = schema.names
	bundleIds   = schema.ids
	bundleApp   = bundleIds["app"]
	// collectionBundles declare a collection instead of a type: their
	// objects are pages filed under them.
	collectionBundles = schema.collections
	// optionalBundles may be absent in a space: collab needs a server that
	// lets a client type declare the chat module.
	optionalBundles = schema.optional
	typeIcons       = schema.icons
)

var defaultSpaceName = "Agents"

func spaceName() string {
	if v := getenv("ANY_AGENTS_SPACE"); v != "" {
		return v
	}
	return defaultSpaceName
}

// --- schema.cnf shape ---

type schemaFile struct {
	names       []string
	ids         map[string]string
	collections map[string]bool
	optional    map[string]bool
	icons       map[string][2]string
	bundles     map[string]*bundleSpec
}

type bundleSpec struct {
	Key             string                             `json:"key"`
	Id              string                             `json:"id"`
	Name            string                             `json:"name"`
	XKey            string                             `json:"xKey"`
	Hidden          bool                               `json:"hidden"`
	Collection      bool                               `json:"collection"`
	Optional        bool                               `json:"optional"`
	Layout          map[string]any                     `json:"layout"`
	Icon            iconSpec                           `json:"icon"`
	RootCollections []string                           `json:"rootCollections"`
	RootProperties  map[string]any                     `json:"rootProperties"`
	Parts           []string                           `json:"parts"`
	Records         []recordSpec                       `json:"records"`
	Properties      []map[string]propSpec              `json:"properties"`
	Choices         map[string][]map[string]optionSpec `json:"choices"`
}

type iconSpec struct {
	Glyph string `json:"glyph"`
	Color string `json:"color"`
}

type propSpec struct {
	Name      string   `json:"name"`
	Type      string   `json:"type"`
	To        []string `json:"to"`
	Multiple  bool     `json:"multiple"`
	Index     string   `json:"index"`
	HideEmpty bool     `json:"hideEmpty"`
}

type optionSpec struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

type recordSpec struct {
	Key         string                 `json:"key"`
	Name        string                 `json:"name"`
	DisplayName string                 `json:"displayName"`
	SkipHistory bool                   `json:"skipHistory"`
	Search      map[string]any         `json:"search"`
	Fields      []map[string]fieldSpec `json:"fields"`
}

type fieldSpec struct {
	Kind     string `json:"kind"`
	Required bool   `json:"required"`
}

// propKinds maps a property type (xFormat.type) to its storage kind.
var propKinds = map[string]string{
	"text": "string", "longtext": "string", "url": "string",
	"datetime": "datetime", "date": "datetime", "checkbox": "boolean", "number": "number",
	"choice": "array", "relation": "array", "member": "array",
}

var partTemplates = map[string]func(pos string) map[string]any{"body": bodyPart, "chat": chatPart}

// --- loading ---

func mustLoadSchema(src []byte) *schemaFile {
	s, err := loadSchema(src)
	if err != nil {
		panic("schema.cnf: " + err.Error())
	}
	return s
}

func loadSchema(src []byte) (*schemaFile, error) {
	js, err := cnfjson.ToJSON(src, nil)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(js))
	dec.DisallowUnknownFields() // a misspelt key is an error, not a silent default
	var specs []*bundleSpec
	if err := dec.Decode(&specs); err != nil {
		return nil, err
	}
	targets := map[string]bool{}
	for _, b := range specs {
		targets[b.XKey] = true
	}
	s := &schemaFile{ids: map[string]string{}, collections: map[string]bool{}, optional: map[string]bool{},
		icons: map[string][2]string{}, bundles: map[string]*bundleSpec{}}
	for _, b := range specs {
		if err := b.check(targets); err != nil {
			return nil, fmt.Errorf("bundle %q: %w", b.Key, err)
		}
		if s.bundles[b.Key] != nil {
			return nil, fmt.Errorf("bundle %q declared twice", b.Key)
		}
		s.names = append(s.names, b.Key)
		s.ids[b.Key] = b.Id
		if b.Collection {
			s.collections[b.Key] = true
		}
		if b.Optional {
			s.optional[b.Key] = true
		}
		s.icons[b.Key] = [2]string{b.Icon.Glyph, b.Icon.Color}
		s.bundles[b.Key] = b
	}
	if s.bundles["app"] == nil {
		return nil, errors.New(`no "app" bundle`)
	}
	return s, nil
}

func (b *bundleSpec) check(targets map[string]bool) error {
	if b.Key == "" || b.Id == "" || b.Name == "" || b.Icon.Glyph == "" {
		return errors.New("key, id, name and icon are required")
	}
	props := map[string]bool{}
	for _, item := range b.Properties {
		xkey, p, ok := only(item)
		if !ok {
			return errors.New("each properties item is one `xKey: {...}` pair")
		}
		if props[xkey] {
			return fmt.Errorf("property %q declared twice", xkey)
		}
		props[xkey] = true
		if err := p.check(len(b.Choices[xkey]) > 0, targets); err != nil {
			return fmt.Errorf("property %q: %w", xkey, err)
		}
	}
	for xkey, opts := range b.Choices {
		if !props[xkey] {
			return fmt.Errorf("choices.%s: no such property", xkey)
		}
		for _, item := range opts {
			if _, o, ok := only(item); !ok || o.Name == "" || o.Color == "" {
				return fmt.Errorf("choices.%s: each item is one `key: {name, color}` pair", xkey)
			}
		}
	}
	for _, p := range b.Parts {
		if partTemplates[p] == nil {
			return fmt.Errorf("unknown part %q", p)
		}
	}
	for _, r := range b.Records {
		if r.Key == "" || r.Name == "" {
			return errors.New("records need a key and a name")
		}
		for _, item := range r.Fields {
			if _, f, ok := only(item); !ok || f.Kind == "" {
				return fmt.Errorf("records %q: each field is one `key: {kind}` pair", r.Key)
			}
		}
	}
	return nil
}

func (p propSpec) check(hasChoices bool, targets map[string]bool) error {
	if p.Name == "" {
		return errors.New("name is required")
	}
	if propKinds[p.Type] == "" {
		return fmt.Errorf("unknown type %q", p.Type)
	}
	if p.Type == "choice" && !hasChoices {
		return errors.New("a choice property needs choices.<xKey>")
	}
	if p.Type != "choice" && hasChoices {
		return errors.New("choices are for a choice property")
	}
	if len(p.To) > 0 && p.Type != "relation" {
		return errors.New("to is for relations")
	}
	if p.Multiple && p.Type != "relation" && p.Type != "member" {
		return errors.New("multiple is for relations and members")
	}
	for _, t := range p.To {
		if !targets[t] {
			return fmt.Errorf("to %q: no bundle has that xKey", t)
		}
	}
	if p.Index != "" && p.Index != "none" && p.Index != "basic" {
		return fmt.Errorf("unknown index %q", p.Index)
	}
	return nil
}

// only returns the single entry of a `- key: value` list item.
func only[V any](m map[string]V) (key string, v V, ok bool) {
	if len(m) != 1 {
		return "", v, false
	}
	for key, v = range m {
	}
	return key, v, true
}

// --- expansion ---

// bundleDrafts expands schema.cnf into the ensure requests, built fresh
// per call.
func bundleDrafts() map[string]*BundleEnsure {
	out := make(map[string]*BundleEnsure, len(schema.bundles))
	for name, b := range schema.bundles {
		out[name] = b.expand()
	}
	return out
}

func (b *bundleSpec) expand() *BundleEnsure {
	e := &BundleEnsure{Id: b.Id, Name: b.Name, XKey: b.XKey, Layout: b.Layout, Hidden: b.Hidden,
		Collection: b.Collection, RootCollections: b.RootCollections, RootProperties: b.RootProperties}
	for i, item := range b.Properties {
		xkey, p, _ := only(item)
		e.Properties = append(e.Properties, p.expand(xkey, lexPos(i), b.Choices[xkey]))
	}
	for i, p := range b.Parts {
		e.Parts = append(e.Parts, partTemplates[p](lexPos(i)))
	}
	for i, r := range b.Records {
		e.Parts = append(e.Parts, r.expand(lexPos(len(b.Parts)+i)))
	}
	return e
}

func (p propSpec) expand(xkey, pos string, choices []map[string]optionSpec) PropertyDraft {
	xf := map[string]any{"type": p.Type, "pos": pos}
	d := PropertyDraft{XKey: xkey, Name: p.Name, Kind: propKinds[p.Type], XFormat: xf}
	switch p.Type {
	case "choice":
		options := map[string]any{}
		for i, item := range choices {
			key, o, _ := only(item)
			options[key] = map[string]any{"name": o.Name, "color": o.Color, "pos": lexPos(i)}
		}
		xf["config"], xf["options"] = map[string]any{"multiple": false}, options
	case "relation", "member":
		xf["config"] = map[string]any{"multiple": p.Multiple}
		if len(p.To) > 0 {
			xf["relation"] = map[string]any{"targetTypes": p.To}
		}
		// Object ids and identities stay out of the text index (the
		// link index still reports relation edges).
		d.Meta = map[string]any{"index": "none"}
	}
	if p.Index != "" {
		d.Meta = map[string]any{"index": p.Index}
	}
	if p.HideEmpty {
		xf["anyui"] = map[string]any{"headerVisibility": "hideWhenEmpty"}
	}
	return d
}

func (r recordSpec) expand(pos string) map[string]any {
	var fields []any
	for _, item := range r.Fields {
		key, f, _ := only(item)
		fd := map[string]any{"key": key, "kind": f.Kind, "mutableBy": "author"}
		if f.Required {
			fd["required"] = true
		}
		fields = append(fields, fd)
	}
	fields = append(fields, stamp("author", "creator"), stamp("createdAt", "createTime"), stamp("updatedAt", "modifyTime"))
	ds := map[string]any{"key": r.Key, "idRule": "user", "deleteBy": "author", "displayName": r.DisplayName, "fields": fields}
	if r.Search != nil {
		ds["search"] = r.Search
	}
	if r.SkipHistory {
		ds["skipHistory"] = true
	}
	return map[string]any{"key": r.Key, "name": r.Name, "pos": pos,
		"ui": map[string]any{"type": "table"}, "datasets": []any{ds}}
}

func stamp(key, kind string) map[string]any {
	f := map[string]any{"key": key, "stamp": kind}
	if kind != "creator" {
		f["xFormat"] = map[string]any{"type": "datetime"}
	}
	return f
}

// bodyPart is the editor module's canonical dataset: the same
// editor_blocks body a `page` has, so retyping keeps it.
func bodyPart(pos string) map[string]any {
	return map[string]any{"key": "body", "name": "Body", "pos": pos,
		"ui":       map[string]any{"type": "document"},
		"datasets": []any{map[string]any{"module": "editor"}}}
}

// chatPart is one transcript per object (the chat module admits only
// its canonical dataset).
func chatPart(pos string) map[string]any {
	return map[string]any{"key": "chat", "name": "Chat", "pos": pos,
		"ui":       map[string]any{"type": "chat"},
		"datasets": []any{map[string]any{"module": "chat"}}}
}

// lexPos yields short byte-ordered position keys: a0 … a9, b0 … .
func lexPos(i int) string {
	return string(rune('a'+i/10)) + string(rune('0'+i%10))
}

// iconOf renders the any-ui icon:v2 value for a lucide glyph.
func iconOf(glyph, color string) string {
	return `icon:v2:{"assignment":{"kind":"pack","ref":{"packId":"io.anyproto.lucide","glyphId":"` + glyph +
		`"}},"color":"` + color + `"}`
}
