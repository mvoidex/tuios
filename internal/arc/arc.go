// Package arc reads Arc repositories and manages the FUSE mounts tuios creates.
// It leaves Arc's store in place on unmount, including local branches and stashes.
package arc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Gaurav-Gosain/tuios/internal/childproc"
)

// Locate finds the nearest Arc checkout without starting a process. A nested
// Git repository ends the search. HEAD distinguishes a checkout's .arc directory
// from the user's ~/.arc directory containing the launcher and mount registry.
func Locate(dir string) (string, bool) {
	if dir == "" {
		return "", false
	}
	for dir = filepath.Clean(dir); ; dir = filepath.Dir(dir) {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return "", false
		}
		marker := filepath.Join(dir, ".arc")
		if st, err := os.Lstat(marker); err == nil {
			if !st.IsDir() {
				return dir, true
			}
			if _, err := os.Stat(filepath.Join(marker, "HEAD")); err == nil {
				return dir, true
			}
		}
		if parent := filepath.Dir(dir); parent == dir {
			return "", false
		}
	}
}

// Info is the subset of arc info used to identify and branch a mount.
type Info struct {
	Repository    string `json:"repository"`
	Branch        string `json:"branch"`
	Hash          string `json:"hash"`
	DefaultBranch string `json:"default_branch"`
}

// ReadInfo reads the working copy's repository, branch and current commit.
func ReadInfo(ctx context.Context, dir string) (Info, error) {
	var info Info
	out, err := Run(ctx, dir, "info", "--json")
	if err != nil {
		return info, err
	}
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		return info, fmt.Errorf("decode arc info: %w", err)
	}
	if info.Repository == "" || info.Hash == "" {
		return info, errors.New("arc info returned no repository or commit")
	}
	return info, nil
}

// DefaultBranch reads the repository's default branch, usually trunk.
func DefaultBranch(ctx context.Context, dir string) (string, error) {
	out, err := Run(ctx, dir, "info", "--mount", "--json")
	if err != nil {
		return "", err
	}
	var info Info
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		return "", fmt.Errorf("decode arc mount info: %w", err)
	}
	return info.DefaultBranch, nil
}

// Entry is a path in Arc's JSON status or diff listing.
type Entry struct {
	Path   string `json:"path"`
	Status string `json:"status"`
	Type   string `json:"type"`
}

// Status reads all changed and untracked paths, without ahead/behind queries.
func Status(ctx context.Context, dir string) (map[string][]Entry, error) {
	out, err := Run(ctx, dir, "status", "--json", "-u", "all", "--no-ahead-behind")
	if err != nil {
		return nil, err
	}
	var result struct {
		Status map[string][]Entry `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		return nil, fmt.Errorf("decode arc status: %w", err)
	}
	return result.Status, nil
}

// Changed counts uncommitted and untracked paths. A path changed in both the
// index and files counts once.
func Changed(status map[string][]Entry) int {
	paths := map[string]bool{}
	for group, entries := range status {
		if group == "ignored" {
			continue
		}
		for _, entry := range entries {
			paths[entry.Path] = true
		}
	}
	return len(paths)
}

// DiffNames lists tracked paths that differ from base in the current files.
func DiffNames(ctx context.Context, dir, base string) ([]Entry, error) {
	out, err := Run(ctx, dir, "diff", "--name-status", "--json", base)
	if err != nil {
		return nil, err
	}
	var groups []struct {
		Names []Entry `json:"names"`
	}
	if err := json.Unmarshal([]byte(out), &groups); err != nil {
		return nil, fmt.Errorf("decode arc diff names: %w", err)
	}
	var entries []Entry
	for _, group := range groups {
		entries = append(entries, group.Names...)
	}
	return entries, nil
}

// ObjectStore finds the mount's object store, including one shared from
// another mount. Such a mount need not have an objects entry in its own .arc.
func ObjectStore(ctx context.Context, root string) (string, error) {
	if objects, err := filepath.EvalSymlinks(filepath.Join(root, ".arc", "objects")); err == nil {
		return objects, nil
	}
	out, err := Run(ctx, os.TempDir(), "mount", "--list", "--json")
	if err != nil {
		return "", err
	}
	var mounts []struct {
		Mount       string `json:"mount"`
		ObjectStore string `json:"object-store"`
	}
	if err := json.Unmarshal([]byte(out), &mounts); err != nil {
		return "", fmt.Errorf("decode arc mount list: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	for _, mount := range mounts {
		path, err := filepath.EvalSymlinks(mount.Mount)
		if err == nil && path == root && mount.ObjectStore != "" {
			return filepath.EvalSymlinks(mount.ObjectStore)
		}
	}
	return "", fmt.Errorf("no Arc object store found for %s", root)
}

// Mount creates an independent mount of repository at path. Arc chooses and
// manages its backing store; mounting this path again reopens that store.
func Mount(ctx context.Context, repository, path, objectStore string) error {
	if repository == "" || strings.HasPrefix(repository, "-") || strings.ContainsAny(repository, "\x00\r\n") {
		return fmt.Errorf("invalid Arc repository name %q", repository)
	}
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("%s already exists", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // a directory under tuios's worktree directory
		return err
	}
	args := []string{"mount", "-r", repository, "-m", path}
	if objectStore != "" {
		args = append(args, "--object-store", objectStore)
	}
	_, err := Run(ctx, os.TempDir(), args...)
	return err
}

// Unmount force-unmounts a tuios-created mount after its panes have stopped,
// so an agent's remaining MCP children cannot keep it busy. It retains the
// store and only removes the empty mount directory, never files recursively.
func Unmount(ctx context.Context, path string) error {
	if _, err := Run(ctx, os.TempDir(), "unmount", "--force", path); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("arc unmounted %s, but removing its empty directory failed: %w", path, err)
	}
	return nil
}

// Command starts an Arc helper bounded by ctx. In particular, output pipes
// inherited by a child cannot keep a canceled review waiting indefinitely.
func Command(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := childproc.CommandContext(ctx, "arc", args...)
	cmd.Dir = dir
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

// boundedOutput keeps a helper's output from consuming unbounded memory.
type boundedOutput struct {
	buffer bytes.Buffer
	limit  int
	full   bool
}

func (b *boundedOutput) Len() int { return b.buffer.Len() }

func (b *boundedOutput) String() string { return b.buffer.String() }

func (b *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	left := max(b.limit-b.Len(), 0)
	if n > left {
		b.full = true
		p = p[:left]
	}
	_, _ = b.buffer.Write(p)
	return n, nil
}

// Run executes one Arc command and returns its bounded stdout. Commands that
// emit diffs use Command and stream their output instead.
func Run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := Command(ctx, dir, args...)
	stdout := &boundedOutput{limit: 32 << 20}
	stderr := &boundedOutput{limit: 64 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("arc %s: %w", args[0], ctx.Err())
		}
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return "", fmt.Errorf("arc %s: %s", args[0], message)
	}
	if stdout.full {
		return "", fmt.Errorf("arc %s output exceeds %d bytes", args[0], stdout.limit)
	}
	return stdout.String(), nil
}
