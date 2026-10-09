package core

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/UnstoppableMango/confit/internal/gitrepo"
)

// Integration outcomes.
const (
	UpToDate    = "up-to-date"
	FastForward = "fast-forward"
	Merged      = "merged"
	Conflict    = "conflict"
)

// IntegrateResult is what happened to one editor branch.
type IntegrateResult struct {
	Editor    string   `json:"editor"`
	Status    string   `json:"status"`
	Commit    string   `json:"commit,omitempty"`
	Conflicts []string `json:"conflicts,omitempty"`
}

// Integrate merges editor branches (all of them when none are named) into
// the integration branch. A conflicting editor is reported and left
// unintegrated; it never blocks the others.
func (b *Buffer) Integrate(editors ...string) ([]IntegrateResult, error) {
	unlock, err := b.Repo.Lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	if len(editors) == 0 {
		if editors, err = b.editors(); err != nil {
			return nil, err
		}
	}
	var results []IntegrateResult
	var errs []error
	for _, e := range editors {
		r, err := b.integrate(e)
		results = append(results, r)
		errs = append(errs, err)
	}
	return results, errors.Join(errs...)
}

// editors lists editor names that have a branch.
func (b *Buffer) editors() ([]string, error) {
	prefix := "refs/heads/" + b.Config.EditsPrefix
	refs, err := b.Repo.ListRefs(prefix)
	if err != nil {
		return nil, err
	}
	var names []string
	for name := range refs {
		names = append(names, strings.TrimPrefix(name, prefix))
	}
	sort.Strings(names)
	return names, nil
}

// integrate does one editor; caller holds the lock. The integration branch
// only moves by compare-and-swap, so a concurrent writer outside confit (a person
// running git) makes us retry rather than lose their commit.
func (b *Buffer) integrate(editor string) (IntegrateResult, error) {
	res := IntegrateResult{Editor: editor}
	for attempt := 0; attempt < 5; attempt++ {
		tip, err := b.Repo.Resolve(b.Config.editRef(editor))
		if err != nil {
			return res, err
		}
		if tip.IsZero() {
			return res, fmt.Errorf("no editor branch %s%s", b.Config.EditsPrefix, editor)
		}
		target, err := b.integrationTip()
		if err != nil {
			return res, err
		}
		if in, err := b.Repo.IsAncestor(tip, target); err != nil || in {
			res.Status, res.Commit = UpToDate, target.String()
			return res, err
		}
		var next gitrepo.Hash
		if ff, err := b.Repo.IsAncestor(target, tip); err != nil {
			return res, err
		} else if ff {
			res.Status, next = FastForward, tip
		} else {
			m, err := b.Repo.MergeTree(target, tip, b.Config.integrationRef())
			if err != nil {
				return res, err
			}
			if !m.Clean {
				res.Status, res.Conflicts = Conflict, m.Conflicts
				return res, nil
			}
			msg := fmt.Sprintf("Integrate %s%s into %s\n\n", b.Config.EditsPrefix, editor, b.Config.Integration) +
				trailers("Confit-Integrated", editor)
			next, err = b.Repo.WriteCommit(gitrepo.Commit{Tree: m.Tree, Parents: []gitrepo.Hash{target, tip},
				Author: b.human(), Committer: b.committer("integrate"), Message: msg})
			if err != nil {
				return res, err
			}
			res.Status = Merged
		}
		err = b.Repo.UpdateRefs("confit integrate "+editor, gitrepo.RefUpdate{Name: b.Config.integrationRef(), New: next, Old: target})
		if errors.Is(err, gitrepo.ErrRefChanged) {
			continue
		}
		if err != nil {
			return res, err
		}
		res.Commit = next.String()
		return res, nil
	}
	return res, fmt.Errorf("%s kept changing while integrating %s", b.Config.Integration, editor)
}
