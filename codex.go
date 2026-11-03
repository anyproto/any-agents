package main

import (
	"os"
	"path/filepath"
)

type codexTool struct{ commandHooks }

func (*codexTool) Name() string       { return "codex" }
func (*codexTool) Label() string      { return "Codex" }
func (*codexTool) Version() string    { return toolVersion("codex") }
func (*codexTool) SessionEnv() string { return "CODEX_THREAD_ID" }

func (*codexTool) Home() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex")
}

func (t *codexTool) HookFile() string { return filepath.Join(t.Home(), "hooks.json") }

func (*codexTool) ConfigRoots() []string {
	return []string{"AGENTS.md", "AGENTS.override.md", "skills"}
}

func (*codexTool) ConfigSkipDirs() []string { return []string{"skills/.system"} }

// Keep the bridge's per-project Markdown out of Codex's native memory store.
// The prefix also keeps its local fingerprints apart from Claude's folded paths.
func (*codexTool) MemoryDir(projectDir string) string {
	return filepath.Join(stateDir(), "projects", "codex-"+resolveScope(projectDir), "memory")
}
