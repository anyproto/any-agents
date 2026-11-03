package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Releases: one page per shipped version of a project, filed under the
// Release collection, the release notes as the body. Other agents learn
// what changed through recall over the notes, the latest release of the
// current project in the session-start context and the status page. A
// newer release of any-agents itself becomes an update notice.

var ghPR = regexp.MustCompile(`https?://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/pull/\d+`)

// prLink is the first GitHub pull-request link in a body, or "".
func prLink(body string) string { return ghPR.FindString(body) }

// prShort renders a PR link as owner/repo#N for listings.
func prShort(u string) string {
	parts := strings.Split(strings.TrimSuffix(u, "/"), "/")
	if len(parts) < 7 {
		return u
	}
	return parts[3] + "/" + parts[4] + "#" + parts[6]
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func cmdRelease(args []string) {
	if len(args) == 0 {
		fatal(fmt.Errorf("usage: release add [notes.md|-] --version V [--project P] [--url U] [--released YYYY-MM-DD] [--breaking] [--api-changes] [--includes P@V,…] [--gh owner/repo@tag] [--announce] [--space S] | list [--project P] [--space S] | show <id>"))
	}
	c := NewClient()
	st := mustSetup(c)
	switch args[0] {
	case "add":
		releaseAdd(c, st, args[1:])
	case "list":
		fs, _ := parseArgs("release list", args[1:], func(fs *flag.FlagSet) {
			spaceFlag(fs)
			fs.String("project", "", "only this project scope")
			fs.Int("n", 30, "how many")
		})
		get := func(k string) string { return fs.Lookup(k).Value.String() }
		n, _ := strconv.Atoi(get("n"))
		releaseList(c, spaceOf(st, get("space")), get("project"), n)
	case "show":
		fs, pos := parseArgs("release show", args[1:], spaceFlag)
		if len(pos) < 1 {
			fatal(fmt.Errorf("release show <id>"))
		}
		sp, id := objectIn(st, spaceOf(st, fs.Lookup("space").Value.String()), pos[0])
		releaseShow(c, sp, id)
	default:
		fatal(fmt.Errorf("unknown release command %q", args[0]))
	}
}

type ghReleaseInfo struct {
	Name        string `json:"name"`
	Body        string `json:"body"`
	URL         string `json:"url"`
	TagName     string `json:"tagName"`
	PublishedAt string `json:"publishedAt"`
}

// ghRelease reads a GitHub release through the gh CLI.
func ghRelease(repo, tag string) (*ghReleaseInfo, error) {
	out, err := exec.Command("gh", "release", "view", tag, "-R", repo, "--json", "name,body,url,tagName,publishedAt").Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("gh release view %s@%s: %s", repo, tag, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("gh release view: %v (pass the notes, --version and --url by hand)", err)
	}
	var r ghReleaseInfo
	if err := json.Unmarshal(out, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func releaseAdd(c *Client, st *State, args []string) {
	fs, pos := parseArgs("release add", args, func(fs *flag.FlagSet) {
		spaceFlag(fs)
		fs.String("project", "", "project scope (default: the --gh repo, else the cwd repo)")
		fs.String("version", "", "version, e.g. v0.3.5 (default: the --gh tag)")
		fs.String("url", "", "release page URL (default: the --gh release URL)")
		fs.String("released", "", "release date YYYY-MM-DD (default: the --gh publish date, else today)")
		fs.Bool("breaking", false, "the release has a breaking change")
		fs.Bool("api-changes", false, "the release changes an API")
		fs.String("includes", "", "bundled releases, comma-separated project@version, already in the space")
		fs.String("gh", "", "owner/repo@tag: notes, URL and date from the GitHub release (needs gh)")
		fs.Bool("announce", false, "post the release with its link into the space's general chat")
	})
	get := func(k string) string { return fs.Lookup(k).Value.String() }
	sp := spaceOf(st, get("space"))
	if !sp.has("release") {
		fatal(fmt.Errorf("the Release bundle is not installed in %s", sp.Name))
	}
	body := ""
	if len(pos) > 0 {
		data, err := readSource(pos[0])
		if err != nil {
			fatal(err)
		}
		body = string(data)
	}
	project, ver, url := get("project"), get("version"), get("url")
	var when time.Time
	if gh := get("gh"); gh != "" {
		repo, tag, ok := strings.Cut(gh, "@")
		if !ok || repo == "" || tag == "" {
			fatal(fmt.Errorf("--gh takes owner/repo@tag"))
		}
		rel, err := ghRelease(repo, tag)
		if err != nil {
			fatal(err)
		}
		body = firstNonEmpty(body, rel.Body)
		ver = firstNonEmpty(ver, rel.TagName)
		url = firstNonEmpty(url, rel.URL)
		if project == "" {
			project = slugify(repo[strings.LastIndex(repo, "/")+1:])
		}
		if t, err := time.Parse(time.RFC3339, rel.PublishedAt); err == nil {
			when = t
		}
	}
	if project == "" {
		cwd, _ := os.Getwd()
		project = resolveScope(cwd)
	}
	if ver == "" {
		fatal(fmt.Errorf("release add needs --version (or --gh owner/repo@tag)"))
	}
	if strings.TrimSpace(body) == "" {
		fatal(fmt.Errorf("release add needs the notes: a file, - for stdin, or --gh"))
	}
	if d := get("released"); d != "" {
		t, err := time.Parse("2006-01-02", d)
		if err != nil {
			fatal(fmt.Errorf("--released takes YYYY-MM-DD"))
		}
		when = t
	}
	if when.IsZero() {
		when = time.Now()
	}
	when = time.Date(when.Year(), when.Month(), when.Day(), 0, 0, 0, 0, time.UTC)
	pid, err := ensureProject(c, sp, project)
	if err != nil {
		fatal(err)
	}
	name := project + " " + ver
	rp := map[string]any{
		"project": linkList([]string{pid}), "version": ver, "released": dateVal(when),
		"breaking": get("breaking") == "true", "apiChanges": get("api-changes") == "true",
		"author": []string{st.me()},
	}
	if url != "" {
		rp["url"] = url
	}
	if v := get("includes"); v != "" {
		ids, err := releaseRefs(c, sp, v)
		if err != nil {
			fatal(err)
		}
		rp["includes"] = linkList(ids)
	}
	group := props(sp, "release", rp)
	var id string
	if row := findRelease(c, sp, pid, ver); row != nil {
		id, _ = row["id"].(string)
		if err := c.SetProperties(sp.Id, id, sp.owner("release"), group); err != nil {
			fatal(err)
		}
		if err := putBody(c, sp, id, name, body); err != nil {
			fatal(err)
		}
		fmt.Fprintf(os.Stderr, "updated existing %s\n", name)
	} else {
		id, err = newObject(c, sp, pageType, []string{sp.owner("release")}, name,
			map[string]map[string]any{sp.owner("release"): group}, body)
		if err != nil {
			fatal(err)
		}
	}
	_ = renderStatusPage(c, sp)
	if get("announce") == "true" {
		if err := announceRelease(c, st, sp, id, name, url); err != nil {
			fmt.Fprintf(os.Stderr, "any-agents: announce: %v\n", err)
		}
	}
	fmt.Printf("%s  %s\n", globalLink(sp.Id, id), name)
}

// findRelease is the space's release page of one project and version.
func findRelease(c *Client, sp *Space, projectId, ver string) map[string]any {
	rows, err := objectsOf(c, sp, "release", nil, map[string]any{propPath(sp, "release", "version"): ver})
	if err != nil {
		return nil
	}
	for _, r := range rows {
		if firstLink(r, sp, "release", "project") == projectId {
			return r
		}
	}
	return nil
}

// releaseRefs resolves project@version refs to release page ids.
func releaseRefs(c *Client, sp *Space, csv string) ([]string, error) {
	projects := map[string]string{} // scope → id
	for id, name := range nameIndex(c, sp, "project") {
		projects[name] = id
	}
	var ids []string
	for _, ref := range strings.Split(csv, ",") {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		project, ver, ok := strings.Cut(ref, "@")
		if !ok {
			return nil, fmt.Errorf("--includes takes project@version, got %q", ref)
		}
		row := findRelease(c, sp, projects[project], ver)
		if row == nil {
			return nil, fmt.Errorf("no release %s %s in %s; add it first", project, ver, sp.Name)
		}
		id, _ := row["id"].(string)
		ids = append(ids, id)
	}
	return ids, nil
}

// announceRelease posts the release into the space's general chat.
func announceRelease(c *Client, st *State, sp *Space, id, name, url string) error {
	res, err := c.CatalogSetup(sp.Id, "general-chat")
	if err != nil {
		return err
	}
	if len(res) == 0 || res[0].Bundle.RootId == "" {
		return fmt.Errorf("no general chat in %s", sp.Name)
	}
	text := fmt.Sprintf("**%s is out.** Release notes: %s", name, globalLink(sp.Id, id))
	if url != "" {
		text += "\n" + url
	}
	_, err = c.ChatSend(sp.Id, res[0].Bundle.RootId, text, agentEnvelope(c, st, sp))
	return err
}

// dateOf reads a datetime property as YYYY-MM-DD.
func dateOf(row map[string]any, ownerId, propId string) string {
	v := group(row, ownerId)[propId]
	if m, ok := v.(map[string]any); ok {
		if s, ok := m["$date"].(string); ok && len(s) >= 10 {
			return s[:10]
		}
	}
	return ""
}

func boolOf(row map[string]any, ownerId, propId string) bool {
	b, _ := group(row, ownerId)[propId].(bool)
	return b
}

func releaseFlags(row map[string]any, sp *Space) string {
	o := sp.owner("release")
	var f []string
	if boolOf(row, o, sp.prop("release", "breaking")) {
		f = append(f, "breaking")
	}
	if boolOf(row, o, sp.prop("release", "apiChanges")) {
		f = append(f, "api-changes")
	}
	return strings.Join(f, ",")
}

func releaseList(c *Client, sp *Space, project string, n int) {
	if !sp.has("release") {
		fatal(fmt.Errorf("the Release bundle is not installed in %s", sp.Name))
	}
	rows, err := objectsOf(c, sp, "release", []string{"-" + propPath(sp, "release", "released")})
	if err != nil {
		fatal(err)
	}
	projects := nameIndex(c, sp, "project")
	o := sp.owner("release")
	shown := 0
	for _, r := range rows {
		proj := projects[firstLink(r, sp, "release", "project")]
		if project != "" && proj != project {
			continue
		}
		if shown == n {
			fmt.Printf("… more; raise -n\n")
			break
		}
		shown++
		id, _ := r["id"].(string)
		fmt.Printf("%s  %-14s %-10s %s  %-20s %s\n", globalLink(sp.Id, id), pad(proj, 14), pad(strOf(r, o, sp.prop("release", "version")), 10),
			dateOf(r, o, sp.prop("release", "released")), pad(releaseFlags(r, sp), 20), strOf(r, o, sp.prop("release", "url")))
	}
}

func releaseShow(c *Client, sp *Space, id string) {
	row, err := c.GetObject(sp.Id, id)
	if err != nil {
		fatal(err)
	}
	o := sp.owner("release")
	projects := nameIndex(c, sp, "project")
	fmt.Printf("# %s\n%s\n", nameOf(row), globalLink(sp.Id, id))
	meta := []string{
		"project=" + projects[firstLink(row, sp, "release", "project")],
		"version=" + strOf(row, o, sp.prop("release", "version")),
		"released=" + dateOf(row, o, sp.prop("release", "released")),
	}
	if f := releaseFlags(row, sp); f != "" {
		meta = append(meta, "flags="+f)
	}
	if u := strOf(row, o, sp.prop("release", "url")); u != "" {
		meta = append(meta, "url="+u)
	}
	if ids := linksOf(row, o, sp.prop("release", "includes")); len(ids) > 0 {
		names := nameIndex(c, sp, "release")
		var inc []string
		for _, i := range ids {
			inc = append(inc, names[i])
		}
		meta = append(meta, "includes="+strings.Join(inc, ","))
	}
	fmt.Println(strings.Join(meta, "  "))
	body, err := c.MarkdownGet(sp.Id, id)
	if err != nil {
		fatal(err)
	}
	fmt.Println()
	fmt.Println(strings.TrimSpace(body))
}

// latestRelease is the newest release page of a project scope in a space.
func latestRelease(c *Client, sp *Space, scope string) map[string]any {
	if !sp.has("release") {
		return nil
	}
	pid := ""
	for id, name := range nameIndex(c, sp, "project") {
		if name == scope {
			pid = id
			break
		}
	}
	if pid == "" {
		return nil
	}
	rows, err := objectsOf(c, sp, "release", []string{"-" + propPath(sp, "release", "released")})
	if err != nil {
		return nil
	}
	for _, r := range rows {
		if firstLink(r, sp, "release", "project") == pid {
			return r
		}
	}
	return nil
}

// releaseDigest is the session-start note: the latest release of the
// current project per space, and an update notice when a newer
// any-agents has been released.
func releaseDigest(c *Client, st *State, cwd string) string {
	scope := resolveScope(cwd)
	var b strings.Builder
	var newest map[string]any
	var newestSp *Space
	for _, sp := range append([]*Space{st.private()}, st.collabs()...) {
		if !sp.ready() {
			continue
		}
		if scope != "any-agents" {
			if r := latestRelease(c, sp, scope); r != nil {
				o := sp.owner("release")
				id, _ := r["id"].(string)
				fmt.Fprintf(&b, "Latest release of %s: %s (%s) %s — `any-agents release show` prints the notes.\n",
					scope, strOf(r, o, sp.prop("release", "version")), dateOf(r, o, sp.prop("release", "released")), globalLink(sp.Id, id))
			}
		}
		if r := latestRelease(c, sp, "any-agents"); r != nil {
			v := strOf(r, sp.owner("release"), sp.prop("release", "version"))
			if newest == nil || semverLess(strOf(newest, newestSp.owner("release"), newestSp.prop("release", "version")), v) {
				newest, newestSp = r, sp
			}
		}
	}
	if newest != nil {
		o := newestSp.owner("release")
		v := strOf(newest, o, newestSp.prop("release", "version"))
		if semverLess(version, v) {
			id, _ := newest["id"].(string)
			fmt.Fprintf(&b, "any-agents %s is out (this machine runs %s): in the any-agents clone `git pull --ff-only && make build && bin/any-agents install`, or the release archive", v, version)
			if u := strOf(newest, o, newestSp.prop("release", "url")); u != "" {
				fmt.Fprintf(&b, " at %s", u)
			}
			fmt.Fprintf(&b, ". Notes: %s\n", globalLink(newestSp.Id, id))
		}
	}
	return b.String()
}

// semverLess reports a < b for vX.Y.Z strings (a leading v is optional;
// anything unparsable sorts first).
func semverLess(a, b string) bool {
	pa, pb := semverParts(a), semverParts(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return false
}

func semverParts(v string) [3]int {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	for i, s := range strings.SplitN(v, ".", 3) {
		n, _ := strconv.Atoi(s)
		out[i] = n
	}
	return out
}
