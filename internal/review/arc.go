package review

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/Gaurav-Gosain/tuios/internal/arc"
)

func resolveArcBase(ctx context.Context, dir, requested, recorded string, uncommitted bool) (Base, error) {
	info, err := arc.ReadInfo(ctx, dir)
	if err != nil {
		return Base{}, err
	}
	head := Base{Name: "HEAD", SHA: info.Hash, Uncommitted: true, From: "head"}
	if uncommitted {
		return head, nil
	}
	fromRef := func(ref, from string) (Base, error) {
		if err := ValidRef(ref); err != nil {
			return Base{}, &RefError{Ref: ref, Err: err}
		}
		sha, err := arc.Run(ctx, dir, "merge-base", info.Hash, ref)
		if err != nil {
			return Base{}, &RefError{Ref: ref, Err: err}
		}
		sha = strings.TrimSpace(sha)
		if sha == "" {
			return Base{}, &RefError{Ref: ref, Err: errors.New("no common ancestor")}
		}
		return Base{Name: ref, SHA: sha, From: from}, nil
	}
	if requested != "" {
		return fromRef(requested, "param")
	}
	if recorded != "" {
		return fromRef(recorded, "worktree")
	}
	branch, err := arc.DefaultBranch(ctx, dir)
	if err != nil {
		return Base{}, err
	}
	if branch != "" {
		if base, err := fromRef(branch, "upstream"); err == nil {
			return base, nil
		} else if ctx.Err() != nil {
			return Base{}, err
		}
	}
	return head, nil
}

// buildArc reads the current tracked diff directly from Arc. Untracked files
// are added from status without staging them. One tracked diff supplies both
// hunks and counts, including counts beyond the display limits.
func buildArc(ctx context.Context, opt Options) (*Diff, error) {
	if opt.AgainstDir != "" || opt.Tree != "" {
		return nil, errors.New("arc review supports a base or uncommitted changes; comparing mounts and Git checkpoints is not supported")
	}
	lim := opt.Limits
	if lim.Files <= 0 {
		lim = DefaultLimits
	}
	entries, err := arc.DiffNames(ctx, opt.Dir, opt.Base.SHA)
	if err != nil {
		return nil, err
	}
	status, err := arc.Status(ctx, opt.Dir)
	if err != nil {
		return nil, err
	}
	d := &Diff{VCS: "arc", Base: opt.Base.Name, BaseSHA: opt.Base.SHA, Uncommitted: opt.Base.Uncommitted, Files: []File{}}
	seen := map[string]bool{}
	for _, entry := range entries {
		if err := ValidPath(entry.Path); err != nil {
			return nil, fmt.Errorf("arc diff path: %w", err)
		}
		if !underPaths(entry.Path, opt.Paths) {
			continue
		}
		f := File{Path: entry.Path, Status: StatusModified, Hunks: []Hunk{}}
		switch entry.Status {
		case "new file", "added":
			f.Status = StatusAdded
		case "deleted", "removed":
			f.Status = StatusDeleted
		}
		d.Files = append(d.Files, f)
		seen[f.Path] = true
	}
	// Keep Arc's listing order while parsing its unified stream; sorting
	// before this would attach hunks to the wrong files.
	remaining := lim.Bytes
	tracked := len(d.Files)
	if tracked > 0 {
		used, err := streamArcFiles(ctx, opt, d.Files, lim)
		if err != nil {
			return nil, err
		}
		remaining -= used
	}
	for _, entry := range status["untracked"] {
		if err := ValidPath(entry.Path); err != nil {
			return nil, fmt.Errorf("arc status path: %w", err)
		}
		if !seen[entry.Path] && underPaths(entry.Path, opt.Paths) {
			d.Files = append(d.Files, File{Path: entry.Path, Status: StatusUntracked, Hunks: []Hunk{}})
			seen[entry.Path] = true
		}
	}
	for i := range d.Files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		f := &d.Files[i]
		fileLim := lim
		fileLim.Bytes = max(remaining, 1)
		countsOnly := i >= lim.Files || remaining <= 0
		if f.Status == StatusUntracked {
			used, err := arcUntracked(ctx, opt, f, fileLim, countsOnly)
			if err != nil {
				return nil, err
			}
			remaining -= used
		}
		d.Truncated = d.Truncated || f.Truncated
		d.Totals.Files++
		d.Totals.Added += f.Added
		d.Totals.Removed += f.Removed
	}
	sort.Slice(d.Files, func(i, j int) bool { return d.Files[i].Path < d.Files[j].Path })
	return d, nil
}

