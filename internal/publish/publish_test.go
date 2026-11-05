package publish

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCommitReplaceRemoveSweep(t *testing.T) {
	root := t.TempDir()
	w, err := NewWriter(root, "")
	if err != nil {
		t.Fatal(err)
	}
	commit := func(content string) string {
		st, err := w.Stage()
		if err != nil {
			t.Fatal(err)
		}
		if err := st.WriteFile("index.html", []byte(content)); err != nil {
			t.Fatal(err)
		}
		if err := st.WriteFile("a/x.png", []byte("img")); err != nil {
			t.Fatal(err)
		}
		v, err := w.Commit("slug1", st)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	v1 := commit("one")
	read := func() string {
		b, err := os.ReadFile(filepath.Join(root, "p", "slug1", "index.html"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if read() != "one" {
		t.Fatal("first version not served")
	}

	// copy asset from the committed version into a new stage
	st, _ := w.Stage()
	ok, err := st.CopyFromVersion(v1, "a/x.png")
	if err != nil || !ok {
		t.Fatalf("copy from version: %v %v", ok, err)
	}
	st.Discard()

	v2 := commit("two")
	if read() != "two" {
		t.Fatal("second version not served")
	}
	if _, err := os.Stat(filepath.Join(root, ".v", v1)); !os.IsNotExist(err) {
		t.Fatal("old version not cleaned up")
	}
	info, _ := os.Stat(filepath.Join(root, ".v", v2, "index.html"))
	if info.Mode().Perm() != FileMode {
		t.Errorf("file mode = %v", info.Mode().Perm())
	}

	// orphan page and stray version
	if err := os.MkdirAll(filepath.Join(root, "p", "orphan"), 0o750); err != nil {
		t.Fatal(err)
	}
	removed, err := w.Sweep(map[string]bool{"slug1": true})
	if err != nil || len(removed) != 1 || removed[0] != "orphan" {
		t.Fatalf("sweep removed %v, %v", removed, err)
	}
	if read() != "two" {
		t.Fatal("sweep removed a kept page")
	}

	if err := w.Remove("slug1"); err != nil {
		t.Fatal(err)
	}
	if w.Exists("slug1") {
		t.Fatal("page still exists after remove")
	}
	if err := w.Remove("../etc"); err == nil {
		t.Fatal("path traversal slug accepted")
	}
}
