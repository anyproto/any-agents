package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "protocol.file.allow=always"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func newRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "init", "-q")
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "init")
}

func TestResolveMemoryRoot(t *testing.T) {
	base := canonicalTempDir(t)
	repo := filepath.Join(base, "repo")
	newRepo(t, repo)
	sub := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(repo, ".claude", "worktrees", "wt")
	gitIn(t, repo, "worktree", "add", "-q", "-b", "wt", worktree)
	worktreeSub := filepath.Join(worktree, "c")
	if err := os.MkdirAll(worktreeSub, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "plain")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join(base, "lib")
	newRepo(t, lib)
	gitIn(t, repo, "submodule", "add", "-q", lib, "vendor/lib")
	submodule := filepath.Join(repo, "vendor", "lib")

	for _, c := range []struct{ name, dir, want string }{
		{"main checkout", repo, repo},
		{"subdirectory", sub, repo},
		{"linked worktree", worktree, repo},
		{"worktree subdirectory", worktreeSub, repo},
		{"submodule", submodule, submodule},
		{"outside git", outside, outside},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveMemoryRootUncached(c.dir); got != c.want {
				t.Fatalf("got %s, want %s", got, c.want)
			}
		})
	}
}

func TestClaudeMemoryDirSharedAcrossWorktrees(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	base := canonicalTempDir(t)
	repo := filepath.Join(base, "repo")
	newRepo(t, repo)
	worktree := filepath.Join(base, "wt")
	gitIn(t, repo, "worktree", "add", "-q", "-b", "wt", worktree)

	tool := &claudeTool{}
	if a, b := tool.MemoryDir(repo), tool.MemoryDir(worktree); a != b {
		t.Fatalf("main checkout %s, worktree %s", a, b)
	}
}