type arcDiffReader struct {
	io.Reader
	read int
}

func (r *arcDiffReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.read += n
	return n, err
}

func streamArcFiles(ctx context.Context, opt Options, files []File, lim Limits) (int, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	args := []string{"diff", "--git", "--no-color", "--unified", strconv.Itoa(max(opt.Context, 0)), opt.Base.SHA}
	if len(opt.Paths) > 0 {
		args = append(args, "--")
		args = append(args, opt.Paths...)
	}
	cmd := arc.Command(ctx, opt.Dir, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	reader := &arcDiffReader{Reader: stdout}
	_, stopped, rerr := readUnifiedMode(reader, files, lim, true)
	if stopped || rerr != nil {
		cancel()
	}
	werr := cmd.Wait()
	if rerr != nil {
		return reader.read, rerr
	}
	if !stopped && werr != nil {
		if ctx.Err() != nil {
			return reader.read, fmt.Errorf("arc diff: %w", ctx.Err())
		}
		return reader.read, fmt.Errorf("arc diff: %s: %w", strings.TrimSpace(stderr.String()), werr)
	}
	return reader.read, nil
}

func arcUntracked(ctx context.Context, opt Options, f *File, lim Limits, countsOnly bool) (int, error) {
	// Resolve the parent, rather than the final symlink: a symlink's target is
	// its content in a diff, and must never cause reads outside the mount.
	parent, err := InsideRoot(opt.Dir, filepath.ToSlash(filepath.Dir(f.Path)))
	if err != nil {
		return 0, err
	}
	name := filepath.Join(parent, filepath.Base(f.Path))
	st, err := os.Lstat(name)
	if err != nil {
		return 0, err
	}
	if opt.MaxUntracked > 0 && st.Size() > opt.MaxUntracked {
		f.Skipped = true
		return 0, nil
	}
	var data []byte
	if st.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(name)
		if err != nil {
			return 0, err
		}
		data = []byte(target)
	} else if st.Mode().IsRegular() {
		file, err := os.Open(name)
		if err != nil {
			return 0, err
		}
		defer func() { _ = file.Close() }()
		// A large file is counted as a stream, with no allocation per line.
		if countsOnly || st.Size() > int64(lim.Bytes) {
			var buf [32 << 10]byte
			var last byte
			var size int
			for {
				if err := ctx.Err(); err != nil {
					return 0, err
				}
				n, err := file.Read(buf[:])
				if n > 0 {
					f.Binary = f.Binary || bytes.IndexByte(buf[:n], 0) >= 0
					f.Added += bytes.Count(buf[:n], []byte{'\n'})
					last, size = buf[n-1], size+n
				}
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					return 0, err
				}
			}
			if size > 0 && last != '\n' {
				f.Added++
			}
			if f.Binary {
				f.Added = 0
			} else {
				f.Truncated = true
			}
			return 0, nil
		}
		data, err = io.ReadAll(io.LimitReader(file, int64(lim.Bytes)+1))
		if err != nil {
			return 0, err
		}
	} else {
		f.Skipped = true
		return 0, nil
	}
	if bytes.IndexByte(data, 0) >= 0 {
		f.Binary = true
		return len(data), nil
	}
	var lines []string
	if len(data) > 0 {
		lines = strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	}
	f.Added = len(lines)
	if countsOnly || len(data) > lim.Bytes || len(lines) > lim.LinesPerFile {
		f.Truncated = true
		return len(data), nil
	}
	if len(lines) > 0 {
		h := Hunk{Header: fmt.Sprintf("@@ -0,0 +1,%d @@", len(lines)), NewStart: 1, NewLines: len(lines), Lines: []Line{}}
		for i, text := range lines {
			h.Lines = append(h.Lines, Line{Op: OpAdd, New: i + 1, Text: strings.TrimSuffix(text, "\r"), raw: text,
				NoNewline: i == len(lines)-1 && !bytes.HasSuffix(data, []byte{'\n'})})
		}
		f.Hunks = []Hunk{h}
	}
	return len(data), nil
}
