package arc

import (
	"os"
	"path/filepath"
	"testing"
)

// Detection must prefer an Arc mount's marker over its Git compatibility
// directory, but stop at a nested Git repository and ignore a user's .arc.
func TestLocateRepositoryBoundary(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{".arc", ".git", "project", "nested/.git"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".arc", "HEAD"), []byte("ref: trunk\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{root, filepath.Join(root, "project")} {
		if got, ok := Locate(dir); !ok || got != root {
			t.Errorf("Locate(%q) = (%q, %v), want (%q, true)", dir, got, ok, root)
		}
	}
	if got, ok := Locate(filepath.Join(root, "nested")); ok {
		t.Errorf("nested Git repository detected as Arc: %q", got)
	}
	if err := os.Remove(filepath.Join(root, ".arc", "HEAD")); err != nil {
		t.Fatal(err)
	}
	if got, ok := Locate(root); ok {
		t.Errorf("non-mount .arc detected as Arc: %q", got)
	}
}
