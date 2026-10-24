package session

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/Gaurav-Gosain/tuios/internal/arc"
	"github.com/Gaurav-Gosain/tuios/internal/review"
)

// createArcMount resolves the starting commit in the source mount. Sharing
// its object store lets the new mount start from an unpushed local commit too.
// The base is a commit, since local branch refs belong to the source store.
func createArcMount(ctx context.Context, root, path, branch, base string, source arc.Info) (string, bool, error) {
	if base == "" {
		base = source.Hash
	}
	if err := review.ValidRef(base); err != nil {
		return "", false, err
	}
	sha, err := arc.Run(ctx, root, "rev-parse", base)
	if err != nil {
		return "", false, err
	}
	base = strings.TrimSpace(sha)
	if err := review.ValidRef(base); err != nil {
		return "", false, err
	}
	objects, err := arc.ObjectStore(ctx, root)
	if err != nil {
		return "", false, fmt.Errorf("find Arc object store: %w", err)
	}
	if err := arc.Mount(ctx, source.Repository, path, objects); err != nil {
		return "", false, err
	}
	// A store retained by an earlier removal can already have this branch.
	_, existsErr := arc.Run(ctx, path, "rev-parse", branch)
	created := existsErr != nil
	args := []string{"checkout", branch}
	if created {
		args = []string{"checkout", "-b", branch, base}
	}
	if _, err := arc.Run(ctx, path, args...); err != nil {
		return "", false, fmt.Errorf("the Arc mount is kept at %s, but checking out %s failed: %w", path, branch, err)
	}
	return base, created, nil
}

// removeArcMount stops the mount's processes before unmounting. keepSession
// retains the session record until the client has shown the result, but its
// PTYs still have to close for Arc to release the filesystem.
func (d *Daemon) removeArcMount(sess *Session, info *WorktreeInfo, force, stash, keepSession bool) (any, *verbError) {
	if !info.Managed {
		return nil, invalidParam("session", "only Arc mounts created by tuios can be removed")
	}
	ctx, cancel := context.WithTimeout(d.ctx, time.Minute)
	defer cancel()
	notesRoot := canonRoot(info.Path)
	status, err := arc.Status(ctx, info.Path)
	if err != nil {
		return nil, reviewGitFailed(err)
	}
	changes := arc.Changed(status)
	if changes > 0 && !force && !stash {
		return nil, hintedVerbError(ErrVerbWorktreeDirty,
			fmt.Sprintf("%s holds %d uncommitted changes. Nothing was removed.", info.Path, changes),
			&VerbHint{Param: "stash", Command: "tuios worktree rm " + sess.Name() + " --stash",
				Detail: "Pass stash to keep the changes in Arc stash, or force to unmount with the changes retained in Arc's store."})
	}
	store, err := filepath.EvalSymlinks(filepath.Join(info.Path, ".arc"))
	if err != nil {
		return nil, reviewGitFailed(err)
	}
	stashed := false
	if changes > 0 && stash {
		if _, err := arc.Run(ctx, info.Path, "stash", "push", "--include-untracked", "-m", "tuios: "+info.Branch); err != nil {
			return nil, reviewGitFailed(err)
		}
		stashed = true
	}
	if keepSession {
		for _, id := range sess.ListPTYIDs() {
			if err := sess.ClosePTY(id); err != nil {
				return nil, hintedVerbError(ErrVerbGitFailed, "close the Arc mount's pane: "+err.Error(), nil)
			}
		}
	} else if err := d.manager.DeleteSession(sess.Name()); err != nil {
		return nil, hintedVerbError(ErrVerbGitFailed, "close the Arc mount's session: "+err.Error(), nil)
	}
	if err := arc.Unmount(ctx, info.Path); err != nil {
		return nil, hintedVerbError(ErrVerbGitFailed, err.Error(), &VerbHint{
			Detail: "The panes were stopped. The Arc store is kept at " + store + ".",
		})
	}
	d.reviewNotes.dropRoot(notesRoot)
	return map[string]any{
		"type": "worktree_removed", "vcs": "arc", "session": sess.Name(),
		"branch": info.Branch, "path": info.Path, "repo": info.Repo,
		"branch_kept": true, "store_kept": true, "store": store,
		"changes": changes, "ignored": 0, "own_commits": 0,
		"stashed": stashed, "discarded": false, "session_killed": !keepSession,
		"stash_message": "tuios: " + info.Branch,
		"note":          "Arc store kept at " + store + ". Remount the same path to recover local branches and stashes.",
	}, nil
}
