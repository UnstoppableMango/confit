package core

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/UnstoppableMango/confit/internal/gitrepo"
)

// CommitSummary is one commit in a pending list.
type CommitSummary struct {
	Commit  string `json:"commit"`
	Subject string `json:"subject"`
	Editor  string `json:"editor,omitempty"`
}

// FileChange is one path in a pending diff: A(dded), M(odified), D(eleted).
type FileChange struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

// PendingResult is `confit pending`: what the consumer has not applied yet.
type PendingResult struct {
	Consumer string          `json:"consumer"`
	Source   string          `json:"source"`
	Applied  string          `json:"applied,omitempty"`
	Target   string          `json:"target"`
	Commits  []CommitSummary `json:"commits"`
	Files    []FileChange    `json:"files"`
	// Empty is true when there is nothing to apply. Commits with no file
	// changes (merges, syncs) can still be listed.
	Empty bool `json:"empty"`
}

// Pending lists commits in applied/<consumer>..<source> and the tree diff a
// consumer would apply. Consumers apply the tree at Target, not patches.
func (b *Buffer) Pending(consumer string) (PendingResult, error) {
	cs, err := b.consumer(consumer)
	if err != nil {
		return PendingResult{}, err
	}
	res := PendingResult{Consumer: consumer, Source: cs.Source, Commits: []CommitSummary{}, Files: []FileChange{}}
	src, err := b.Repo.Resolve(b.Config.sourceRef(cs))
	if err != nil {
		return res, err
	}
	if src.IsZero() {
		return res, fmt.Errorf("source branch %s does not exist", cs.Source)
	}
	applied, err := b.Repo.Resolve(b.Config.appliedRef(consumer))
	if err != nil {
		return res, err
	}
	res.Target, res.Applied = src.String(), hashOrEmpty(applied)
	args := []string{src.String()}
	if !applied.IsZero() {
		args = append(args, "^"+applied.String())
	}
	commits, err := b.Repo.RevList(args...)
	if err != nil {
		return res, err
	}
	for _, h := range commits {
		c, err := b.Repo.ReadCommit(h)
		if err != nil {
			return res, err
		}
		res.Commits = append(res.Commits, CommitSummary{Commit: h.String(), Subject: subject(c.Message), Editor: trailerValue(c.Message, "Confit-Editor")})
	}
	var fromTree gitrepo.Hash
	if !applied.IsZero() {
		ac, err := b.Repo.ReadCommit(applied)
		if err != nil {
			return res, err
		}
		fromTree = ac.Tree
	}
	sc, err := b.Repo.ReadCommit(src)
	if err != nil {
		return res, err
	}
	if res.Files, err = b.diffTrees(fromTree, sc.Tree); err != nil {
		return res, err
	}
	res.Empty = len(res.Files) == 0
	return res, nil
}

