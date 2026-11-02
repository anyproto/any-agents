package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// renderStatusPage regenerates the app root's body: the one page a
// human opens to see what the agents are doing. PUT diffs blocks, so an
// unchanged page writes nothing.
func renderStatusPage(c *Client, sp *Space) error {
	root := sp.owner("app")
	projects := nameIndex(c, sp, "project")
	link := func(id string) string { return globalLink(sp.Id, id) }
	var b strings.Builder
	if sp.Kind == kindCollab {
		fmt.Fprintf(&b, "Shared context of this space's coding agents. Updated %s.\n",
			time.Now().Format("2006-01-02 15:04"))
	} else {
		fmt.Fprintf(&b, "Private context of this account's coding agents. Updated %s by %s.\n",
			time.Now().Format("2006-01-02 15:04"), hostName())
	}

	if sp.has("collab") {
		members := memberIndex(c, sp)
		collabs, _ := objectsOf(c, sp, "collab", []string{"-" + propPath(sp, "collab", "date")},
			map[string]any{propPath(sp, "collab", "status"): map[string]any{"$in": []string{"request", "in_progress"}}})
		b.WriteString("\n## Open collabs\n\n")
		if len(collabs) == 0 {
			b.WriteString("Nothing open. `any-agents collab open` files a help request here.\n")
		}
		for _, r := range collabs {
			t := sp.owner("collab")
			who := members[identityOf(firstMember(r, t, sp.prop("collab", "requester")))]
			var helpers []string
			for _, m := range membersOf(r, t, sp.prop("collab", "assignees")) {
				helpers = append(helpers, members[identityOf(m)])
			}
			extra := ""
			if len(helpers) > 0 {
				extra = " with " + strings.Join(helpers, ", ")
			}
			fmt.Fprintf(&b, "- %s [%s](%s) by %s%s\n", choiceOf(r, t, sp.prop("collab", "status")), nameOf(r), link(r["id"].(string)), who, extra)
		}
	}

	machines := nameIndex(c, sp, "machine")
	tasks, _ := objectsOf(c, sp, "task", []string{"modifiedAt"},
		map[string]any{propPath(sp, "task", "status"): map[string]any{"$in": []string{"todo", "in_progress"}}})
	b.WriteString("\n## Open tasks\n\n")
	if len(tasks) == 0 {
		b.WriteString("Nothing pending. File a page under Task and the next session picks it up.\n")
	}
	for _, t := range tasks {
		status := choiceOf(t, sp.owner("task"), sp.prop("task", "status"))
		proj := projects[firstLink(t, sp, "doc", "project")]
		if proj == "" {
			proj = "any project"
		}
		extra := ""
		if status == "in_progress" {
			if m := machines[firstLink(t, sp, "task", "handledBy")]; m != "" {
				extra = " — in progress on " + m
			} else {
				extra = " — in progress"
			}
		}
		fmt.Fprintf(&b, "- [ ] [%s](%s) (%s)%s\n", nameOf(t), link(t["id"].(string)), proj, extra)
	}

	sessions, _ := objectsOf(c, sp, "session", []string{"-" + propPath(sp, "session", "startedAt")})
	if sp.Kind == kindPrivate || len(sessions) > 0 {
		b.WriteString("\n## Sessions\n\n")
		active := 0
		for _, s := range sessions {
			if choiceOf(s, sp.owner("session"), sp.prop("session", "status")) == "active" {
				active++
			}
		}
		fmt.Fprintf(&b, "%d active, %d total.\n\n", active, len(sessions))
		for i, s := range sessions {
			if i == 8 {
				break
			}
			t := sp.owner("session")
			summary := strOf(s, t, sp.prop("session", "summary"))
			if summary != "" {
				summary = " — " + trunc(summary, 120)
			}
			on := machines[firstLink(s, sp, "session", "machine")]
			if on == "" {
				on = strOf(s, t, sp.prop("session", "host"))
			}
			fmt.Fprintf(&b, "- %s [%s](%s) %s on %s%s\n", choiceOf(s, t, sp.prop("session", "status")),
				nameOf(s), link(s["id"].(string)), projects[firstLink(s, sp, "session", "project")], on, summary)
		}
	}

	docs, _ := objectsOf(c, sp, "doc", nil, map[string]any{propPath(sp, "doc", "archived"): map[string]any{"$ne": true}})
	b.WriteString("\n## Recent docs\n\n")
	n := 0
	for _, d := range docs {
		if sp.isA(d, "task") {
			continue
		}
		if n++; n > 10 {
			break
		}
		proj := ""
		if p := projects[firstLink(d, sp, "doc", "project")]; p != "" {
			proj = " (" + p + ")"
		}
		fmt.Fprintf(&b, "- %s [%s](%s)%s\n", choiceOf(d, sp.owner("doc"), sp.prop("doc", "kind")),
			nameOf(d), link(d["id"].(string)), proj)
	}

	if sp.Kind == kindPrivate {
		b.WriteString("\n## Memory\n\n")
		if rows, err := c.Aggregate(sp.Id, root, sp.coll(dsMemories), []map[string]any{
			{"$match": map[string]any{"kind": map[string]any{"$ne": kindRetracted}}},
			{"$group": map[string]any{"_id": "$scope", "n": map[string]any{"$sum": 1}, "last": map[string]any{"$max": "$updatedAt"}}},
			{"$sort": map[string]any{"n": -1}},
		}); err == nil {
			total := 0
			var lines []string
			for _, r := range rows {
				scope, _ := r["id"].(string)
				cnt := int(recordFloat(r, "n"))
				total += cnt
				lines = append(lines, fmt.Sprintf("| %s | %d | %s |", scope, cnt, day(recordFloat(r, "last"))))
			}
			fmt.Fprintf(&b, "%d memories across %d projects.\n\n| project | memories | last update |\n|---|---|---|\n%s\n", total, len(rows), strings.Join(lines, "\n"))
		}

		b.WriteString("\n## Machines\n\n")
		ms, _ := objectsOf(c, sp, "machine", []string{"any.name"})
		sort.Slice(ms, func(i, j int) bool { return nameOf(ms[i]) < nameOf(ms[j]) })
		for _, m := range ms {
			t := sp.owner("machine")
			fmt.Fprintf(&b, "- [%s](%s) %s, last seen %s\n", nameOf(m), link(m["id"].(string)),
				choiceOf(m, t, sp.prop("machine", "status")), day(recordFloat(group(m, t), sp.prop("machine", "lastSeen"))))
		}
	}
	return c.MarkdownPut(sp.Id, root, b.String())
}
