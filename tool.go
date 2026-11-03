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
	InstallHooks(bin string) error
	ParseHookInput(r io.Reader) *hookInput
	EmitContext(event, ctx string)
}

var tools = map[string]func() Tool{"claude": func() Tool { return &claudeTool{} }}

// currentTool picks the harness: --tool on hooks, else $ANY_AGENTS_TOOL,
// else claude.
func currentTool() Tool {
	name := os.Getenv("ANY_AGENTS_TOOL")
	if name == "" {
		name = "claude"
	}
	mk := tools[name]
	if mk == nil {
		fatal(fmt.Errorf("unknown tool %q (known: claude)", name))
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
}

// --- Claude Code ---

type claudeTool struct{ version string }

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
// harness keys it by the path with non-alphanumerics folded to '-').
func (t *claudeTool) MemoryDir(projectDir string) string {
	var b strings.Builder
	for _, r := range projectDir {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	return filepath.Join(t.Home(), "projects", b.String(), "memory")
}

func (t *claudeTool) SessionEnv() string { return "CLAUDE_CODE_SESSION_ID" }

// Version runs `claude --version` once (bounded); empty when absent.
func (t *claudeTool) Version() string {
	if t.version != "" {
		return t.version
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "claude", "--version").Output()
	if err != nil {
		return ""
	}
	if f := strings.Fields(string(out)); len(f) > 0 {
		t.version = f[0]
	}
	return t.version
}

func (t *claudeTool) ParseHookInput(r io.Reader) *hookInput {
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
		Prompt string `json:"prompt"`
	}
	b, _ := io.ReadAll(r)
	_ = json.Unmarshal(b, &in)
	h := &hookInput{SessionId: in.SessionId, TranscriptPath: in.TranscriptPath, Cwd: in.Cwd, Source: in.Source,
		ToolName: in.ToolName, FilePath: in.ToolInput.FilePath, Command: in.ToolInput.Command, Prompt: in.Prompt}
	if h.Cwd == "" {
		h.Cwd, _ = os.Getwd()
	}
	return h
}

func (t *claudeTool) EmitContext(event, ctx string) {
	out := map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": event, "additionalContext": ctx}}
	_ = json.NewEncoder(os.Stdout).Encode(out)
}

// InstallHooks merges the bridge's hooks into settings.json, preserving
// everything else. Idempotent: an existing any-agents entry is replaced.
func (t *claudeTool) InstallHooks(bin string) error {
	settingsPath := filepath.Join(t.Home(), "settings.json")
	raw := map[string]any{}
	if b, err := os.ReadFile(settingsPath); err == nil {
		if err := json.Unmarshal(b, &raw); err != nil {
			return fmt.Errorf("parse %s: %w", settingsPath, err)
		}
		_ = os.WriteFile(settingsPath+".bak-any-agents", b, 0o644)
	}
	hooks, _ := raw["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	set := func(event, matcher, cmd string, timeout int) {
		hook := map[string]any{"type": "command", "command": cmd}
		if timeout > 0 {
			hook["timeout"] = timeout
		}
		entry := map[string]any{"hooks": []any{hook}}
		if matcher != "" {
			entry["matcher"] = matcher
		}
		arr, _ := hooks[event].([]any)
		kept := make([]any, 0, len(arr))
		for _, e := range arr { // drop stale entries of ours
			s := fmt.Sprint(e)
			if !strings.Contains(s, "any-agents") && !strings.Contains(s, "any-claude") {
				kept = append(kept, e)
			}
		}
		hooks[event] = append(kept, entry)
	}
	cmd := bin + " hook --tool claude "
	set("SessionStart", "", cmd+"session-start", 60)
	set("SessionEnd", "", cmd+"session-end", 120)
	set("PostToolUse", "Write|Edit|MultiEdit|NotebookEdit|Bash", cmd+"post-tool", 0)
	set("UserPromptSubmit", "", cmd+"prompt", 0)
	raw["hooks"] = hooks
	b, _ := json.MarshalIndent(raw, "", "  ")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(settingsPath, append(b, '\n'), 0o644)
}
