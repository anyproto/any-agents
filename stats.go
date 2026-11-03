package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Telemetry: every bridge operation bumps a counter in a local
// append-only day log (O_APPEND lines — safe across concurrent hooks,
// no locking); flushStats folds the log into one stats record per
// (machine, day). The flush upsert is idempotent.

var statCounters = []string{
	"pulls", "pushes", "conflicts", "memSynced", "memRejected",
	"ragPrompts", "ragInjected", "searches", "sessions", "errors",
}

type statLine struct {
	C string `json:"c"`
	N int    `json:"n"`
	E string `json:"e,omitempty"` // error text, C == "errors" only
}

func hostName() string {
	h, _ := os.Hostname()
	return h
}

func statsLogPath(day string) string {
	return filepath.Join(stateDir(), "stats", day+".log")
}

func appendStatLine(l statLine) {
	dir := filepath.Join(stateDir(), "stats")
	_ = os.MkdirAll(dir, 0o755)
	f, err := os.OpenFile(statsLogPath(time.Now().Format("2006-01-02")),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(l)
	_, _ = f.Write(append(b, '\n'))
}

func bumpStat(name string, n int) {
	if n > 0 {
		appendStatLine(statLine{C: name, N: n})
	}
}

// recordErrorStat notes a bridge failure for the stats record and echoes
// it to stderr.
func recordErrorStat(context string, err error) {
	fmt.Fprintf(os.Stderr, "any-agents: %s: %v\n", context, err)
	appendStatLine(statLine{C: "errors", N: 1, E: context + ": " + err.Error()})
}

func readStatsLog(day string) (map[string]int, []string) {
	counters := map[string]int{}
	var errs []string
	f, err := os.Open(statsLogPath(day))
	if err != nil {
		return counters, errs
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var l statLine
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		counters[l.C] += l.N
		if l.E != "" {
			errs = append(errs, l.E)
		}
	}
	if len(errs) > 5 {
		errs = errs[len(errs)-5:]
	}
	return counters, errs
}

// flushStats upserts today's (and yesterday's, to close out the day
// boundary) rollups. Best-effort — telemetry never fails an operation.
func flushStats(c *Client, sp *Space) {
	machine := hostName()
	var records []UpsertRecord
	for _, day := range []string{
		time.Now().AddDate(0, 0, -1).Format("2006-01-02"),
		time.Now().Format("2006-01-02"),
	} {
		counters, errs := readStatsLog(day)
		if len(counters) == 0 {
			continue
		}
		fields := map[string]any{"machine": machine, "day": day}
		for _, k := range statCounters {
			fields[k] = counters[k]
		}
		if errs == nil {
			errs = []string{}
		}
		fields["lastErrors"] = errs
		records = append(records, UpsertRecord{Id: machine + ":" + day, Fields: fields})
	}
	if len(records) == 0 {
		return
	}
	if _, err := c.Upsert(sp.Id, sp.owner("app"), sp.coll(dsStats), records); err != nil {
		fmt.Fprintf(os.Stderr, "any-agents: stats flush: %v\n", err)
	}
}

// --- feedback ---

func cmdFeedback(args []string) {
	fs, pos := parseArgs("feedback", args, func(fs *flag.FlagSet) {
		fs.String("kind", "note", "bug | idea | memory-wrong | memory-helpful | note")
		fs.String("ref", "", "related record or object id")
		fs.String("s", "", "session id")
		spaceFlag(fs)
	})
	text := strings.Join(pos, " ")
	if strings.TrimSpace(text) == "" {
		fatal(fmt.Errorf("feedback needs text"))
	}
	c := NewClient()
	// A note in a collab space is read by every member's agent; the
	// private space keeps notes about memories and the bridge itself.
	st := mustSetup(c)
	sp := spaceOf(st, fs.Lookup("space").Value.String())
	fields := map[string]any{"kind": fs.Lookup("kind").Value.String(), "text": text}
	var id string
	if sp.Kind == kindPrivate {
		machine := hostName()
		id = fmt.Sprintf("%s:%d", machine, time.Now().UnixMilli())
		fields["machine"] = machine
		if s, _ := resolveSession(st, fs.Lookup("s").Value.String()); s != "" {
			fields["session"] = s
		}
	} else {
		id = fmt.Sprintf("%s:%d", shortId(st.Identity), time.Now().UnixMilli()) // no machine name leaves the private space
	}
	if ref := fs.Lookup("ref").Value.String(); ref != "" {
		fields["ref"] = parseObjectId(ref)
	}
	res, err := c.Upsert(sp.Id, sp.owner("app"), sp.coll(dsFeedback), []UpsertRecord{{Id: id, Fields: fields}})
	if err != nil {
		fatal(err)
	}
	for _, rej := range res.Rejections {
		fatal(fmt.Errorf("rejected: %s", rej.Reason))
	}
	fmt.Println("recorded", id)
}

// --- stats report ---

func day(ts float64) string {
	if ts == 0 {
		return "-"
	}
	return time.Unix(int64(ts), 0).Format("01-02 15:04")
}

func cmdStats(args []string) {
	fs, pos := parseArgs("stats", args, func(fs *flag.FlagSet) {
		fs.Int("days", 14, "how many days back to report")
		fs.Bool("global", false, "all projects + usage rollups (default: current project only)")
	})
	c := NewClient()
	sp := mustSetup(c).private()
	flushStats(c, sp)
	num := func(r map[string]any, k string) int { return int(recordFloat(r, k)) }
	root := sp.owner("app")
	if fs.Lookup("global").Value.String() != "true" {
		scope := ""
		if len(pos) > 0 {
			scope = pos[0]
		} else {
			cwd, _ := os.Getwd()
			scope = resolveScope(cwd)
		}
		fmt.Printf("== project %s   (run `any-agents stats --global` for all projects)\n", scope)
		total, last := 0, 0.0
		var kinds []string
		if rows, err := c.Aggregate(sp.Id, root, sp.coll(dsMemories), []map[string]any{
			{"$match": map[string]any{"scope": scope, "kind": map[string]any{"$ne": kindRetracted}}},
			{"$group": map[string]any{"_id": "$kind", "n": map[string]any{"$sum": 1}, "last": map[string]any{"$max": "$updatedAt"}}},
			{"$sort": map[string]any{"n": -1}},
		}); err == nil {
			for _, r := range rows {
				kind, _ := r["id"].(string)
				total += num(r, "n")
				kinds = append(kinds, fmt.Sprintf("%s %d", kind, num(r, "n")))
				if l := recordFloat(r, "last"); l > last {
					last = l
				}
			}
		}
		fmt.Printf("memories   %d (%s), last update %s\n", total, strings.Join(kinds, " | "), day(last))
		cmdSession([]string{"list", "--project", scope, "-n", "5"})
		if rows, err := c.QueryRecordsAll(sp.Id, root, sp.coll(dsFeedback),
			map[string]any{"ref": map[string]any{"$regex": "^" + scope + ":"}}, nil); err == nil && len(rows) > 0 {
			fmt.Println("feedback on this project's memories:")
			for _, r := range rows {
				fmt.Printf("  [%s] %s — %s\n", r["kind"], r["ref"], trunc(fmt.Sprint(r["text"]), 90))
			}
		}
		return
	}
	fmt.Println("== projects")
	cmdProject(nil)
	fmt.Println("\n== machines")
	cmdMachine(nil)
	fmt.Println("\n== recent sessions")
	cmdSession([]string{"list", "-n", "8"})
	fmt.Println("\n== data")
	if rows, err := c.Aggregate(sp.Id, root, sp.coll(dsMemories), []map[string]any{
		{"$match": map[string]any{"kind": map[string]any{"$ne": kindRetracted}}},
		{"$group": map[string]any{"_id": "$kind", "n": map[string]any{"$sum": 1}}}, {"$sort": map[string]any{"n": -1}},
	}); err == nil {
		var parts []string
		for _, r := range rows {
			parts = append(parts, fmt.Sprintf("%s %d", r["id"], num(r, "n")))
		}
		fmt.Printf("memory kinds  %s\n", strings.Join(parts, " | "))
	}
	if rows, err := c.Aggregate(sp.Id, root, sp.coll(dsConfig), []map[string]any{{"$count": "n"}}); err == nil && len(rows) > 0 {
		fmt.Printf("config files  %d\n", num(rows[0], "n"))
	}
	if files, err := c.FileList(sp.Id); err == nil {
		var bytes int64
		for _, f := range files {
			bytes += f.Size
		}
		fmt.Printf("files         %d attachment(s), %.1f MB\n", len(files), float64(bytes)/(1<<20))
	}
	fmt.Println("\n== usage")
	cutoff := time.Now().AddDate(0, 0, -atoi(fs.Lookup("days").Value.String())).Format("2006-01-02")
	rows, err := c.QueryRecordsAll(sp.Id, root, sp.coll(dsStats),
		map[string]any{"day": map[string]any{"$gte": cutoff}}, []string{"day"})
	if err != nil {
		fatal(err)
	}
	fmt.Printf("%-11s %-12s %5s %5s %5s %6s %4s %4s %5s %5s %4s\n",
		"day", "machine", "pulls", "pushes", "confl", "synced", "rag", "inj", "srch", "sess", "err")
	totals := map[string]int{}
	var allErrs []string
	for _, r := range rows {
		fmt.Printf("%-11s %-12s %5d %5d %5d %6d %4d %4d %5d %5d %4d\n",
			r["day"], r["machine"], num(r, "pulls"), num(r, "pushes"), num(r, "conflicts"),
			num(r, "memSynced"), num(r, "ragPrompts"), num(r, "ragInjected"),
			num(r, "searches"), num(r, "sessions"), num(r, "errors"))
		for _, k := range statCounters {
			totals[k] += num(r, k)
		}
		if arr, ok := r["lastErrors"].([]any); ok {
			for _, e := range arr {
				allErrs = append(allErrs, fmt.Sprintf("%s %s: %v", r["day"], r["machine"], e))
			}
		}
	}
	fmt.Printf("%-24s %5d %5d %5d %6d %4d %4d %5d %5d %4d\n", "totals",
		totals["pulls"], totals["pushes"], totals["conflicts"], totals["memSynced"],
		totals["ragPrompts"], totals["ragInjected"], totals["searches"], totals["sessions"], totals["errors"])
	if len(allErrs) > 0 {
		fmt.Println("\nrecent errors:")
		for _, e := range allErrs {
			fmt.Println("  " + e)
		}
	}
	if fb, err := c.QueryRecordsAll(sp.Id, root, sp.coll(dsFeedback), nil, []string{"-createdAt"}); err == nil && len(fb) > 0 {
		fmt.Println("\nfeedback:")
		for i, r := range fb {
			if i == 15 {
				fmt.Printf("  … %d more\n", len(fb)-15)
				break
			}
			ref := ""
			if s, _ := r["ref"].(string); s != "" {
				ref = " → " + s
			}
			fmt.Printf("  [%s]%s %s\n", r["kind"], ref, trunc(fmt.Sprint(r["text"]), 110))
		}
	}
}
