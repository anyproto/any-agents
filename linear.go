package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Linear issues are objects: one per issue key, minted as a deterministic
// child of the app root (seed linear:<KEY>) so every machine agrees on
// the id, enriched from `linctl issue get --json` when linctl is
// available. Docs, tasks and plans point at them through the `linear`
// relation; an issue's backlinks are everything the agents did for it.

var linearKeyRe = regexp.MustCompile(`^[A-Z][A-Z0-9]+-[0-9]+$`)

// linctlPath finds linctl on PATH or at $ANY_AGENTS_LINCTL.
func linctlPath() string {
	if p := os.Getenv("ANY_AGENTS_LINCTL"); p != "" {
		return p
	}
	if p, err := exec.LookPath("linctl"); err == nil {
		return p
	}
	return ""
}

type linearIssue struct {
	Identifier    string `json:"identifier"`
	Title         string `json:"title"`
	Description   string `json:"description"`
	URL           string `json:"url"`
	PriorityLabel string `json:"priorityLabel"`
	BranchName    string `json:"branchName"`
	DueDate       string `json:"dueDate"`
	State         struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"state"`
	Team struct {
		Key string `json:"key"`
	} `json:"team"`
	Assignee struct {
		DisplayName string `json:"displayName"`
	} `json:"assignee"`
}

// fetchLinear reads one issue through linctl; nil when linctl is absent
// or the call fails (the object is still created from the key alone).
func fetchLinear(key string) *linearIssue {
	bin := linctlPath()
	if bin == "" {
		return nil
	}
	out, err := exec.Command(bin, "issue", "get", key, "--json").Output()
	if err != nil {
		return nil
	}
	var li linearIssue
	if json.Unmarshal(out, &li) != nil || li.Identifier == "" {
		return nil
	}
	return &li
}

func linearStatusOf(li *linearIssue) string {
	switch li.State.Type {
	case "backlog", "triage":
		return "backlog"
	case "unstarted":
		return "todo"
	case "started":
		if strings.Contains(strings.ToLower(li.State.Name), "review") {
			return "in_review"
		}
		return "in_progress"
	case "completed":
		return "done"
	case "canceled", "cancelled":
		return "canceled"
	}
	return "todo"
}

func linearPriorityOf(li *linearIssue) string {
	switch strings.ToLower(li.PriorityLabel) {
	case "urgent", "high", "medium", "low":
		return strings.ToLower(li.PriorityLabel)
	}
	return "none"
}

// ensureLinearIssue resolves (and refreshes) the object for an issue key.
func ensureLinearIssue(c *Client, sp *Space, key string) (string, error) {
	key = strings.ToUpper(strings.TrimSpace(key))
	if !linearKeyRe.MatchString(key) {
		return "", fmt.Errorf("%q is not a Linear issue key (like ABC-123)", key)
	}
	id, err := c.BundleChild(sp.Id, bundleApp, "linear:"+key, pageType, []string{sp.owner("linear")})
	if err != nil {
		return "", err
	}
	row, err := c.GetObject(sp.Id, id)
	if err != nil {
		return "", err
	}
	fresh := nameOf(row) == ""
	li := fetchLinear(key)
	p := map[string]any{"key": key, "syncedAt": dateVal(time.Now())}
	name := key
	if li != nil {
		name = key + " · " + li.Title
		p["url"] = li.URL
		p["status"] = []string{linearStatusOf(li)}
		p["team"] = li.Team.Key
		p["priority"] = []string{linearPriorityOf(li)}
		p["assignee"] = li.Assignee.DisplayName
		p["branch"] = li.BranchName
		if li.DueDate != "" {
			if t, err := time.Parse("2006-01-02", li.DueDate); err == nil {
				p["dueDate"] = dateVal(t)
			}
		}
	}
	if fresh || li != nil {
		if err := c.SetProperties(sp.Id, id, "any", map[string]any{"name": name}); err != nil {
			return "", err
		}
	}
	if err := c.SetProperties(sp.Id, id, sp.owner("linear"), props(sp, "linear", p)); err != nil {
		return "", err
	}
	if li != nil {
		body := fmt.Sprintf("%s\n\nSynced from Linear %s.\n", strings.TrimSpace(li.Description), time.Now().Format("2006-01-02 15:04"))
		if err := c.MarkdownPut(sp.Id, id, body); err != nil {
			return "", err
		}
	} else if fresh {
		_ = c.MarkdownPut(sp.Id, id, "Linear issue (details not synced — run `any-agents linear sync "+key+"` with linctl installed).\n")
	}
	return id, nil
}

// linearLinks resolves a comma-separated list of keys to relation values.
func linearLinks(c *Client, sp *Space, keys string) ([]string, error) {
	var ids []string
	for _, k := range strings.Split(keys, ",") {
		if k = strings.TrimSpace(k); k == "" {
			continue
		}
		id, err := ensureLinearIssue(c, sp, k)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return linkList(ids), nil
}

func cmdLinear(args []string) {
	if len(args) == 0 {
		fatal(fmt.Errorf("usage: linear list [--all] | show <KEY> | sync [KEY…]"))
	}
	c := NewClient()
	sp := mustSetup(c).private()
	t := sp.owner("linear")
	switch args[0] {
	case "list":
		fs, _ := parseArgs("linear list", args[1:], func(fs *flag.FlagSet) { fs.Bool("all", false, "include done and canceled") })
		var extra []map[string]any
		if fs.Lookup("all").Value.String() != "true" {
			extra = append(extra, map[string]any{propPath(sp, "linear", "status"): map[string]any{"$nin": []string{"done", "canceled"}}})
		}
		rows, err := objectsOf(c, sp, "linear", []string{propPath(sp, "linear", "key")}, extra...)
		if err != nil {
			fatal(err)
		}
		for _, r := range rows {
			fmt.Printf("%s  %-9s %-11s %-7s %s\n", globalLink(sp.Id, r["id"].(string)),
				strOf(r, t, sp.prop("linear", "key")), choiceOf(r, t, sp.prop("linear", "status")),
				choiceOf(r, t, sp.prop("linear", "priority")), linearTitle(nameOf(r)))
		}
	case "show":
		if len(args) < 2 {
			fatal(fmt.Errorf("linear show <KEY>"))
		}
		id, err := ensureLinearIssue(c, sp, args[1])
		if err != nil {
			fatal(err)
		}
		showObject(c, sp, id, false)
		edges, _ := c.Backlinks(sp.Id, id)
		seen := map[string]bool{}
		for _, e := range edges {
			if seen[e.Source.ObjectId] {
				continue
			}
			seen[e.Source.ObjectId] = true
			if row, err := c.GetObject(sp.Id, e.Source.ObjectId); err == nil {
				fmt.Printf("← %s  %s\n", globalLink(sp.Id, e.Source.ObjectId), nameOf(row))
			}
		}
	case "sync":
		keys := args[1:]
		if len(keys) == 0 {
			rows, err := objectsOf(c, sp, "linear", nil)
			if err != nil {
				fatal(err)
			}
			for _, r := range rows {
				keys = append(keys, strOf(r, t, sp.prop("linear", "key")))
			}
		}
		if linctlPath() == "" {
			fatal(fmt.Errorf("linctl not found (needed to read Linear)"))
		}
		for _, k := range keys {
			if _, err := ensureLinearIssue(c, sp, k); err != nil {
				fmt.Fprintf(os.Stderr, "any-agents: %s: %v\n", k, err)
				continue
			}
			fmt.Println("synced", k)
		}
	default:
		fatal(fmt.Errorf("unknown linear command %q", args[0]))
	}
}

// linearTitle strips the "KEY · " prefix an issue object's name carries.
func linearTitle(name string) string {
	if _, t, ok := strings.Cut(name, " · "); ok {
		return t
	}
	return ""
}
