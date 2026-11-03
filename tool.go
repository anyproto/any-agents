package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Tool is the coding-agent harness this bridge serves on a machine:
// where its global config and per-project memory live, how it names a
// session, and how its hooks are installed and parsed. One tool per
// bridge invocation ($ANY_AGENTS_TOOL, default claude).
type Tool interface {
	Name() string  // short id, the choice value on sessions: claude
	Label() string // display name used in chat agent names: Claude
	Version() string
	Home() string             // the tool's config dir
	ConfigRoots() []string    // files/dirs under Home mirrored as config records
	ConfigSkipDirs() []string // dirs under Home the tool syncs itself
	MemoryDir(projectDir string) string
	SessionEnv() string // env var carrying the session id inside a session
	HookFile() string
	ParseHookInput(r io.Reader) *hookInput
	EmitContext(event, ctx string)
	EmitBlock(reason string) // Stop hook: keep the agent working, with the reason
}

var tools = map[string]func() Tool{
	"claude": func() Tool { return &claudeTool{} },
	"codex":  func() Tool { return &codexTool{} },
}

// An explicit selection wins. Commands run inside Codex pick its thread,
// even if another harness has opened a session more recently.
func currentTool() Tool {
	name := os.Getenv("ANY_AGENTS_TOOL")
	if name == "" && os.Getenv("CODEX_THREAD_ID") != "" && os.Getenv("CLAUDE_CODE_SESSION_ID") == "" {
		name = "codex"
	}
	if name == "" {
		name = "claude"
	}
	mk := tools[name]
	if mk == nil {
		fatal(fmt.Errorf("unknown tool %q (known: claude, codex)", name))
	}
	return mk()
}

// hookInput is the common shape of a hook event: every tool maps its own
// payload onto it.
type hookInput struct {
	SessionId      string
	TranscriptPath string
	Cwd            string
	Source         string
	ToolName       string
	FilePath       string // file-tool edits
	Command        string // shell-tool commands
	Prompt         string
	StopHookActive bool // a Stop hook already held this turn once
}

// --- Claude Code ---

type claudeTool struct{ commandHooks }

func (t *claudeTool) Name() string  { return "claude" }
func (t *claudeTool) Label() string { return "Claude" }

// Home is $CLAUDE_CONFIG_DIR (Claude Code's own override), else ~/.claude.
func (t *claudeTool) Home() string {
	if h := os.Getenv("CLAUDE_CONFIG_DIR"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

func (t *claudeTool) ConfigRoots() []string {
	return []string{"CLAUDE.md", "keybindings.json", "skills"}
}

// skills/synced is a bundle Claude Code syncs itself on every machine.
func (t *claudeTool) ConfigSkipDirs() []string { return []string{"skills/synced"} }

// MemoryDir is Claude Code's auto-memory dir for a project path (the
// harness keys it by the main checkout's path with non-alphanumerics folded
// to '-', so a linked worktree or a subdirectory uses the repo's one dir).
func (t *claudeTool) MemoryDir(projectDir string) string {
	var b strings.Builder
	for _, r := range resolveMemoryRoot(projectDir) {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	return filepath.Join(t.Home(), "projects", b.String(), "memory")
}

func (t *claudeTool) HookFile() string { return filepath.Join(t.Home(), "settings.json") }

func (t *claudeTool) SessionEnv() string { return "CLAUDE_CODE_SESSION_ID" }

func (t *claudeTool) Version() string { return toolVersion("claude") }

func toolVersion(name string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, "--version").Output()
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out))
	if len(fields) > 1 && fields[0] == "codex-cli" {
		return fields[1]
	}
	if len(fields) > 0 {
		return fields[0]
	}
	return ""
}

// Both harnesses use the same command hook input and output envelopes.
type commandHooks struct{}

func (commandHooks) ParseHookInput(r io.Reader) *hookInput {
	var in struct {
		SessionId      string `json:"session_id"`
		TranscriptPath string `json:"transcript_path"`
		Cwd            string `json:"cwd"`
		Source         string `json:"source"`
		ToolName       string `json:"tool_name"`
		ToolInput      struct {
			FilePath string `json:"file_path"`
			Command  string `json:"command"`
		} `json:"tool_input"`
		Prompt         string `json:"prompt"`
		StopHookActive bool   `json:"stop_hook_active"`
	}
	b, _ := io.ReadAll(r)
	_ = json.Unmarshal(b, &in)
	h := &hookInput{SessionId: in.SessionId, TranscriptPath: in.TranscriptPath, Cwd: in.Cwd, Source: in.Source,
		ToolName: in.ToolName, FilePath: in.ToolInput.FilePath, Command: in.ToolInput.Command, Prompt: in.Prompt, StopHookActive: in.StopHookActive}
	if h.Cwd == "" {
		h.Cwd, _ = os.Getwd()
	}
	return h
}

func (commandHooks) EmitContext(event, ctx string) {
	out := map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": event, "additionalContext": ctx}}
	_ = json.NewEncoder(os.Stdout).Encode(out)
}

func (commandHooks) EmitBlock(reason string) {
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"decision": "block", "reason": reason})
}
