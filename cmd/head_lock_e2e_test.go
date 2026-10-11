package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/treehouse/v3/internal/process"
)

func headLockPath(t *testing.T, slot string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--path-format=absolute", "--git-path", "HEAD")
	cmd.Dir = slot
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(strings.TrimSpace(string(out))) + ".lock"
}

func TestHeadLockStatusReportsAnonymousLocks(t *testing.T) {
	for _, leased := range []bool{false, true} {
		t.Run(fmt.Sprintf("leased=%t", leased), func(t *testing.T) {
			repo, home := setupTestRepo(t)
			env := []string{"TREEHOUSE_ROOT=" + filepath.Join(t.TempDir(), "root")}
			if err := os.WriteFile(filepath.Join(repo, "treehouse.toml"), []byte("max_trees = 1\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			out, stderr, code := runTreehouse(t, repo, home, env, "get", "--lease", "--no-fetch")
			if code != 0 {
				t.Fatalf("get: %s", stderr)
			}
			slot := strings.TrimSpace(out)
			if !leased {
				if _, stderr, code := runTreehouse(t, repo, home, env, "return", slot); code != 0 {
					t.Fatalf("return: %s", stderr)
				}
			}
			lock := headLockPath(t, slot)
			if err := os.WriteFile(lock, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			out, stderr, code = runTreehouse(t, repo, home, env, "status")
			if code != 0 || !strings.Contains(out, "HEAD.lock") || !strings.Contains(out, "manual") {
				t.Fatalf("status must explain the anonymous lock and manual inspection: %s (%s)", out, stderr)
			}
			out, stderr, code = runTreehouse(t, repo, home, env, "status", "--json")
			var statuses []struct {
				Status    string `json:"status"`
				LockError string `json:"lock_error"`
			}
			if err := json.Unmarshal([]byte(out), &statuses); err != nil || code != 0 || len(statuses) != 1 {
				t.Fatalf("status JSON: %s (%s, %v)", out, stderr, err)
			}
			want := "locked"
			if leased {
				want = "leased"
			}
			if statuses[0].Status != want || !strings.Contains(statuses[0].LockError, lock) {
				t.Fatalf("status must retain the lease and expose the exact lock: %+v", statuses[0])
			}
			if !leased {
				inside, _, code := runTreehouseFromDir(t, repo, slot, home, env, "status", "--json")
				if code != 0 || !strings.Contains(inside, `"status":"locked"`) {
					t.Fatalf("standing in a locked slot must not hide its obstruction: %s", inside)
				}
				_, stderr, code = runTreehouse(t, repo, home, env, "get", "--lease", "--no-fetch")
				if code == 0 || !strings.Contains(stderr, "HEAD.lock") {
					t.Fatalf("full-pool get must explain the lock: code=%d %s", code, stderr)
				}
			}
			for _, args := range [][]string{{"return", slot}, {"return", "--force", slot}} {
				_, stderr, code = runTreehouse(t, repo, home, env, args...)
				if code == 0 || !strings.Contains(stderr, "manual") {
					t.Fatalf("anonymous lock must require manual inspection: code=%d %s", code, stderr)
				}
				if data, err := os.ReadFile(lock); err != nil || len(data) != 0 {
					t.Fatalf("anonymous lock changed: %q (%v)", data, err)
				}
			}
		})
	}
}

func TestHeadLockKilledResetIsRecoverable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Git delay wrapper is a POSIX shell script")
	}
	for _, action := range []string{"return", "get"} {
		for _, written := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/written=%t", action, written), func(t *testing.T) {
				repo, home := setupTestRepo(t)
				root := filepath.Join(t.TempDir(), "root")
				env := []string{"TREEHOUSE_ROOT=" + root}
				if err := os.WriteFile(filepath.Join(repo, "treehouse.toml"), []byte("max_trees = 1\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				out, stderr, code := runTreehouse(t, repo, home, env, "get", "--lease", "--no-fetch")
				if code != 0 {
					t.Fatalf("initial get: %s", stderr)
				}
				slot := strings.TrimSpace(out)
				args := []string{"return", slot}
				if action == "get" {
					if _, stderr, code := runTreehouse(t, repo, home, env, args...); code != 0 {
						t.Fatalf("initial return: %s", stderr)
					}
					args = []string{"get", "--lease", "--no-fetch"}
				}
				lock := headLockPath(t, slot)
				realGit, err := exec.LookPath("git")
				if err != nil {
					t.Fatal(err)
				}
				shim := t.TempDir()
				ready, release := filepath.Join(shim, "ready"), filepath.Join(shim, "release")
				script := "#!/bin/sh\nif [ \"$1\" = read-tree ]; then\n  : > \"$TREEHOUSE_TEST_READY\"\n  while [ ! -f \"$TREEHOUSE_TEST_RELEASE\" ]; do sleep 0.02; done\nfi\nexec \"$TREEHOUSE_TEST_GIT\" \"$@\"\n"
				if err := os.WriteFile(filepath.Join(shim, "git"), []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command(treehouseBin, args...)
				cmd.Dir = repo
				cmd.Env = buildEnv(home, append(env, "PATH="+shim+string(os.PathListSeparator)+os.Getenv("PATH"),
					"TREEHOUSE_TEST_READY="+ready, "TREEHOUSE_TEST_RELEASE="+release, "TREEHOUSE_TEST_GIT="+realGit)...)
				var diagnostics bytes.Buffer
				cmd.Stdout, cmd.Stderr = &diagnostics, &diagnostics
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					_ = os.WriteFile(release, nil, 0o644)
					_ = cmd.Process.Kill()
				})
				deadline := time.Now().Add(10 * time.Second)
				for {
					if _, err := os.Stat(ready); err == nil {
						break
					}
					if time.Now().After(deadline) {
						_ = cmd.Process.Kill()
						_ = cmd.Wait()
						t.Fatalf("reset never reached read-tree: %s", diagnostics.String())
					}
					time.Sleep(20 * time.Millisecond)
				}
				if err := cmd.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				_ = cmd.Wait()
				if action == "return" {
					_, stderr, code := runTreehouse(t, repo, home, env, "return", slot)
					if code == 0 {
						t.Fatalf("return recovered while the killed reset's Git child was still running: %s", stderr)
					}
					if _, err := os.Stat(lock); err != nil {
						t.Fatalf("return removed the lock while the Git child was live: %v", err)
					}
				}
				if err := os.WriteFile(release, nil, 0o644); err != nil {
					t.Fatal(err)
				}
				for {
					procs, err := process.FindProcessesInWorktree(slot)
					if err != nil {
						t.Fatal(err)
					}
					if len(procs) == 0 {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("Git child did not exit: %v", procs)
					}
					time.Sleep(20 * time.Millisecond)
				}
				if _, err := os.Stat(lock); err != nil {
					t.Fatalf("killed reset did not leave HEAD.lock: %v", err)
				}
				if written {
					head, err := exec.Command("git", "-C", slot, "rev-parse", "HEAD").Output()
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(lock, head, 0o644); err != nil {
						t.Fatal(err)
					}
				}
				out, stderr, code = runTreehouse(t, repo, home, env, args...)
				if code != 0 {
					t.Fatalf("%s must recover its killed reset: %s %s", action, out, stderr)
				}
				if action == "get" && strings.TrimSpace(out) != slot {
					t.Fatalf("get must reuse the only slot: %q, want %q", out, slot)
				}
				if _, err := os.Stat(lock); !os.IsNotExist(err) {
					t.Fatalf("successful recovery left HEAD.lock: %v", err)
				}
			})
		}
	}
}
