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

	"github.com/Gaurav-Gosain/tuitest"
)

// arcFixture isolates tuios while letting the real Arc CLI use its launcher
// and credentials. All mounts written by callers must be owned by the test.
func arcFixture(t *testing.T) (base, source string, arcRun func(string, ...string) string) {
	t.Helper()
	source = os.Getenv("TUIOS_E2E_ARC_SOURCE")
	if source == "" {
		t.Skip("set TUIOS_E2E_ARC_SOURCE to opt into real Arc mounts")
	}
	arcBin, err := exec.LookPath("arc")
	if err != nil {
		t.Fatal(err)
	}
	base, _ = fanFixture(t)
	// The daemon's HOME remains isolated. Only Arc needs the real launcher's
	// installed releases and credentials. Every mount it writes is test-owned.
	bin := filepath.Join(base, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	script := "#!/bin/sh\nexport HOME=" + quote(os.Getenv("HOME")) + "\nexec " + quote(arcBin) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "arc"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	arcRun = func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command(filepath.Join(bin, "arc"), args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("arc %v: %v: %s", args, err, out)
		}
		return string(out)
	}
	return base, source, arcRun
}

// TestArcReviewAndManagedMount uses a real Arc service and FUSE mount. Opt in
// with TUIOS_E2E_ARC_SOURCE pointing inside an already mounted repository. The
// source is only read; all edits and commits stay in the mount this test makes.
// Nothing is pushed. The test saves the review, notes and delivered message as
// artifacts, and checks that removing the mount preserves its local branch.
func TestArcReviewAndManagedMount(t *testing.T) {
	base, source, arcRun := arcFixture(t)
	var metadata struct {
		Repository string `json:"repository"`
	}
	if err := json.Unmarshal([]byte(arcRun(source, "info", "--json")), &metadata); err != nil {
		t.Fatal(err)
	}
	branch := fmt.Sprintf("tuios-e2e-%d", time.Now().UnixNano())
	path := filepath.Join(base, "worktrees", metadata.Repository, branch)
	childBranch := branch + "-child"
	childPath := filepath.Join(base, "worktrees", metadata.Repository, childBranch)
	t.Cleanup(func() {
		// unmount must run outside the repository after its panes have stopped.
		_, _ = tuiosCLI(t, base, "kill-server")
		for _, mountPath := range []string{childPath, path} {
			cmd := exec.Command("arc", "unmount", "--forget", mountPath)
			cmd.Dir = os.TempDir()
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Logf("cleanup Arc mount: %v: %s", err, out)
			}
		}
	})
	if out, err := tuiosCLI(t, base, "new", "plain", "--detach"); err != nil {
		t.Fatalf("start daemon: %v: %s", err, out)
	}
	out, err := tuiosCLI(t, base, "worktree", "new", branch, "--repo", source, "--agent", "claude", "--detach")
	if err != nil {
		t.Fatalf("create Arc mount: %v: %s", err, out)
	}
	rows := worktreeRows(t, base)
	if len(rows) != 1 || rows[0]["vcs"] != "arc" || rows[0]["repo"] != metadata.Repository || rows[0]["managed"] != true {
		t.Fatalf("managed Arc worktree listing: %#v", rows)
	}
	session, _ := rows[0]["session"].(string)
	if got := rows[0]["path"]; got != path {
		t.Fatalf("mount path = %v, want %s", got, path)
	}
	const folder = "tuios-e2e-review"
	if err := os.Mkdir(filepath.Join(path, folder), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(path, folder, "notes.txt")
	if err := os.WriteFile(file, []byte("committed line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	arcRun(path, "add", "--", folder)
	arcRun(path, "commit", "-m", "test: Arc review fixture", "--no-verify", "--skip-hook", "prepare-commit-msg")
	// A new mount can branch from this commit without pushing it anywhere.
	if out, err := tuiosCLI(t, base, "worktree", "new", childBranch, "--repo", path, "--detach"); err != nil {
		t.Fatalf("mount from an unpushed commit: %v: %s", err, out)
	}
	childSession := metadata.Repository + "-" + childBranch
	if got, err := os.ReadFile(filepath.Join(childPath, folder, "notes.txt")); err != nil || string(got) != "committed line\n" {
		t.Fatalf("child mount did not inherit the local commit: %q (%v)", got, err)
	}
	// An MCP child can outlive its pane and keep a cwd in the mount. Explicit
	// removal must force the unmount while retaining Arc's backing store.
	holder := exec.Command("/bin/sh", "-c", "exec sleep 120")
	holder.Dir = childPath
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Process.Kill(); _ = holder.Wait() })
	if out, err := tuiosCLI(t, base, "worktree", "rm", childSession, "--keep-session"); err != nil {
		t.Fatalf("remove clean child mount: %v: %s", err, out)
	}
	_ = holder.Process.Kill()
	if out, err := tuiosCLI(t, base, "kill-session", childSession); err != nil {
		t.Fatalf("close the retained child session: %v: %s", err, out)
	}
	if err := os.WriteFile(file, []byte("committed line\nreview this line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	untracked := filepath.Join(path, folder, "new file.txt")
	if err := os.WriteFile(untracked, []byte("untracked line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := arcRun(path, "status", "--json", "-u", "all", "--no-ahead-behind")
	out, err = tuiosCLI(t, base, "review", session, "--json")
	if err != nil {
		t.Fatalf("review Arc: %v: %s", err, out)
	}
	var diff struct {
		VCS   string `json:"vcs"`
		Files []struct {
			Path   string `json:"path"`
			Status string `json:"status"`
			Added  int    `json:"added"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(out), &diff); err != nil {
		t.Fatal(err)
	}
	if diff.VCS != "arc" || len(diff.Files) != 2 || !strings.Contains(out, "committed line") || !strings.Contains(out, "review this line") {
		t.Fatalf("review omitted committed, uncommitted or untracked work: %s", out)
	}
	for _, f := range diff.Files {
		if f.Path == folder+"/new file.txt" && (f.Status != "U" || f.Added != 1) {
			t.Errorf("untracked file: %+v", f)
		}
	}
	writeReviewArtifact(t, "arc-review.json", out)
	if after := arcRun(path, "status", "--json", "-u", "all", "--no-ahead-behind"); after != before {
		t.Fatalf("review modified Arc state:\nbefore %s\nafter %s", before, after)
	}
	if out, err := tuiosCLI(t, base, "review", "note", "-s", session, folder+"/notes.txt:2", "Explain this choice"); err != nil {
		t.Fatalf("add review note: %v: %s", err, out)
	}
	if err := os.WriteFile(file, []byte("inserted line\ncommitted line\nreview this line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := tuiosCLI(t, base, "review", session); err != nil {
		t.Fatalf("refresh review: %v: %s", err, out)
	}
	out, err = tuiosCLI(t, base, "review", "notes", "-s", session, "--json")
	if err != nil {
		t.Fatalf("read notes: %v: %s", err, out)
	}
	var notes struct {
		Notes []struct {
			Line     int  `json:"line"`
			Outdated bool `json:"outdated"`
		} `json:"notes"`
	}
	if err := json.Unmarshal([]byte(out), &notes); err != nil || len(notes.Notes) != 1 || notes.Notes[0].Line != 3 || notes.Notes[0].Outdated {
		t.Fatalf("note did not follow its line: %s (%v)", out, err)
	}
	writeReviewArtifact(t, "arc-notes.json", out)
	if out, err := tuiosCLI(t, base, "review", "send", "-s", session); err != nil {
		t.Fatalf("send review: %v: %s", err, out)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		out, _ = tuiosCLI(t, base, "capture-pane", "-s", session)
		if strings.Contains(out, "GOT:    Explain this choice") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent never received review note: %s", out)
		}
		time.Sleep(200 * time.Millisecond)
	}
	writeReviewArtifact(t, "arc-delivered.txt", out)
	if out, err := tuiosCLI(t, base, "worktree", "rm", session); err == nil {
		t.Fatalf("removed dirty Arc mount without --stash or --force: %s", out)
	}
	if out, err := tuiosCLI(t, base, "worktree", "rm", session, "--stash"); err != nil {
		t.Fatalf("remove Arc mount: %v: %s", err, out)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("mount directory still exists: %v", err)
	}
	if rows := worktreeRows(t, base); len(rows) != 0 {
		t.Fatalf("removed mount still listed: %#v", rows)
	}
	// Arc retains the local store on unmount: a local commit must survive.
	arcRun(os.TempDir(), "mount", "-r", metadata.Repository, "-m", path)
	arcRun(path, "checkout", branch)
	if got, err := os.ReadFile(file); err != nil || string(got) != "committed line\n" {
		t.Fatalf("local branch was lost during removal: %q (%v)", got, err)
	}
	if out := arcRun(path, "stash", "list"); !strings.Contains(out, "tuios:") {
		t.Fatalf("stash was lost during removal: %s", out)
	}
}

// TestArcManualMountRegistration enters a mount made outside tuios, follows
// branch and directory changes, creates a child through the actual TUI menu,
// and verifies that killing either session leaves its mount mounted.
func TestArcManualMountRegistration(t *testing.T) {
	base, source, arcRun := arcFixture(t)
	var metadata struct {
		Repository string `json:"repository"`
	}
	if err := json.Unmarshal([]byte(arcRun(source, "info", "--json")), &metadata); err != nil {
		t.Fatal(err)
	}
	manualPath := filepath.Join(base, "manual mount")
	branch := fmt.Sprintf("t-%x", time.Now().UnixNano()%0xffffff)
	childBranch := branch + "-child"
	childPath := filepath.Join(base, "worktrees", metadata.Repository, childBranch)
	arcRun(os.TempDir(), "mount", "-r", metadata.Repository, "-m", manualPath)
	t.Cleanup(func() {
		_, _ = tuiosCLI(t, base, "kill-server")
		for _, path := range []string{childPath, manualPath} {
			cmd := exec.Command("arc", "unmount", "--force", "--forget", path)
			cmd.Dir = os.TempDir()
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Logf("cleanup Arc fixture: %v: %s", err, out)
			}
		}
	})
	arcRun(manualPath, "checkout", "-b", branch)
	subdir := filepath.Join(manualPath, "tuios-e2e-registration")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := tuiosCLIIn(t, base, subdir, "new", "manual", "--detach"); err != nil {
		t.Fatalf("start tuios inside the manual mount: %v: %s", err, out)
	}
	waitManual := func(wantBranch string, present bool) {
		t.Helper()
		deadline := time.Now().Add(uiTimeout)
		for {
			found := false
			rows := worktreeRows(t, base)
			for _, row := range rows {
				if row["session"] != "manual" {
					continue
				}
				found = true
				if present && row["vcs"] == "arc" && row["repo"] == metadata.Repository && row["branch"] == wantBranch && row["managed"] == false {
					return
				}
			}
			if !present && !found {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("manual Arc registration (branch %q, present %t): %#v", wantBranch, present, rows)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	waitManual(branch, true)
	runManual := func(command string) {
		t.Helper()
		if out, err := tuiosCLI(t, base, "send-keys", "-s", "manual", "--literal", command); err != nil {
			t.Fatalf("write to manual mount's shell: %v: %s", err, out)
		}
		if out, err := tuiosCLI(t, base, "send-keys", "-s", "manual", "Enter"); err != nil {
			t.Fatalf("run in manual mount's shell: %v: %s", err, out)
		}
	}
	// HEAD changes must invalidate cached metadata; saves in an unchanged
	// checkout must not launch arc info on every terminal update.
	branch += "-switched"
	arcRun(manualPath, "checkout", "-b", branch)
	runManual("echo ARC_BRANCH_CHANGED")
	waitManual(branch, true)
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	for _, step := range []struct {
		dir     string
		present bool
	}{{base, false}, {subdir, true}} {
		runManual("cd " + quote(step.dir))
		waitManual(branch, step.present)
	}
	term := attachIn(t, base, "manual", startOpts{})
	windowManagementMode(t, term)
	toggleSidebarViaPalette(t, term)
	waitForAll(t, term, uiTimeout, "detected Arc repository in sidebar", sidebarHeader, metadata.Repository, branch)
	// A discovered mount enables creation, but is not offered for deletion.
	col, row := findOnScreen(t, term, branch)
	mouseClick(t, term, col, row, tuitest.MouseRight, 0)
	waitScreen(t, term, "manual mount session menu", "Kill session and quit")
	col, row = findOnScreen(t, term, "Kill session and quit")
	mouseClick(t, term, col, row, tuitest.MouseLeft, 0)
	waitScreen(t, term, "manual mount close dialog", "Cancel")
	if strings.Contains(term.Screen().Text(), "Delete worktree too") {
		t.Fatal("a manually created mount was offered for unmounting")
	}
	saveArtifact(t, term, artifactDir(t), "arc-manual-close")
	sendKeys(t, term, tuitest.Esc)
	if out, err := tuiosCLI(t, base, "worktree", "rm", "manual", "--force"); err == nil || !strings.Contains(out, "only Arc mounts created by tuios") {
		t.Fatalf("manual mount removal was not refused: %v: %s", err, out)
	}
	col, row = findOnScreen(t, term, metadata.Repository)
	mouseClick(t, term, col, row, tuitest.MouseRight, 0)
	waitScreen(t, term, "Arc repository menu", "New worktree...")
	col, row = findOnScreen(t, term, "New worktree...")
	mouseClick(t, term, col, row, tuitest.MouseLeft, 0)
	waitScreen(t, term, "Arc worktree creation dialog", "new worktree")
	sendKeys(t, term, childBranch)
	saveArtifact(t, term, artifactDir(t), "arc-create-from-manual")
	sendKeys(t, term, tuitest.Enter)
	childSession := metadata.Repository + "-" + childBranch
	deadline := time.Now().Add(30 * time.Second)
	for !hasName(sessionNames(t, base), childSession) {
		if time.Now().After(deadline) {
			t.Fatalf("the UI did not create the child Arc mount:\n%s", term.Snapshot())
		}
		time.Sleep(100 * time.Millisecond)
	}
	listing, err := tuiosCLI(t, base, "worktree", "ls", "--json")
	if err != nil {
		t.Fatal(err)
	}
	writeReviewArtifact(t, "arc-registration.json", listing)
	for _, session := range []string{childSession, "manual"} {
		if out, err := tuiosCLI(t, base, "kill-session", session); err != nil {
			t.Fatalf("kill %s: %v: %s", session, err, out)
		}
	}
	for _, path := range []string{childPath, manualPath} {
		if out := arcRun(path, "info", "--json"); !strings.Contains(out, metadata.Repository) {
			t.Fatalf("killing a session unmounted %s: %s", path, out)
		}
	}
}
