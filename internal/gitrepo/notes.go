package gitrepo

import (
	"errors"
	"fmt"
	"strings"
)

// AppendNote appends text to the note on target under notesRef (for example
// refs/notes/confit-applied), creating it if needed. It never uses
// `git notes add`, which drops notes under concurrent writers; instead it
// writes the notes commit itself and moves the ref by compare-and-swap.
func (r *Repo) AppendNote(notesRef string, target Hash, text string, sig Signature) error {
	for attempt := 0; attempt < 20; attempt++ {
		old, err := r.Resolve(notesRef)
		if err != nil {
			return err
		}
		var parents []Hash
		var tree Hash
		if !old.IsZero() {
			c, err := r.ReadCommit(old)
			if err != nil {
				return err
			}
			parents, tree = []Hash{old}, c.Tree
		}
		entries, err := r.TreeEntries(tree)
		if err != nil {
			return err
		}
		// Notes trees may use fanout (ab/cdef...); match on the joined path.
		var existing []byte
		for p, e := range entries {
			if strings.ReplaceAll(p, "/", "") == target.String() {
				if existing, err = r.ReadBlob(e.Hash); err != nil {
					return err
				}
				delete(entries, p)
			}
		}
		content := text
		if len(existing) > 0 {
			content = strings.TrimRight(string(existing), "\n") + "\n\n" + text
		}
		if !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		blob, err := r.WriteBlob([]byte(content))
		if err != nil {
			return err
		}
		entries[target.String()] = Entry{Mode: 0o100644, Hash: blob}
		newTree, err := r.WriteTree(entries)
		if err != nil {
			return err
		}
		c, err := r.WriteCommit(Commit{Tree: newTree, Parents: parents, Author: sig, Committer: sig,
			Message: "Notes added by confit\n"})
		if err != nil {
			return err
		}
		err = r.UpdateRefs("confit: note", RefUpdate{Name: notesRef, New: c, Old: old})
		if errors.Is(err, ErrRefChanged) {
			continue
		}
		return err
	}
	return fmt.Errorf("could not update %s: too much contention", notesRef)
}

// ReadNote returns the note on target, or "" if there is none.
func (r *Repo) ReadNote(notesRef string, target Hash) (string, error) {
	tip, err := r.Resolve(notesRef)
	if err != nil || tip.IsZero() {
		return "", err
	}
	c, err := r.ReadCommit(tip)
	if err != nil {
		return "", err
	}
	entries, err := r.TreeEntries(c.Tree)
	if err != nil {
		return "", err
	}
	for p, e := range entries {
		if strings.ReplaceAll(p, "/", "") == target.String() {
			b, err := r.ReadBlob(e.Hash)
			return string(b), err
		}
	}
	return "", nil
}
