package core

import (
	"sort"
)

// EditorStatus is one editor branch relative to the integration branch.
type EditorStatus struct {
	Editor string `json:"editor"`
	Tip    string `json:"tip"`
	// Unintegrated counts commits not yet in the integration branch.
	Unintegrated int `json:"unintegrated"`
	// Changes is false when those commits would not change the integration
	// branch's content (for example after a reverted drift).
	Changes   bool     `json:"changes"`
	Conflicts []string `json:"conflicts,omitempty"`
}

// ConsumerStatus is one consumer's pointer and what it is waiting on.
type ConsumerStatus struct {
	Consumer string      `json:"consumer"`
	Source   string      `json:"source"`
	Policy   DriftPolicy `json:"policy"`
	Applied  string      `json:"applied,omitempty"`
	Pending  int         `json:"pendingCommits"`
	Files    int         `json:"pendingFiles"`
	// Drift names adapters whose last observed live state differs from
	// what this consumer last applied. It reflects the last capture.
	Drift []string `json:"drift"`
}

// StatusResult is `gb status`.
type StatusResult struct {
	Integration string           `json:"integration"`
	Tip         string           `json:"tip"`
	Editors     []EditorStatus   `json:"editors"`
	Consumers   []ConsumerStatus `json:"consumers"`
}

// Status reports unintegrated editors, conflicts, pending work and recorded
// drift. It reads only; run `gb capture` first for up-to-the-moment drift.
func (b *Buffer) Status() (StatusResult, error) {
	tip, err := b.integrationTip()
	if err != nil {
		return StatusResult{}, err
	}
	res := StatusResult{Integration: b.Config.Integration, Tip: tip.String(), Editors: []EditorStatus{}, Consumers: []ConsumerStatus{}}
	editors, err := b.editors()
	if err != nil {
		return res, err
	}
	tc, err := b.Repo.ReadCommit(tip)
	if err != nil {
		return res, err
	}
	for _, e := range editors {
		et, err := b.Repo.Resolve(b.Config.editRef(e))
		if err != nil {
			return res, err
		}
		commits, err := b.Repo.RevList(et.String(), "^"+tip.String())
		if err != nil {
			return res, err
		}
		es := EditorStatus{Editor: e, Tip: et.String(), Unintegrated: len(commits)}
		if len(commits) > 0 {
			m, err := b.Repo.MergeTree(tip, et, b.Config.integrationRef())
			if err != nil {
				return res, err
			}
			es.Changes = m.Tree != tc.Tree || !m.Clean
			es.Conflicts = m.Conflicts
		}
		res.Editors = append(res.Editors, es)
	}
	// Configured consumers plus any applied/* pointer for an unconfigured one.
	names := map[string]bool{}
	for n := range b.Config.Consumers {
		names[n] = true
	}
	applied, err := b.Repo.ListRefs("refs/heads/" + b.Config.AppliedPrefix)
	if err != nil {
		return res, err
	}
	for ref := range applied {
		names[ref[len("refs/heads/"+b.Config.AppliedPrefix):]] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	for _, n := range sorted {
		cs, _ := b.consumer(n)
		p, err := b.Pending(n)
		if err != nil {
			return res, err
		}
		st := ConsumerStatus{Consumer: n, Source: cs.Source, Policy: cs.Drift, Applied: p.Applied,
			Pending: len(p.Commits), Files: len(p.Files), Drift: []string{}}
		if p.Applied != "" {
			if st.Drift, err = b.unaccountedDrift(cs); err != nil {
				return res, err
			}
			if st.Drift == nil {
				st.Drift = []string{}
			}
		}
		res.Consumers = append(res.Consumers, st)
	}
	return res, nil
}
