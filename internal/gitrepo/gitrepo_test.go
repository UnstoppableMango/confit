package gitrepo

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newRepo(t *testing.T) (*Repo, Hash) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "r.git")
	if err := Init(dir, "desired", true); err != nil {
		t.Fatal(err)
	}
	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	blob, _ := r.WriteBlob([]byte("x\n"))
	tree, _ := r.WriteTree(map[string]Entry{"keep/x": {Mode: 0o100644, Hash: blob}, "sub/y": {Mode: 0o100644, Hash: blob}})
	sig := Signature{Name: "t", Email: "t@t", When: time.Unix(0, 0)}
	c, err := r.WriteCommit(Commit{Tree: tree, Author: sig, Committer: sig, Message: "root\n"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateRefs("t", RefUpdate{Name: "refs/heads/desired", New: c}); err != nil {
		t.Fatal(err)
	}
	return r, c
}

func TestUpdateRefsIsAtomicCAS(t *testing.T) {
	r, c := newRepo(t)
	ci, _ := r.ReadCommit(c)
	sig := Signature{Name: "t", Email: "t@t", When: time.Unix(1, 0)}
	c2, _ := r.WriteCommit(Commit{Tree: ci.Tree, Parents: []Hash{c}, Author: sig, Committer: sig, Message: "2\n"})

	// Second update has a wrong old value: neither ref may move.
	err := r.UpdateRefs("t",
		RefUpdate{Name: "refs/heads/desired", New: c2, Old: c},
		RefUpdate{Name: "refs/heads/applied/x", New: c2, Old: c2})
	if !errors.Is(err, ErrRefChanged) {
		t.Fatalf("want ErrRefChanged, got %v", err)
	}
	if h, _ := r.Resolve("refs/heads/desired"); h != c {
		t.Fatal("desired moved although the transaction failed")
	}
	// Creating an existing ref is also a lost race.
	if err := r.UpdateRefs("t", RefUpdate{Name: "refs/heads/desired", New: c2}); !errors.Is(err, ErrRefChanged) {
		t.Fatalf("want ErrRefChanged on create, got %v", err)
	}
	if err := r.UpdateRefs("t", RefUpdate{Name: "refs/heads/desired", New: c2, Old: c}); err != nil {
		t.Fatal(err)
	}
}

func TestReplaceSubtreeKeepsOtherPaths(t *testing.T) {
	r, c := newRepo(t)
	ci, _ := r.ReadCommit(c)
	tree, err := r.ReplaceSubtree(ci.Tree, "sub", map[string][]byte{"z/new": []byte("n\n")})
	if err != nil {
		t.Fatal(err)
	}
	files, _ := r.ReadFiles(tree, "")
	if _, ok := files["keep/x"]; !ok || files["sub/z/new"] == nil || files["sub/y"] != nil {
		t.Fatalf("unexpected files %v", files)
	}
	if eq, _ := r.SubtreeEqual(ci.Tree, tree, "keep"); !eq {
		t.Fatal("keep/ should be untouched")
	}
	// git agrees with our tree encoding.
	if _, err := r.Git("fsck", "--no-dangling"); err != nil {
		t.Fatal(err)
	}
}

// git notes add loses notes under concurrency (see spike E4); ours must not.
func TestAppendNoteConcurrent(t *testing.T) {
	r, c := newRepo(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rr, _ := Open(r.GitDir) // separate handle, like a separate process
			sig := Signature{Name: "t", Email: "t@t", When: time.Now()}
			if err := rr.AppendNote("refs/notes/buffer-applied", c, fmt.Sprintf("Consumer: c%d\n", i), sig); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	note, err := r.Git("notes", "--ref=buffer-applied", "show", c.String())
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(note, "Consumer: "); n != 8 {
		t.Fatalf("want 8 entries, got %d:\n%s", n, note)
	}
}

func TestMergeTreeUsesAttrTreeNotHead(t *testing.T) {
	r, _ := newRepo(t)
	// HEAD points at a branch that does not exist; attributes must still
	// come from the tree we name.
	if _, err := r.Git("symbolic-ref", "HEAD", "refs/heads/nothing"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Git("config", "merge.always-theirs.driver", "cp %B %A"); err != nil {
		t.Fatal(err)
	}
	sig := Signature{Name: "t", Email: "t@t", When: time.Unix(0, 0)}
	mk := func(content string, parents ...Hash) Hash {
		attrs, _ := r.WriteBlob([]byte("*.txt merge=always-theirs\n"))
		f, _ := r.WriteBlob([]byte(content))
		tree, _ := r.WriteTree(map[string]Entry{".gitattributes": {Mode: 0o100644, Hash: attrs}, "f.txt": {Mode: 0o100644, Hash: f}})
		c, _ := r.WriteCommit(Commit{Tree: tree, Parents: parents, Author: sig, Committer: sig, Message: content})
		return c
	}
	base := mk("base\n")
	ours := mk("ours\n", base)
	theirs := mk("theirs\n", base)
	r.UpdateRefs("t", RefUpdate{Name: "refs/heads/target", New: ours})
	m, err := r.MergeTree(ours, theirs, "refs/heads/target")
	if err != nil || !m.Clean {
		t.Fatalf("driver did not run: %+v %v", m, err)
	}
	files, _ := r.ReadFiles(m.Tree, "")
	if string(files["f.txt"]) != "theirs\n" {
		t.Fatalf("got %q", files["f.txt"])
	}
}
