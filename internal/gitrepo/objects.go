package gitrepo

import (
	"io"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Entry is a non-tree object at a path: a blob with its mode, or a gitlink.
type Entry struct {
	Mode filemode.FileMode
	Hash Hash
}

// Signature is an author or committer.
type Signature struct {
	Name, Email string
	When        time.Time
}

// Commit is what gb writes. Trailers belong in Message.
type Commit struct {
	Tree      Hash
	Parents   []Hash
	Author    Signature
	Committer Signature
	Message   string
}

// WriteBlob stores data and returns its id.
func (r *Repo) WriteBlob(data []byte) (Hash, error) {
	obj := r.r.Storer.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	w, err := obj.Writer()
	if err != nil {
		return ZeroHash, err
	}
	if _, err := w.Write(data); err != nil {
		return ZeroHash, err
	}
	if err := w.Close(); err != nil {
		return ZeroHash, err
	}
	return r.r.Storer.SetEncodedObject(obj)
}

// ReadBlob returns a blob's contents.
func (r *Repo) ReadBlob(h Hash) ([]byte, error) {
	b, err := r.r.BlobObject(h)
	if err != nil {
		return nil, err
	}
	rd, err := b.Reader()
	if err != nil {
		return nil, err
	}
	defer rd.Close()
	return io.ReadAll(rd)
}

// WriteCommit stores a commit object.
func (r *Repo) WriteCommit(c Commit) (Hash, error) {
	oc := &object.Commit{
		Author:       object.Signature{Name: c.Author.Name, Email: c.Author.Email, When: c.Author.When},
		Committer:    object.Signature{Name: c.Committer.Name, Email: c.Committer.Email, When: c.Committer.When},
		Message:      c.Message,
		TreeHash:     c.Tree,
		ParentHashes: c.Parents,
	}
	obj := r.r.Storer.NewEncodedObject()
	if err := oc.Encode(obj); err != nil {
		return ZeroHash, err
	}
	return r.r.Storer.SetEncodedObject(obj)
}

// CommitInfo is the part of a commit gb reads back.
type CommitInfo struct {
	Hash    Hash
	Tree    Hash
	Parents []Hash
	Author  Signature
	Message string
}

// ReadCommit loads a commit.
func (r *Repo) ReadCommit(h Hash) (CommitInfo, error) {
	c, err := r.r.CommitObject(h)
	if err != nil {
		return CommitInfo{}, err
	}
	return CommitInfo{
		Hash: h, Tree: c.TreeHash, Parents: c.ParentHashes, Message: c.Message,
		Author: Signature{Name: c.Author.Name, Email: c.Author.Email, When: c.Author.When},
	}, nil
}

// TreeEntries flattens a tree into path -> entry. A zero tree is empty.
func (r *Repo) TreeEntries(tree Hash) (map[string]Entry, error) {
	out := map[string]Entry{}
	if tree.IsZero() {
		return out, nil
	}
	t, err := r.r.TreeObject(tree)
	if err != nil {
		return nil, err
	}
	var walk func(t *object.Tree, prefix string) error
	walk = func(t *object.Tree, prefix string) error {
		for _, e := range t.Entries {
			p := prefix + e.Name
			if e.Mode == filemode.Dir {
				sub, err := r.r.TreeObject(e.Hash)
				if err != nil {
					return err
				}
				if err := walk(sub, p+"/"); err != nil {
					return err
				}
				continue
			}
			out[p] = Entry{Mode: e.Mode, Hash: e.Hash}
		}
		return nil
	}
	return out, walk(t, "")
}

// WriteTree builds nested trees from flat path -> entry. No index or worktree
// is involved.
func (r *Repo) WriteTree(entries map[string]Entry) (Hash, error) {
	type node struct {
		files map[string]Entry
		dirs  map[string]*node
	}
	newNode := func() *node { return &node{files: map[string]Entry{}, dirs: map[string]*node{}} }
	root := newNode()
	for p, e := range entries {
		parts := strings.Split(p, "/")
		n := root
		for _, d := range parts[:len(parts)-1] {
			if n.dirs[d] == nil {
				n.dirs[d] = newNode()
			}
			n = n.dirs[d]
		}
		n.files[parts[len(parts)-1]] = e
	}
	var build func(n *node) (Hash, error)
	build = func(n *node) (Hash, error) {
		var es []object.TreeEntry
		for name, e := range n.files {
			es = append(es, object.TreeEntry{Name: name, Mode: e.Mode, Hash: e.Hash})
		}
		for name, child := range n.dirs {
			h, err := build(child)
			if err != nil {
				return ZeroHash, err
			}
			es = append(es, object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: h})
		}
		// git orders tree entries as if directory names ended in "/".
		key := func(e object.TreeEntry) string {
			if e.Mode == filemode.Dir {
				return e.Name + "/"
			}
			return e.Name
		}
		sort.Slice(es, func(i, j int) bool { return key(es[i]) < key(es[j]) })
		obj := r.r.Storer.NewEncodedObject()
		if err := (&object.Tree{Entries: es}).Encode(obj); err != nil {
			return ZeroHash, err
		}
		return r.r.Storer.SetEncodedObject(obj)
	}
	return build(root)
}

// ReadFiles returns the contents of every file under prefix in tree, keyed by
// path relative to prefix. An empty prefix means the whole tree.
func (r *Repo) ReadFiles(tree Hash, prefix string) (map[string][]byte, error) {
	entries, err := r.TreeEntries(tree)
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for p, e := range entries {
		rel, ok := underPrefix(p, prefix)
		if !ok || e.Mode == filemode.Submodule {
			continue
		}
		data, err := r.ReadBlob(e.Hash)
		if err != nil {
			return nil, err
		}
		out[rel] = data
	}
	return out, nil
}

// ReplaceSubtree returns base with everything under prefix replaced by files.
// Everything outside prefix is kept as is.
func (r *Repo) ReplaceSubtree(base Hash, prefix string, files map[string][]byte) (Hash, error) {
	entries, err := r.TreeEntries(base)
	if err != nil {
		return ZeroHash, err
	}
	old := map[string]Entry{}
	for p, e := range entries {
		if rel, ok := underPrefix(p, prefix); ok {
			old[rel] = e
			delete(entries, p)
		}
	}
	for rel, data := range files {
		h, err := r.WriteBlob(data)
		if err != nil {
			return ZeroHash, err
		}
		mode := filemode.Regular
		if o, ok := old[rel]; ok && o.Mode == filemode.Executable {
			mode = o.Mode
		}
		entries[joinPrefix(prefix, rel)] = Entry{Mode: mode, Hash: h}
	}
	return r.WriteTree(entries)
}

// SubtreeEqual reports whether two trees have identical content under prefix.
func (r *Repo) SubtreeEqual(a, b Hash, prefix string) (bool, error) {
	if a == b {
		return true, nil
	}
	ea, err := r.TreeEntries(a)
	if err != nil {
		return false, err
	}
	eb, err := r.TreeEntries(b)
	if err != nil {
		return false, err
	}
	count := 0
	for p, e := range ea {
		if _, ok := underPrefix(p, prefix); !ok {
			continue
		}
		count++
		if eb[p] != e {
			return false, nil
		}
	}
	for p := range eb {
		if _, ok := underPrefix(p, prefix); ok {
			count--
		}
	}
	return count == 0, nil
}

func underPrefix(p, prefix string) (string, bool) {
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return p, true
	}
	if strings.HasPrefix(p, prefix+"/") {
		return p[len(prefix)+1:], true
	}
	return "", false
}

func joinPrefix(prefix, rel string) string {
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return rel
	}
	return prefix + "/" + rel
}