func (b *Buffer) diffTrees(from, to gitrepo.Hash) ([]FileChange, error) {
	a, err := b.Repo.TreeEntries(from)
	if err != nil {
		return nil, err
	}
	c, err := b.Repo.TreeEntries(to)
	if err != nil {
		return nil, err
	}
	changes := []FileChange{}
	for p, e := range c {
		if old, ok := a[p]; !ok {
			changes = append(changes, FileChange{p, "A"})
		} else if old != e {
			changes = append(changes, FileChange{p, "M"})
		}
	}
	for p := range a {
		if _, ok := c[p]; !ok {
			changes = append(changes, FileChange{p, "D"})
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes, nil
}

// PrepareResult is the drift check a consumer runs before applying.
type PrepareResult struct {
	Consumer   string            `json:"consumer"`
	Policy     DriftPolicy       `json:"policy"`
	Target     string            `json:"target"`
	Drift      []EditResult      `json:"drift"`
	Integrated []IntegrateResult `json:"integrated,omitempty"`
	Blocked    bool              `json:"blocked"`
	Reason     string            `json:"reason,omitempty"`
}

// Prepare captures every adapter the consumer writes, so nothing live is
// lost, then applies the consumer's drift policy. Target is the commit to
// apply. Consumers never apply blind: `confit apply` always runs this first, and
// external consumers (home-manager) run `confit prepare` before switching.
func (b *Buffer) Prepare(consumer string) (PrepareResult, error) {
	unlock, err := b.Repo.Lock()
	if err != nil {
		return PrepareResult{}, err
	}
	defer unlock()
	return b.prepare(consumer)
}

func (b *Buffer) prepare(consumer string) (PrepareResult, error) {
	cs, err := b.consumer(consumer)
	if err != nil {
		return PrepareResult{}, err
	}
	res := PrepareResult{Consumer: consumer, Policy: cs.Drift, Drift: []EditResult{}}
	for _, name := range cs.Adapters {
		c, err := b.capture(name, false)
		if err != nil {
			return res, fmt.Errorf("capture %s: %w", name, err)
		}
		if c.Edit.Changed {
			res.Drift = append(res.Drift, c.Edit)
		}
	}
	switch cs.Drift {
	case Adopt:
		for _, name := range cs.Adapters {
			editor := b.Config.Adapters[name].Editor
			if tip, err := b.Repo.Resolve(b.Config.editRef(editor)); err != nil {
				return res, err
			} else if tip.IsZero() {
				continue // nothing captured yet, so no drift to adopt
			}
			ir, err := b.integrate(editor)
			if err != nil {
				return res, err
			}
			res.Integrated = append(res.Integrated, ir)
			if ir.Status == Conflict {
				res.Blocked = true
				res.Reason = fmt.Sprintf("live changes from %s conflict with %s in %s; resolve with git, then retry",
					name, b.Config.Integration, strings.Join(ir.Conflicts, ", "))
			}
		}
		if cs.Source != b.Config.Integration && len(res.Drift) > 0 {
			res.Reason = "drift was integrated into " + b.Config.Integration + ", but this consumer applies " + cs.Source + ", so this apply reverts it"
		}
	case Block:
		unaccounted, err := b.unaccountedDrift(cs)
		if err != nil {
			return res, err
		}
		if len(unaccounted) > 0 {
			res.Blocked = true
			res.Reason = "live state differs from what was last applied in " + strings.Join(unaccounted, ", ") +
				"; integrate the editor branch to keep it, or apply with --force to discard it"
		}
	}
	src, err := b.Repo.Resolve(b.Config.sourceRef(cs))
	if err != nil {
		return res, err
	}
	if src.IsZero() {
		return res, fmt.Errorf("source branch %s does not exist", cs.Source)
	}
	res.Target = src.String()
	return res, nil
}

// unaccountedDrift names the consumer's adapters whose last observed live
// state differs from what the consumer last applied (or, before any apply,
// from what it is about to apply) and that nobody has integrated into the
// consumer's source.
func (b *Buffer) unaccountedDrift(cs Consumer) ([]string, error) {
	src, err := b.Repo.Resolve(b.Config.sourceRef(cs))
	if err != nil {
		return nil, err
	}
	baseline, err := b.Repo.Resolve(b.Config.appliedRef(cs.Name))
	if err != nil {
		return nil, err
	}
	if baseline.IsZero() {
		baseline = src
	}
	bc, err := b.Repo.ReadCommit(baseline)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, name := range cs.Adapters {
		a := b.Config.Adapters[name]
		tip, err := b.Repo.Resolve(b.Config.editRef(a.Editor))
		if err != nil {
			return nil, err
		}
		if tip.IsZero() {
			continue
		}
		tc, err := b.Repo.ReadCommit(tip)
		if err != nil {
			return nil, err
		}
		if eq, err := b.Repo.SubtreeEqual(tc.Tree, bc.Tree, a.Path); err != nil {
			return nil, err
		} else if eq {
			continue
		}
		// Drift someone chose to keep by integrating it is accounted for:
		// the source already contains it, so the apply reproduces it.
		if kept, err := b.Repo.IsAncestor(tip, src); err != nil {
			return nil, err
		} else if !kept {
			names = append(names, name)
		}
	}
	return names, nil
}

// ApplyResult is `confit apply`.
type ApplyResult struct {
	Prepare PrepareResult `json:"prepare"`
	Applied string        `json:"applied,omitempty"`
}

// ErrBlocked is returned when the drift policy refuses an apply.
var ErrBlocked = errors.New("apply blocked by drift policy")

// Apply runs the drift check, writes the target tree to every adapter the
// consumer owns, and only then advances applied/<consumer>. A failure leaves
// the pointer where it was.
func (b *Buffer) Apply(consumer string, force bool) (ApplyResult, error) {
	unlock, err := b.Repo.Lock()
	if err != nil {
		return ApplyResult{}, err
	}
	defer unlock()
	cs, err := b.consumer(consumer)
	if err != nil {
		return ApplyResult{}, err
	}
	if len(cs.Adapters) == 0 {
		return ApplyResult{}, fmt.Errorf("consumer %s has no adapters; external consumers apply themselves and then run confit applied advance", consumer)
	}
	var res ApplyResult
	if res.Prepare, err = b.prepare(consumer); err != nil {
		return res, err
	}
	if res.Prepare.Blocked && !force {
		return res, fmt.Errorf("%w: %s", ErrBlocked, res.Prepare.Reason)
	}
	target := gitrepo.NewHash(res.Prepare.Target)
	tc, err := b.Repo.ReadCommit(target)
	if err != nil {
		return res, err
	}
	for _, name := range cs.Adapters {
		cfg, a, err := b.adapter(name)
		if err != nil {
			return res, err
		}
		files, err := b.Repo.ReadFiles(tc.Tree, cfg.Path)
		if err != nil {
			return res, err
		}
		if err := a.Apply(files); err != nil {
			return res, fmt.Errorf("apply %s: %w", name, err)
		}
	}
	old, err := b.Repo.Resolve(b.Config.appliedRef(consumer))
	if err != nil {
		return res, err
	}
	if err := b.advance(cs, target, old, "ok"); err != nil {
		return res, err
	}
	res.Applied = target.String()
	return res, nil
}

// Advance is `confit applied advance`: an external consumer reports that it
// applied commit. expect, when non-zero, is the value applied/<consumer>
// must have now (compare-and-swap).
func (b *Buffer) Advance(consumer, commit, expect, result string) (string, error) {
	unlock, err := b.Repo.Lock()
	if err != nil {
		return "", err
	}
	defer unlock()
	cs, err := b.consumer(consumer)
	if err != nil {
		return "", err
	}
	h, err := b.Repo.ResolveRev(commit)
	if err != nil {
		return "", err
	}
	old, err := b.Repo.Resolve(b.Config.appliedRef(consumer))
	if err != nil {
		return "", err
	}
	if expect != "" {
		if old, err = b.Repo.ResolveRev(expect); err != nil {
			return "", err
		}
	}
	return h.String(), b.advance(cs, h, old, or(result, "ok"))
}

func (b *Buffer) advance(cs Consumer, commit, old gitrepo.Hash, result string) error {
	src, err := b.Repo.Resolve(b.Config.sourceRef(cs))
	if err != nil {
		return err
	}
	if ok, err := b.Repo.IsAncestor(commit, src); err != nil {
		return err
	} else if src.IsZero() || !ok {
		return fmt.Errorf("%s is not in the history of %s; consumers may only apply commits from their source", short(commit), cs.Source)
	}
	if old == commit {
		// Nothing to move, but the pointer must really be where expected.
		if cur, err := b.Repo.Resolve(b.Config.appliedRef(cs.Name)); err != nil {
			return err
		} else if cur != old {
			return fmt.Errorf("%w: %s%s is at %s", gitrepo.ErrRefChanged, b.Config.AppliedPrefix, cs.Name, short(cur))
		}
	} else {
		err = b.Repo.UpdateRefs("confit applied advance "+cs.Name, gitrepo.RefUpdate{Name: b.Config.appliedRef(cs.Name), New: commit, Old: old})
		if err != nil {
			return err
		}
	}
	note := trailers("Consumer", cs.Name, "Applied-At", b.now().UTC().Format("2006-01-02T15:04:05Z"),
		"Result", result, "Host", b.Config.Host, "Tool", "confit/"+Version)
	if err := b.Repo.AppendNote(notesRef, commit, note, b.committer("apply")); err != nil {
		return fmt.Errorf("applied pointer moved, but writing the apply note failed: %w", err)
	}
	return b.syncEditors(cs, commit)
}

// syncEditors records on each adapter's editor branch that the live state
// now equals the applied commit, so the next capture compares against what
// is really live. The applied commit becomes a parent, which keeps later
// merges' base correct.
func (b *Buffer) syncEditors(cs Consumer, applied gitrepo.Hash) error {
	ac, err := b.Repo.ReadCommit(applied)
	if err != nil {
		return err
	}
	for _, name := range cs.Adapters {
		a := b.Config.Adapters[name]
		ref := b.Config.editRef(a.Editor)
		for attempt := 0; ; attempt++ {
			tip, err := b.Repo.Resolve(ref)
			if err != nil {
				return err
			}
			var next gitrepo.Hash
			if tip.IsZero() {
				next = applied
			} else {
				tc, err := b.Repo.ReadCommit(tip)
				if err != nil {
					return err
				}
				eq, err := b.Repo.SubtreeEqual(tc.Tree, ac.Tree, a.Path)
				if err != nil {
					return err
				}
				// Skip only when history already connects the two. Equal
				// content alone isn't enough: an editor branch that never
				// shared history with the applied commit (captured before
				// the first apply, under a non-adopt policy) would later
				// merge with the root as its base and conflict on its own
				// unchanged files.
				if eq {
					linked, err := b.Repo.IsAncestor(tip, applied)
					if err == nil && !linked {
						linked, err = b.Repo.IsAncestor(applied, tip)
					}
					if err != nil {
						return err
					}
					if linked {
						break
					}
				}
				// The applied tree is the right merge result for every path:
				// the adapter's subtree is now live, and the editor never
				// changes anything outside it.
				tree := ac.Tree
				msg := fmt.Sprintf("%s: live state is now %s, applied by %s\n\n", name, short(applied), cs.Name) +
					trailers("Confit-Editor", a.Editor, "Confit-Applied-By", cs.Name)
				if next, err = b.Repo.WriteCommit(gitrepo.Commit{Tree: tree, Parents: []gitrepo.Hash{tip, applied},
					Author: b.human(), Committer: b.committer("apply"), Message: msg}); err != nil {
					return err
				}
			}
			err = b.Repo.UpdateRefs("confit sync "+a.Editor, gitrepo.RefUpdate{Name: ref, New: next, Old: tip})
			if errors.Is(err, gitrepo.ErrRefChanged) && attempt < 5 {
				continue
			}
			if err != nil {
				return err
			}
			break
		}
	}
	return nil
}
