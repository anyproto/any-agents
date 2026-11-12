package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

// Recall-quality eval: replays a golden set of paraphrased queries
// against the live index and reports (a) retrieval quality — hit@1 /
// hit@3 / MRR over the top-10 — and (b) auto-RAG injection behavior at
// the configured threshold: would the hook have injected the right
// memory (positives) or anything at all (negatives)? Negatives are
// entries with an empty expect list.

type goldenEntry struct {
	Query  string   `json:"query"`
	Expect []string `json:"expect"`
}

func cmdEval(args []string) {
	fs := flag.NewFlagSet("eval", flag.ExitOnError)
	golden := fs.String("golden", filepath.Join(stateDir(), "eval", "golden.json"), "golden set file")
	minScore := fs.Float64("min", -1, "RAG injection threshold to simulate (default: the hook's own for the server's score scale)")
	verbose := fs.Bool("v", false, "print every query, not just misses")
	_ = fs.Parse(args)
	b, err := os.ReadFile(*golden)
	if err != nil {
		fatal(err)
	}
	var entries []goldenEntry
	if err := json.Unmarshal(b, &entries); err != nil {
		fatal(err)
	}
	c := NewClient()
	sp := mustSetup(c).private()
	cwd, _ := os.Getwd()
	scope := resolveScope(cwd)

	var pos, hit1, hit3, injected int
	var mrr float64
	var negs, negFP, negFPHits int
	var floor float64
	for _, e := range entries {
		res, err := c.Search(sp.Id, e.Query, 10)
		if err != nil {
			fatal(err)
		}
		// The hook's own gate, so the simulation stays faithful.
		floor = *minScore
		if floor < 0 {
			floor = ragMinScore(res.ScoreScale)
		}
		wouldInject := recallPick(c, sp, scope, res.Hits, floor, nil, 3)
		if len(e.Expect) == 0 {
			negs++
			if len(wouldInject) > 0 {
				negFP++
				negFPHits += len(wouldInject)
				fmt.Printf("FP    %-55q injects %d (top %s %.4f)\n",
					trunc(e.Query, 52), len(wouldInject), wouldInject[0].RecordId, wouldInject[0].Score)
			} else if *verbose {
				fmt.Printf("ok-   %-55q injects nothing\n", trunc(e.Query, 52))
			}
			continue
		}
		pos++
		rank := 0
		for i, h := range res.Hits {
			if h.Dataset == sp.coll(dsMemories) && contains(e.Expect, h.RecordId) {
				rank = i + 1
				break
			}
		}
		inj := false
		for _, h := range wouldInject {
			if contains(e.Expect, h.RecordId) {
				inj = true
				break
			}
		}
		switch {
		case rank == 1:
			hit1++
			hit3++
		case rank > 1 && rank <= 3:
			hit3++
		}
		if rank > 0 {
			mrr += 1.0 / float64(rank)
		}
		if inj {
			injected++
		}
		if rank == 0 || rank > 3 || !inj || *verbose {
			top := "-"
			score := 0.0
			if len(res.Hits) > 0 {
				top = res.Hits[0].RecordId
				score = res.Hits[0].Score
			}
			fmt.Printf("r=%-3s inj=%-5v %-55q top=%s %.4f\n",
				rankStr(rank), inj, trunc(e.Query, 52), top, score)
		}
	}
	fmt.Printf("\npositives: %d  hit@1 %.0f%%  hit@3 %.0f%%  MRR %.3f  auto-injected %.0f%%\n",
		pos, pct(hit1, pos), pct(hit3, pos), mrr/float64(max(pos, 1)), pct(injected, pos))
	fmt.Printf("negatives: %d  false-positive prompts %d (%d hits injected)\n", negs, negFP, negFPHits)
	fmt.Printf("injection floor: %.3f\n", floor)
}

func rankStr(r int) string {
	if r == 0 {
		return "MISS"
	}
	return fmt.Sprint(r)
}

func pct(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}
