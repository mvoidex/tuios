package arc

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// infoKey changes when a mount switches branch, repository or backing store.
// A branch's tip may move without changing HEAD; registration only needs its
// name, and operations resolving commits always use ReadInfo instead.
type infoKey struct {
	head, target string
	configTime   int64
	configSize   int64
}

type cachedInfo struct {
	mu      sync.Mutex
	key     infoKey
	keyOK   bool
	checked time.Time
	info    Info
	err     error
}

var infoCache = struct {
	sync.Mutex
	entries map[string]*cachedInfo
}{entries: make(map[string]*cachedInfo)}

// ReadInfoCached reads mount identity for automatic registration. It caches
// successful lookups until HEAD or config changes, and retries failed lookups
// after five seconds on the next call. It starts no timers or goroutines.
// ReadInfo must be used when the current commit, rather than identity, matters.
func ReadInfoCached(ctx context.Context, root string) (Info, error) {
	infoCache.Lock()
	entry := infoCache.entries[root]
	if entry == nil {
		if len(infoCache.entries) >= 256 {
			clear(infoCache.entries)
		}
		entry = &cachedInfo{}
		infoCache.entries[root] = entry
	}
	infoCache.Unlock()
	entry.mu.Lock()
	defer entry.mu.Unlock()
	key, err := mountInfoKey(root)
	keyOK := err == nil
	if !entry.checked.IsZero() && key == entry.key && keyOK == entry.keyOK {
		if (entry.err == nil && keyOK) || time.Since(entry.checked) < 5*time.Second {
			return entry.info, entry.err
		}
	}
	entry.key, entry.keyOK = key, keyOK
	entry.info, entry.err = ReadInfo(ctx, root)
	entry.checked = time.Now()
	return entry.info, entry.err
}

func mountInfoKey(root string) (infoKey, error) {
	marker := filepath.Join(root, ".arc")
	file, err := os.Open(filepath.Join(marker, "HEAD"))
	if err != nil {
		return infoKey{}, err
	}
	defer func() { _ = file.Close() }()
	head, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return infoKey{}, err
	}
	if len(head) > 4096 {
		return infoKey{}, fmt.Errorf("arc HEAD exceeds 4096 bytes")
	}
	key := infoKey{head: string(head)}
	key.target, _ = os.Readlink(marker)
	if config, err := os.Stat(filepath.Join(marker, "config")); err == nil {
		key.configTime, key.configSize = config.ModTime().UnixNano(), config.Size()
	}
	return key, nil
}
