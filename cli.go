package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "any-agents: %v\n", err)
	os.Exit(1)
}

// parseArgs parses flags that may appear before or after positionals.
func parseArgs(name string, args []string, define func(fs *flag.FlagSet)) (*flag.FlagSet, []string) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	define(fs)
	var pos []string
	for len(args) > 0 {
		if strings.HasPrefix(args[0], "-") && args[0] != "-" {
			_ = fs.Parse(args)
			args = fs.Args()
			continue
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
	return fs, pos
}

// spaceFlag declares --space on a command (empty = the private space).
func spaceFlag(fs *flag.FlagSet) {
	fs.String("space", "", "space name, id or link (default: the private space)")
}

// readSource reads a file path or "-" (stdin).
func readSource(src string) ([]byte, error) {
	if src == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(src)
}

// textArg resolves a <text|file|-> argument: "-" reads stdin, a single
// argument naming an existing file reads that file, anything else is the
// text itself.
func textArg(args []string) (string, error) {
	if len(args) == 1 {
		if args[0] == "-" {
			b, err := readSource("-")
			return string(b), err
		}
		if fi, err := os.Stat(args[0]); err == nil && fi.Mode().IsRegular() {
			b, err := os.ReadFile(args[0])
			return string(b), err
		}
	}
	return strings.Join(args, " "), nil
}

var mdHeading = regexp.MustCompile(`(?m)^#\s+(.+)$`)

// titleOf picks a title: the flag, else the first H1, else the fallback.
func titleOf(flag, body, fallback string) string {
	if flag != "" {
		return flag
	}
	if m := mdHeading.FindStringSubmatch(body); m != nil {
		return strings.TrimSpace(m[1])
	}
	return fallback
}

// mustSetup loads state and resolves the private space (adopt only).
func mustSetup(c *Client) *State {
	st, err := loadState()
	if err != nil {
		fatal(err)
	}
	if err := ensureSetup(c, st, false); err != nil {
		fatal(err)
	}
	return st
}

// spaceOf resolves a --space value or dies.
func spaceOf(st *State, flag string) *Space {
	sp, err := st.pick(flag)
	if err != nil {
		fatal(err)
	}
	// A collab space set up by an older binary catches up on first use.
	if sp.Kind == kindCollab && sp.SetupVersion != setupVersion {
		if err := ensureSpaceSetup(NewClient(), sp); err != nil {
			fmt.Fprintf(os.Stderr, "any-agents: %s: setup: %v\n", sp.Name, err)
		}
	}
	return sp
}

// resolveSession picks the session id: flag → the tool's session env var
// → the last seen on this machine. exact is false for the last seen,
// which may belong to another agent session.
func resolveSession(st *State, flag string) (id string, exact bool) {
	if flag != "" {
		return flag, true
	}
	if v := os.Getenv(currentTool().SessionEnv()); v != "" {
		return v, true
	}
	return st.LastSessionId, false
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func pad(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s + strings.Repeat(" ", n-len(r))
}
