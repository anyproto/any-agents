package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexInstallMergesOnlyItsHooks(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	tool := &codexTool{}
	const original = `{"description":"keep","hooks":{"SessionStart":[
		{"matcher":"startup","hooks":[{"type":"command","command":"echo any-agents"}]},
		{"hooks":[{"type":"command","command":"/old/bin/any-agents hook --tool codex session-start"},{"type":"command","command":"other-hook"}]}
	]}}`
	if err := os.WriteFile(tool.HookFile(), []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	const bin = "/home/Pat's Tools/bin/any-agents"
	install := func() string {
		t.Helper()
		if err := installHooks(tool, bin); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(tool.HookFile())
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	got := install()
	if install() != got {
		t.Fatal("reinstall changed the hook definitions")
	}
	backup, err := os.ReadFile(tool.HookFile() + ".bak-any-agents")
	if err != nil || string(backup) != original {
		t.Fatalf("backup of original settings: %v", err)
	}
	var file struct {
		Description string
		Hooks       map[string][]struct {
			Matcher string
			Hooks   []struct {
				Command string
				Timeout int
			}
		}
	}
	if err := json.Unmarshal([]byte(got), &file); err != nil {
		t.Fatal(err)
	}
	start := file.Hooks["SessionStart"]
	if file.Description != "keep" || len(file.Hooks) != 5 || len(start) != 3 ||
		start[0].Matcher != "startup" || start[0].Hooks[0].Command != "echo any-agents" || start[1].Hooks[0].Command != "other-hook" ||
		start[2].Hooks[0].Command != `'/home/Pat'\''s Tools/bin/any-agents' hook --tool codex session-start` {
		t.Fatalf("unexpected merged hooks: %s", got)
	}
	if file.Hooks["SessionEnd"][0].Hooks[0].Timeout != 3 || file.Hooks["PostToolUse"][0].Matcher != "^(Bash|apply_patch)$" {
		t.Fatal("unexpected Codex timeout or matcher")
	}
	for _, invalid := range []string{"{", "null", `{"hooks":[]}`, `{"hooks":{"Stop":{}}}`} {
		if err := os.WriteFile(tool.HookFile(), []byte(invalid), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := installHooks(tool, bin); err == nil {
			t.Fatalf("accepted invalid settings: %s", invalid)
		}
		b, _ := os.ReadFile(tool.HookFile())
		if string(b) != invalid {
			t.Fatal("invalid settings overwritten")
		}
	}
}

func TestCodexConfigScope(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("ANY_AGENTS_TOOL", "codex")
	for _, path := range []string{"AGENTS.md", "auth.json", "config.toml", "hooks.json", "skills/.system/example/SKILL.md", "skills/personal/SKILL.md"} {
		file := filepath.Join(home, path)
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(path), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(home, "auth.json"), filepath.Join(home, "skills/personal/link")); err != nil {
		t.Fatal(err)
	}
	bundle, err := scanConfigBundle()
	if err != nil || len(bundle) != 2 || bundle["codex:AGENTS.md"] == nil || bundle["codex:skills:personal:SKILL.md"] == nil {
		t.Fatalf("unexpected scanned config: %v (%v)", bundle, err)
	}
	if configPathAllowed(currentTool(), "auth.json") || configPathAllowed(currentTool(), "skills/../auth.json") {
		t.Fatal("accepted out-of-scope config")
	}
	if configStateName() == "config" {
		t.Fatal("used Claude's existing config fingerprints")
	}
}

func TestCodexThreadSelection(t *testing.T) {
	t.Setenv("ANY_AGENTS_TOOL", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("CODEX_THREAD_ID", "codex-thread")
	t.Setenv("ANY_AGENTS_HOME", t.TempDir())
	if tool := currentTool(); tool.Name() != "codex" || tool.Label() != "Codex" {
		t.Fatal(tool)
	}
	if sid, exact := resolveSession(&State{LastSessionId: "unrelated"}, ""); !exact || sid != "codex-thread" {
		t.Fatal(sid)
	}
	in := currentTool().ParseHookInput(strings.NewReader(`{"tool_name":"apply_patch","tool_input":{"command":"*** Update File: /some/.codex/AGENTS.md"},"transcript_path":null}`))
	if in.ToolName != "apply_patch" || !strings.Contains(in.Command, "AGENTS.md") || in.TranscriptPath != "" {
		t.Fatal(in)
	}
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("ANY_AGENTS_TOOL", "codex")
	if sid, _ := resolveSession(&State{LastSessionId: "claude-thread"}, ""); sid != "" {
		t.Fatal("Codex picked Claude session", sid)
	}
	t.Setenv("ANY_AGENTS_TOOL", "claude")
	if currentTool().Name() != "claude" || configStateName() != "config" {
		t.Fatal("explicit Claude selection or legacy config baseline lost")
	}
}

func TestHookInstallKeepsSettingsSymlink(t *testing.T) {
	tool := &codexTool{}
	t.Setenv("CODEX_HOME", t.TempDir())
	target := filepath.Join(t.TempDir(), "dotfiles.json")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, tool.HookFile()); err != nil {
		t.Fatal(err)
	}
	if err := installHooks(tool, "/bin/any-agents"); err != nil {
		t.Fatal(err)
	}
	if link, err := os.Readlink(tool.HookFile()); err != nil || link != target {
		t.Fatalf("lost symlink: %q %v", link, err)
	}
	if b, err := os.ReadFile(target); err != nil || !strings.Contains(string(b), "session-start") {
		t.Fatalf("target missing hooks: %v", err)
	}
}
