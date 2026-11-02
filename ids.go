package main

import (
	"strings"

	"github.com/anyproto/lexid"
)

// lexidGen matches any's editor/wiki allocator (alphabet, block, step)
// so positions this tool writes interleave with the UI's.
var lexidGen = lexid.Must(lexid.CharsAllNoEscape, 4, 100)

// nextPos returns a position sorting after prev ("" = first entry).
func nextPos(prev string) string {
	if prev == "" {
		return lexidGen.Middle()
	}
	return lexidGen.Next(prev)
}

// objectLink renders the bare in-space link form relation values use.
// objectLink renders an id as a link value; an id that already is a
// link (an object, a member `any://u/…`) passes through.
func objectLink(id string) string {
	if strings.HasPrefix(id, "any://") {
		return id
	}
	return "any://" + id
}

// globalLink renders the shareable form (works in bodies and across spaces).
func globalLink(space, id string) string { return "any://o/" + space + "/" + id }

// parseObjectId accepts an object id, a bare any://<id> or a global
// any://o/<space>/<id>[/…] link and returns the object id.
func parseObjectId(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "any://")
	if strings.HasPrefix(s, "o/") {
		parts := strings.Split(s, "/")
		if len(parts) >= 3 {
			return parts[2]
		}
	}
	if i := strings.IndexAny(s, "/#"); i >= 0 {
		s = s[:i]
	}
	return s
}

// linksOf reads a relation property value (array of any://<id>) as bare
// object ids; a member link (any://u/<identity>) is kept whole.
func linksOf(row map[string]any, typeId, propId string) []string {
	arr, _ := group(row, typeId)[propId].([]any)
	var out []string
	for _, v := range arr {
		if s, ok := v.(string); ok {
			if strings.HasPrefix(s, "any://u/") {
				out = append(out, s)
				continue
			}
			out = append(out, parseObjectId(s))
		}
	}
	return out
}

// choiceOf reads the first option key of a choice value.
func choiceOf(row map[string]any, typeId, propId string) string {
	arr, _ := group(row, typeId)[propId].([]any)
	if len(arr) > 0 {
		s, _ := arr[0].(string)
		return s
	}
	return ""
}

func strOf(row map[string]any, typeId, propId string) string {
	s, _ := group(row, typeId)[propId].(string)
	return s
}

func nameOf(row map[string]any) string {
	n, _ := group(row, "any")["name"].(string)
	return n
}

// typeOf reads an object's one type.
func typeOf(row map[string]any) string {
	t, _ := group(row, "any")["type"].(string)
	return t
}

// inCollection reports whether the object is filed under a collection.
func inCollection(row map[string]any, collectionId string) bool {
	arr, _ := group(row, "any")["collections"].([]any)
	for _, c := range arr {
		if c == collectionId {
			return true
		}
	}
	return false
}

func shortId(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// linkList renders ids as relation values.
func linkList(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != "" {
			out = append(out, objectLink(id))
		}
	}
	return out
}

// addToLinks returns the union of an existing relation value and id.
func addToLinks(cur []string, id string) []string {
	for _, c := range cur {
		if c == id {
			return linkList(cur)
		}
	}
	return linkList(append(cur, id))
}
