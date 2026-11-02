package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

const usage = `any-agents — shared context for coding agents, backed by any spaces

  install [--create-space]     resolve or create the private space + bundles, copy the binary, merge the tool's hooks
  status | version             show state / versions
  pull [dir] [--allow-deletions]   mirror memories + config down     (session-start hook)
  push [dir]                   mirror memories + config up            (session-end / post-tool hooks)
  search <query> [-n N] [--json] [--space S]

  doc  add <file|->  [--title T] [--kind K] [--project P] [--linear L] [--related ID,…] [--new] [--space S]
  doc  list [--kind K] [--project P] [--all] [--space S] | show <id> | append <id> <text|file|-> | edit <id> --old S --new S
  doc  archive <id> | link <id> <objectId>
  task add <file|->  [--title T] [--project P] [--priority PR] [--assignee M] [--target HOST] [--space S]
  task list [--all] [--mine] [--project P] [--space S] | show <id> | start <id> | done <id> | cancel <id> | link <id> <objectId>
  plan add [<taskId>] <file|-> [--status draft|approved] [--replace] | list [--all] | show <id> | approve <id> | done <id>
  session note <text|file|-> | show | update [--summary S] [--title T] [--linear KEYS] | list [--project P] | transcript [-o FILE]   (-s ID)
  session share --space S [--transcript]     copy this session's page into a collab space
  collab open <file|-> --space S [--title T] [--to MEMBER,…] [--project P] [--linear L]
  collab list [--space S] [--mine] [--status X] | show <id> [-n N] | join <id> | say <id> <text|file|->
  collab wait <id> [--timeout D] | done <id> | cancel <id>
  memory share <slug|file.md> --space S [--scope P] | list [--space S] [--scope P] | unshare <slug> --space S
  space list | add <name|id> | create <name> | remove <name|id>
  project list | show <scope> [--space S]      machine list
  linear list [--all] | show <KEY> | sync [KEY…]      Linear issue objects (enriched via linctl)
  stats [project] [--global] [--days N]     feedback <text> [--kind K] [--ref ID]     gc [--apply]     eval [--golden FILE]
  hook [--tool T] session-start|session-end|post-tool|prompt

Ids accept an object id or an any://o/<space>/<id> link; a link names its space.
Aliases: save = doc add · prompt = task · scratch append|show = session note|show
Env: ANY_AGENTS_ADDR (127.0.0.1:2046), ANY_AGENTS_SPACE (Agents), ANY_AGENTS_RAG_MIN (0.030), ANY_AGENTS_TOOL (claude), ANY_AGENTS_HOME (~/.any-agents)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}
	args := os.Args[2:]
	switch os.Args[1] {
	case "install":
		cmdInstall(args)
	case "status":
		cmdStatus()
	case "version":
		fmt.Println("any-agents", version)
	case "pull":
		cmdPull(args)
	case "push":
		cmdPush(args)
	case "search":
		cmdSearch(args)
	case "doc":
		cmdDoc(args)
	case "save":
		cmdDoc(append([]string{"add"}, args...))
	case "task", "prompt":
		cmdTask(args)
	case "plan":
		cmdPlan(args)
	case "session":
		cmdSession(args)
	case "scratch":
		if len(args) > 0 && args[0] == "append" {
			args[0] = "note"
		}
		cmdSession(args)
	case "collab":
		cmdCollab(args)
	case "memory":
		cmdMemory(args)
	case "space":
		cmdSpace(args)
	case "project":
		cmdProject(args)
	case "machine":
		cmdMachine(args)
	case "linear":
		cmdLinear(args)
	case "stats":
		cmdStats(args)
	case "feedback":
		cmdFeedback(args)
	case "gc":
		cmdGC(args)
	case "eval":
		cmdEval(args)
	case "hook":
		fs, pos := parseArgs("hook", args, func(fs *flag.FlagSet) {
			fs.String("tool", "", "the harness that fired the hook (default: $ANY_AGENTS_TOOL, claude)")
		})
		if len(pos) < 1 {
			fatal(fmt.Errorf("hook needs a name"))
		}
		if t := fs.Lookup("tool").Value.String(); t != "" {
			_ = os.Setenv("ANY_AGENTS_TOOL", t)
		}
		tool := currentTool()
		switch pos[0] {
		case "session-start":
			hookSessionStart(tool)
		case "session-end":
			hookSessionEnd(tool)
		case "post-tool":
			hookPostTool(tool)
		case "prompt":
			hookPrompt(tool)
		default:
			fatal(fmt.Errorf("unknown hook %q", pos[0]))
		}
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}
}

func cmdInstall(args []string) {
	fs, _ := parseArgs("install", args, func(fs *flag.FlagSet) {
		fs.Bool("create-space", false, "create the private space when this account does not have it yet")
	})
	c := NewClient()
	if err := c.Health(); err != nil {
		fatal(fmt.Errorf("any server not reachable at %s — start it with `any run`", c.base))
	}
	st, err := loadState()
	if err != nil {
		fatal(err)
	}
	for _, sp := range st.Spaces {
		sp.SetupVersion = 0 // force a full ensure everywhere, Collab included
	}
	if err := ensureSetup(c, st, fs.Lookup("create-space").Value.String() == "true"); err != nil {
		fatal(err)
	}
	bin, err := installBinary()
	if err != nil {
		fatal(err)
	}
	tool := currentTool()
	if err := tool.InstallHooks(bin); err != nil {
		fatal(err)
	}
	sp := st.private()
	if err := renderStatusPage(c, sp); err != nil {
		fmt.Fprintf(os.Stderr, "any-agents: status page: %v\n", err)
	}
	fmt.Printf("installed\n  space    %s (%s)\n  app      %s\n  machine  %s\n  binary   %s\n  hooks    merged into %s\n",
		sp.Name, sp.Id, globalLink(sp.Id, sp.owner("app")), globalLink(sp.Id, sp.MachineObjectId), bin, filepath.Join(tool.Home(), "settings.json"))
	if !sp.has("collab") {
		fmt.Println("  collabs  off (the server reserves the chat module)")
	}
}

// installBinary copies the running executable to a stable location so
// hooks keep working when the build tree moves.
func installBinary() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	home, _ := os.UserHomeDir()
	dst := filepath.Join(home, ".local", "bin", "any-agents")
	if h := os.Getenv("ANY_AGENTS_HOME"); h != "" {
		dst = filepath.Join(h, "bin", "any-agents")
	}
	if self == dst {
		return dst, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	data, err := os.ReadFile(self)
	if err != nil {
		return "", err
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return "", err
	}
	return dst, os.Rename(tmp, dst)
}

func cmdStatus() {
	c := NewClient()
	if err := c.Health(); err != nil {
		fatal(fmt.Errorf("any server not reachable at %s", c.base))
	}
	st, err := loadState()
	if err != nil {
		fatal(err)
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	fmt.Println(string(b))
	if sp := st.private(); sp == nil || !sp.ready() {
		fmt.Println("not installed — run `any-agents install`")
	}
}

func cmdSpace(args []string) {
	if len(args) == 0 {
		fatal(fmt.Errorf("usage: space list|add <name|id>|create <name>|remove <name|id>"))
	}
	c := NewClient()
	st := mustSetup(c)
	switch args[0] {
	case "list":
		for _, sp := range append([]*Space{st.private()}, st.collabs()...) {
			collab := "collabs on"
			if !sp.has("collab") {
				collab = "collabs off"
			}
			fmt.Printf("%s  %-8s %-24s %s\n", sp.Id, sp.Kind, pad(sp.Name, 24), collab)
		}
	case "add":
		if len(args) < 2 {
			fatal(fmt.Errorf("space add <name|id>"))
		}
		sp, err := addCollabSpace(c, st, args[1])
		if err != nil {
			fatal(err)
		}
		fmt.Printf("%s  %s set up as a collab space\n", sp.Id, sp.Name)
	case "create":
		if len(args) < 2 {
			fatal(fmt.Errorf("space create <name>"))
		}
		id, err := c.CreateSpace(args[1], "Shared context of a team's coding agents (managed by any-agents)")
		if err != nil {
			fatal(err)
		}
		st.addSpace(id, args[1], kindCollab).Created = true
		sp, err := addCollabSpace(c, st, id)
		if err != nil {
			fatal(err)
		}
		fmt.Printf("%s  %s created; invite members with `any invite create %s`\n", sp.Id, sp.Name, sp.Id)
	case "remove":
		if len(args) < 2 {
			fatal(fmt.Errorf("space remove <name|id>"))
		}
		sp := spaceOf(st, args[1])
		if sp.Kind == kindPrivate {
			fatal(fmt.Errorf("the private space cannot be removed"))
		}
		delete(st.Spaces, sp.Id)
		if err := st.save(); err != nil {
			fatal(err)
		}
		fmt.Printf("%s forgotten on this machine (the space itself is untouched)\n", sp.Name)
	default:
		fatal(fmt.Errorf("unknown space command %q", args[0]))
	}
}

func projectDirArg(pos []string) string {
	if len(pos) > 0 {
		return pos[0]
	}
	cwd, _ := os.Getwd()
	return cwd
}

func cmdPull(args []string) {
	fs, pos := parseArgs("pull", args, func(fs *flag.FlagSet) {
		fs.Bool("allow-deletions", false, "honor a remote that lost most of its records")
	})
	c := NewClient()
	st := mustSetup(c)
	sp := st.private()
	shared := pullSharedMemory(c, st, projectDirArg(pos))
	n, err := pullMemory(c, sp, projectDirArg(pos), fs.Lookup("allow-deletions").Value.String() == "true", shared)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("%d memories in scope, %d shared set(s)\n", n, len(shared))
	if a, err := pullConfig(c, sp); err != nil {
		fatal(err)
	} else if a > 0 {
		fmt.Printf("%d config file(s) updated\n", a)
	}
}

func cmdPush(args []string) {
	_, pos := parseArgs("push", args, func(fs *flag.FlagSet) {})
	c := NewClient()
	st := mustSetup(c)
	sp := st.private()
	if err := pushMemory(c, sp, projectDirArg(pos)); err != nil {
		fatal(err)
	}
	if err := pushSharedMemory(c, st, projectDirArg(pos)); err != nil {
		fatal(err)
	}
	if err := pushConfig(c, sp); err != nil {
		fatal(err)
	}
	fmt.Println("pushed")
}

func cmdSearch(args []string) {
	fs, pos := parseArgs("search", args, func(fs *flag.FlagSet) {
		fs.Int("n", 8, "max hits")
		fs.Bool("json", false, "raw JSON hits")
		spaceFlag(fs)
	})
	if len(pos) == 0 {
		fatal(fmt.Errorf("search <query>"))
	}
	c := NewClient()
	st := mustSetup(c)
	sp := spaceOf(st, fs.Lookup("space").Value.String())
	query := ""
	for i, p := range pos {
		if i > 0 {
			query += " "
		}
		query += p
	}
	res, err := c.Search(sp.Id, query, atoi(fs.Lookup("n").Value.String()))
	if err != nil {
		fatal(err)
	}
	bumpStat("searches", 1)
	if fs.Lookup("json").Value.String() == "true" {
		b, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(b))
		return
	}
	for _, h := range res.Hits {
		if h.Scope == "props" {
			continue // FTS-only property noise; names, bodies and records carry the signal
		}
		fmt.Printf("%.3f  %s\n      %s\n", h.Score, hitLabel(c, sp, h), trunc(oneLine(h.Data), 300))
	}
	fmt.Fprintf(os.Stderr, "(%s, vector %s)\n", res.Mode, res.VectorStatus)
}

func oneLine(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' {
			r = ' '
		}
		out = append(out, r)
	}
	return string(out)
}
