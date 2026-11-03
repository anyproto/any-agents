package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Match the command itself. Mentions of the binary in another command,
// a status message, or another handler in the group belong to the user.
var bridgeCommand = regexp.MustCompile(`^(?:/(?:[a-zA-Z0-9_./-]+/)?)?(any-agents|any-claude) hook (?:--tool (claude|codex) )?(session-start|session-end|post-tool|prompt|stop)$`)
var plainShellPath = regexp.MustCompile(`^[a-zA-Z0-9_./-]+$`)

func shellPath(path string) string {
	if plainShellPath.MatchString(path) {
		return path
	}
	return "'" + strings.ReplaceAll(path, "'", "'\\''") + "'"
}

type hookGroup map[string]json.RawMessage

func installHooks(tool Tool, bin string) error {
	path := tool.HookFile()
	target := path
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		target, err = filepath.EvalSymlinks(path)
		if err != nil {
			return err
		}
	}
	raw := map[string]json.RawMessage{}
	before, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(before, &raw); err != nil || raw == nil {
			return fmt.Errorf("invalid hook settings in %s", path)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	hooks := map[string]json.RawMessage{}
	if data, ok := raw["hooks"]; ok {
		if err := json.Unmarshal(data, &hooks); err != nil || hooks == nil {
			return fmt.Errorf("invalid hooks object in %s", path)
		}
	}

	matcher, endTimeout := "Write|Edit|MultiEdit|NotebookEdit|Bash", 120
	if tool.Name() == "codex" {
		matcher, endTimeout = "^(Bash|apply_patch)$", 3
	}
	for _, spec := range []struct {
		event, action, matcher string
		timeout                int
	}{
		{"SessionStart", "session-start", "", 60},
		{"SessionEnd", "session-end", "", endTimeout},
		{"PostToolUse", "post-tool", matcher, 0},
		{"UserPromptSubmit", "prompt", "", 0},
		{"Stop", "stop", "", 30},
	} {
		cmd := shellPath(bin) + " hook --tool " + tool.Name() + " " + spec.action
		h := map[string]any{"type": "command", "command": cmd}
		if spec.timeout > 0 {
			h["timeout"] = spec.timeout
		}
		desired := hookGroup{}
		desired["hooks"], _ = json.Marshal([]any{h})
		if spec.matcher != "" {
			desired["matcher"], _ = json.Marshal(spec.matcher)
		}
		groups, err := mergeHookGroups(hooks[spec.event], desired, cmd, tool.Name())
		if err != nil {
			return fmt.Errorf("%s in %s: %w", spec.event, path, err)
		}
		hooks[spec.event], _ = json.Marshal(groups)
	}
	raw["hooks"], _ = json.Marshal(hooks)
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	if bytes.Equal(out, before) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if before != nil {
		if err := writePrivateFile(path+".bak-any-agents", before); err != nil {
			return err
		}
	}
	return writePrivateFile(target, out)
}

func mergeHookGroups(data json.RawMessage, desired hookGroup, cmd, tool string) ([]hookGroup, error) {
	var groups []hookGroup
	if data != nil {
		if err := json.Unmarshal(data, &groups); err != nil {
			return nil, err
		}
	}
	var merged []hookGroup
	placed := false
	for _, g := range groups {
		var handlers []json.RawMessage
		if err := json.Unmarshal(g["hooks"], &handlers); err != nil || handlers == nil {
			return nil, fmt.Errorf("invalid hook group")
		}
		var kept []json.RawMessage
		for _, h := range handlers {
			var definition struct{ Type, Command string }
			if err := json.Unmarshal(h, &definition); err != nil {
				return nil, err
			}
			m := bridgeCommand.FindStringSubmatch(definition.Command)
			ours := definition.Command == cmd || m != nil && (m[2] == tool || m[2] == "" && tool == "claude")
			if definition.Type != "command" || !ours {
				kept = append(kept, h)
			}
		}
		switch {
		case len(kept) == len(handlers):
			merged = append(merged, g)
		case len(kept) > 0:
			g["hooks"], _ = json.Marshal(kept)
			merged = append(merged, g)
		case !placed:
			// Codex tracks hook trust by index. Reinstall in the same slot.
			merged = append(merged, desired)
			placed = true
		}
	}
	if !placed {
		merged = append(merged, desired)
	}
	return merged, nil
}
