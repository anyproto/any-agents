package main

import "testing"

// Only a hybrid reply injects, at the floor of its score scale.
func TestRagFloor(t *testing.T) {
	t.Setenv("ANY_AGENTS_RAG_MIN", "")
	for _, c := range []struct {
		name  string
		res   SearchResult
		floor float64
		ok    bool
	}{
		{"hybrid on the unit scale", SearchResult{Mode: "hybrid", ScoreScale: "unit"}, 0.90, true},
		{"hybrid with the raw sum", SearchResult{Mode: "hybrid"}, 0.030, true},
		{"fell back to full text", SearchResult{Mode: "fts"}, 0, false},
		{"vector only", SearchResult{Mode: "vector"}, 0, false},
		{"no mode", SearchResult{}, 0, false},
	} {
		if floor, ok := ragFloor(&c.res); floor != c.floor || ok != c.ok {
			t.Errorf("%s: floor %v, ok %v; want %v, %v", c.name, floor, ok, c.floor, c.ok)
		}
	}

	// The override sets the floor on the unit scale only.
	t.Setenv("ANY_AGENTS_RAG_MIN", "0.5")
	if floor, ok := ragFloor(&SearchResult{Mode: "hybrid", ScoreScale: "unit"}); floor != 0.5 || !ok {
		t.Errorf("override on the unit scale: %v, %v", floor, ok)
	}
	if floor, ok := ragFloor(&SearchResult{Mode: "hybrid"}); floor != 0.030 || !ok {
		t.Errorf("override on the raw scale: %v, %v", floor, ok)
	}
}
