package tuie2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Gaurav-Gosain/tuios/internal/testutil"
	"github.com/Gaurav-Gosain/tuitest"
)

// driveWorktreeKillNext exercises the actual sidebar or quit-menu action,
// cancels its confirmation, then chooses whether the worktree goes too. The
// client must stay running on plain, and only the original session may die.
func driveWorktreeKillNext(t *testing.T, base, session, branch, route string, remove bool) {
	t.Helper()
	term := startIn(t, base, startOpts{args: []string{"attach", session}})
	if err := term.WaitFor(func(s tuitest.Screen) bool { return countWindows(s) == 1 }, bootTimeout); err != nil {
		t.Fatalf("attach worktree: %v\n%s", err, term.Snapshot())
	}
	windowManagementMode(t, term)
	toggleSidebarViaPalette(t, term)
	waitForAll(t, term, uiTimeout, "worktree and destination sessions", sidebarHeader, branch, "plain")

	open := func() {
		t.Helper()
		if route == "sidebar" {
			col, row := findOnScreen(t, term, branch)
			mouseClick(t, term, col, row, tuitest.MouseRight, 0)
		} else if err := term.SendKeys(tuitest.Ctrl('b'), "q"); err != nil {
			t.Fatal(err)
		}
		waitScreen(t, term, "kill-and-go-next action", "Kill session, go to next")
		col, row := findOnScreen(t, term, "Kill session, go to next")
		mouseClick(t, term, col, row, tuitest.MouseLeft, 0)
		waitForAll(t, term, uiTimeout, "worktree close confirmation", "Close "+session+"?", "Delete worktree too")
	}
	open()
	saveArtifact(t, term, artifactDir(t), "kill-next-confirmation")
	if err := term.SendKeys(tuitest.Esc); err != nil {
		t.Fatal(err)
	}
	if err := term.WaitFor(func(s tuitest.Screen) bool {
		return !strings.Contains(s.Text(), "Close "+session+"?")
	}, uiTimeout); err != nil {
		t.Fatal(err)
	}
	if names := sessionNames(t, base); !hasName(names, session) || !hasName(names, "plain") {
		t.Fatalf("cancel changed the sessions: %v", names)
	}
	if row := railLineWith(term.Screen(), branch); row < 0 || !strings.Contains(term.Screen().Line(row), "▎") {
		t.Fatalf("cancel switched away from the worktree\n%s", term.Snapshot())
	}

	open()
	answer := "Close session"
	if remove {
		answer = "Delete worktree too"
	}
	col, row := findOnScreen(t, term, answer)
	mouseClick(t, term, col, row, tuitest.MouseLeft, 0)
	if err := term.WaitFor(func(s tuitest.Screen) bool {
		names := sessionNames(t, base)
		row := railLineWith(s, "plain")
		return !hasName(names, session) && hasName(names, "plain") && row >= 0 && strings.Contains(s.Line(row), "▎")
	}, uiTimeout); err != nil {
		t.Fatalf("kill-and-go-next did not leave the client on plain: %v\n%s", err, term.Snapshot())
	}
	alive(t, term, "after kill-and-go-next")
	if remove && strings.Contains(term.Screen().Text(), "Killed session:") {
		t.Fatalf("the kill notification replaced the worktree recovery note\n%s", term.Snapshot())
	}
	saveArtifact(t, term, artifactDir(t), "kill-next-complete")
}

// TestWorktreeKillNextConfirmsAndDeletes catches bypassing the confirmation,
// losing the requested switch after deletion, removing files on plain close,
// and losing untracked changes instead of stashing them.
func TestWorktreeKillNextConfirmsAndDeletes(t *testing.T) {
	for _, route := range []string{"sidebar", "quit-menu"} {
		for _, remove := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/delete=%t", route, remove), func(t *testing.T) {
				base := t.TempDir()
				killDaemon(t, base)
				repo := testutil.GitRepo(t)
				t.Setenv("TUIOS_WORKTREE_DIR", filepath.Join(base, "worktrees"))
				if out, err := tuiosCLIIn(t, base, t.TempDir(), "new", "plain", "--detach"); err != nil {
					t.Fatalf("create destination: %v: %s", err, out)
				}
				if out, err := tuiosCLI(t, base, "worktree", "new", "feat/task", "--repo", repo, "--detach"); err != nil {
					t.Fatalf("create worktree: %v: %s", err, out)
				}
				path := filepath.Join(base, "worktrees", "repo", "feat-task")
				if err := os.WriteFile(filepath.Join(path, "untracked.txt"), []byte("keep this work\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				driveWorktreeKillNext(t, base, "repo-feat-task", "feat/task", route, remove)
				if remove {
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Fatalf("deleted worktree still exists: %v", err)
					}
					if stash := testutil.Git(t, repo, "stash", "show", "-p", "--include-untracked"); !strings.Contains(stash, "keep this work") {
						t.Fatalf("untracked work was not stashed: %s", stash)
					}
				} else if content, err := os.ReadFile(filepath.Join(path, "untracked.txt")); err != nil || string(content) != "keep this work\n" {
					t.Fatalf("closing the session changed its worktree: %q: %v", content, err)
				}
				testutil.Git(t, repo, "show-ref", "--verify", "refs/heads/feat/task")
			})
		}
	}
}

// TestArcWorktreeKillNext verifies the same UI against real FUSE mounts. All
// mutations are confined to the fixture's mounts; the source is read only.
func TestArcWorktreeKillNext(t *testing.T) {
	for _, route := range []string{"sidebar", "quit-menu"} {
		t.Run(route, func(t *testing.T) {
			base, source, arcRun := arcFixture(t)
			var info struct {
				Repository string `json:"repository"`
			}
			if err := json.Unmarshal([]byte(arcRun(source, "info", "--json")), &info); err != nil {
				t.Fatal(err)
			}
			branch := fmt.Sprintf("t-%x", time.Now().UnixNano()%0xffffff)
			path := filepath.Join(base, "worktrees", info.Repository, branch)
			t.Cleanup(func() {
				_, _ = tuiosCLI(t, base, "kill-server")
				cmd := exec.Command("arc", "unmount", "--force", "--forget", path)
				cmd.Dir = os.TempDir()
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Logf("cleanup test mount: %v: %s", err, out)
				}
			})
			if out, err := tuiosCLIIn(t, base, t.TempDir(), "new", "plain", "--detach"); err != nil {
				t.Fatalf("create destination: %v: %s", err, out)
			}
			if out, err := tuiosCLI(t, base, "worktree", "new", branch, "--repo", source, "--detach"); err != nil {
				t.Fatalf("create mount: %v: %s", err, out)
			}
			if err := os.WriteFile(filepath.Join(path, "tuios-kill-next.txt"), []byte("keep this Arc work\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			driveWorktreeKillNext(t, base, info.Repository+"-"+branch, branch, route, true)
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("removed mount still exists: %v", err)
			}
			arcRun(os.TempDir(), "mount", "-r", info.Repository, "-m", path)
			arcRun(path, "checkout", branch)
			if stash := arcRun(path, "stash", "list"); !strings.Contains(stash, "tuios: "+branch) {
				t.Fatalf("Arc store did not retain the stash: %s", stash)
			}
		})
	}
}
