package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/UnstoppableMango/confit/internal/adapter"
	"github.com/UnstoppableMango/confit/internal/gitrepo"
	"github.com/UnstoppableMango/confit/internal/mergedriver"
)

// EditResult describes one commit (or no-op) on an editor branch.
type EditResult struct {
	Editor  string   `json:"editor"`
	Branch  string   `json:"branch"`
	Commit  string   `json:"commit,omitempty"`
	Changed bool     `json:"changed"`
	Summary string   `json:"summary,omitempty"`
	Keys    []string `json:"keys,omitempty"`
}

// CaptureResult is the outcome of `confit capture`.
type CaptureResult struct {
	Adapter    string           `json:"adapter"`
	Edit       EditResult       `json:"edit"`
	Integrated *IntegrateResult `json:"integrated,omitempty"`
}

type editRequest struct {
	editor string
	// build returns the new contents of each prefix it replaces, given the
	// tree the edit is based on.
	build     func(tree gitrepo.Hash) (map[string]map[string][]byte, error)
	label     string // summary prefix: adapter or editor name
	component string // committer component, e.g. "dconf"
	source    string // Confit-Source trailer
	session   string
	message   string              // overrides the generated summary line
	keyName   func(string) string // renders changed keys for messages
	base      func() (gitrepo.Hash, error)
	// onto, when set, may move the parent forward to the integration tip
	// (docs/multi-host.md, decision 3).
	onto func(parent gitrepo.Hash) (gitrepo.Hash, error)
}

// commitEdit replaces each prefix req.build returns on the editor branch. When the
// result equals the branch tip's tree it does nothing: identical trees mean
// no drift, so no diffing is needed. Caller holds the repo lock.
func (b *Buffer) commitEdit(req editRequest) (EditResult, error) {
	ref := b.Config.editRef(req.editor)
	res := EditResult{Editor: req.editor, Branch: strings.TrimPrefix(ref, "refs/heads/")}
	for attempt := 0; attempt < 5; attempt++ {
		tip, err := b.Repo.Resolve(ref)
		if err != nil {
			return res, err
		}
		parent := tip
		if parent.IsZero() {
			if parent, err = req.base(); err != nil {
				return res, err
			}
		}
		if req.onto != nil {
			if parent, err = req.onto(parent); err != nil {
				return res, err
			}
		}
		pc, err := b.Repo.ReadCommit(parent)
		if err != nil {
			return res, err
		}
		layers, err := req.build(pc.Tree)
		if err != nil {
			return res, err
		}
		tree := pc.Tree
		var changes []keyChange
		for _, prefix := range sortedKeys(layers) {
			before, err := b.Repo.ReadFiles(pc.Tree, prefix)
			if err != nil {
				return res, err
			}
			files := keepEquivalent(before, layers[prefix])
			if tree, err = b.Repo.ReplaceSubtree(tree, prefix, files); err != nil {
				return res, err
			}
			changes = append(changes, changedKeys(req, before, files)...)
		}
		if tree == pc.Tree {
			return res, nil
		}
		keys, summary := describe(req.label, changes)
		if req.message != "" {
			summary = req.message
		}
		msg := summary + "\n\n" + trailers(
			"Confit-Editor", req.editor,
			"Confit-Adapter", req.component+"/"+Version,
			"Confit-Source", req.source,
			"Confit-Session", req.session,
			"Confit-Keys", joinKeys(keys),
		)
		c, err := b.Repo.WriteCommit(gitrepo.Commit{Tree: tree, Parents: []gitrepo.Hash{parent},
			Author: b.human(), Committer: b.committer(req.component), Message: msg})
		if err != nil {
			return res, err
		}
		err = b.Repo.UpdateRefs("confit: "+summary, gitrepo.RefUpdate{Name: ref, New: c, Old: tip})
		if errors.Is(err, gitrepo.ErrRefChanged) {
			continue
		}
		if err != nil {
			return res, err
		}
		res.Commit, res.Changed, res.Summary, res.Keys = c.String(), true, summary, keys
		return res, nil
	}
	return res, fmt.Errorf("%s kept changing; giving up", ref)
}

// keepEquivalent keeps the committed bytes of any INI file whose keys and
// values didn't change. A snapshot serializes dconf in its own order, so
// without this a hand-edited file in git (keys in another order) would come
// back from the next capture as a no-op "update" commit.
func keepEquivalent(before, after map[string][]byte) map[string][]byte {
	out := make(map[string][]byte, len(after))
	for p, data := range after {
		out[p] = data
		old, ok := before[p]
		if !ok || !strings.HasSuffix(p, ".ini") || bytes.Equal(old, data) {
			continue
		}
		if keys, _, err := mergedriver.INIKeys(old, data); err == nil && len(keys) == 0 {
			out[p] = old
		}
	}
	return out
}

// editorBase is where a new editor branch for an adapter starts: the last
// commit a consumer applied to that system, since that is what the live
// state should match. Without one, it starts from the integration branch's
// root, so a first capture of unrelated live state can't silently replace
// intent that is in git but not applied yet: the two meet as a merge.
func (b *Buffer) editorBase(adapterName string) func() (gitrepo.Hash, error) {
	return func() (gitrepo.Hash, error) {
		var names []string
		for name, cs := range b.Config.Consumers {
			for _, a := range cs.Adapters {
				if a == adapterName {
					names = append(names, name)
				}
			}
		}
		sort.Strings(names)
		for _, name := range names {
			h, err := b.Repo.Resolve(b.Config.appliedRef(name))
			if err != nil || !h.IsZero() {
				return h, err
			}
		}
		tip, err := b.integrationTip()
		if err != nil {
			return tip, err
		}
		return b.Repo.RootCommit(tip)
	}
}

// ontoIntegration starts a capture from the integration tip instead of the
// editor's own tip when nothing is lost by it: the editor's history is all
// integrated and its layers already match. The capture then integrates as a
// fast-forward, one commit per change, and commits on the integration branch
// that don't touch this adapter (manifest edits, other adapters) don't force
// a merge.
func (b *Buffer) ontoIntegration(cfg adapter.Config) func(gitrepo.Hash) (gitrepo.Hash, error) {
	return func(parent gitrepo.Hash) (gitrepo.Hash, error) {
		tip, err := b.integrationTip()
		if err != nil || tip == parent {
			return parent, err
		}
		if ok, err := b.Repo.IsAncestor(parent, tip); err != nil || !ok {
			return parent, err
		}
		pc, err := b.Repo.ReadCommit(parent)
		if err != nil {
			return parent, err
		}
		tc, err := b.Repo.ReadCommit(tip)
		if err != nil {
			return parent, err
		}
		if eq, err := b.layersEqual(pc.Tree, tc.Tree, cfg); err != nil || !eq {
			return parent, err
		}
		return tip, nil
	}
}

func (b *Buffer) integrationTip() (gitrepo.Hash, error) {
	tip, err := b.Repo.Resolve(b.Config.integrationRef())
	if err == nil && tip.IsZero() {
		err = fmt.Errorf("branch %s does not exist; run confit init", b.Config.Integration)
	}
	return tip, err
}

// Capture snapshots an adapter's live state, commits any difference to its
// editor branch, and integrates it unless the adapter is set not to.
func (b *Buffer) Capture(name string, integrate bool) (CaptureResult, error) {
	unlock, err := b.Repo.Lock()
	if err != nil {
		return CaptureResult{}, err
	}
	defer unlock()
	if err := b.refresh(); err != nil {
		return CaptureResult{}, err
	}
	cfg, _, err := b.adapter(name)
	if err != nil {
		return CaptureResult{}, err
	}
	return b.capture(name, integrate && cfg.Integrate)
}

func (b *Buffer) capture(name string, integrate bool) (CaptureResult, error) {
	res := CaptureResult{Adapter: name}
	cfg, a, err := b.adapter(name)
	if err != nil {
		return res, err
	}
	liveFiles, err := a.Snapshot()
	if err != nil {
		return res, err
	}
	t := b.Config.Tracking[name]
	live, err := t.unitize(liveFiles)
	if err != nil {
		return res, err
	}
	// "shared" tracking follows the integration branch's layers, which an
	// editor branch based on an older commit may not have yet.
	tip, err := b.integrationTip()
	if err != nil {
		return res, err
	}
	tc, err := b.Repo.ReadCommit(tip)
	if err != nil {
		return res, err
	}
	integrated, err := b.readLayers(cfg, tc.Tree)
	if err != nil {
		return res, err
	}
	known := union(integrated.sharedUnits, integrated.hostUnits)
	// Each tracked unit goes to the layer it belongs to; untracked ones,
	// live or committed, are left alone.
	build := func(tree gitrepo.Hash) (map[string]map[string][]byte, error) {
		l, err := b.readLayers(cfg, tree)
		if err != nil {
			return nil, err
		}
		shared, host := t.route(live, l.sharedUnits, l.hostUnits, known)
		out := map[string]map[string][]byte{}
		if out[cfg.Path], err = t.compose(shared, []map[string][]byte{liveFiles, l.shared}, l.shared, liveFiles); err != nil {
			return nil, err
		}
		if out[b.Config.hostPath(cfg)], err = t.compose(host, []map[string][]byte{liveFiles, l.host}, l.host, liveFiles); err != nil {
			return nil, err
		}
		return out, nil
	}
	req := editRequest{
		editor: cfg.Editor, build: build, label: name,
		component: cfg.Type, source: "unattributed", base: b.editorBase(name),
		onto: b.ontoIntegration(cfg),
	}
	if d, ok := a.(*adapter.Dconf); ok {
		req.keyName = d.KeyPath
	}
	if res.Edit, err = b.commitEdit(req); err != nil {
		return res, err
	}
	if integrate && res.Edit.Changed {
		ir, err := b.integrate(cfg.Editor)
		res.Integrated = &ir
		if err != nil {
			return res, err
		}
	}
	return res, nil
}

// EditOptions is `confit edit commit`: an editor hands confit the full content of
// its directory and confit records the difference.
type EditOptions struct {
	Prefix    string
	Files     map[string][]byte
	Message   string
	Session   string
	Integrate bool
}

// EditCommit is the generic editor entry point used by UIs and adapters
// that write their own files (for example a VSCode extension).
func (b *Buffer) EditCommit(editor string, opts EditOptions) (CaptureResult, error) {
	if strings.Trim(opts.Prefix, "/") == "" {
		return CaptureResult{}, fmt.Errorf("edit commit needs a prefix; without one the edit would replace the whole tree")
	}
	unlock, err := b.Repo.Lock()
	if err != nil {
		return CaptureResult{}, err
	}
	defer unlock()
	res := CaptureResult{}
	res.Edit, err = b.commitEdit(editRequest{
		editor: editor, label: editor,
		build: func(gitrepo.Hash) (map[string]map[string][]byte, error) {
			return map[string]map[string][]byte{strings.Trim(opts.Prefix, "/"): opts.Files}, nil
		},
		component: "edit", session: opts.Session, message: opts.Message,
		base: b.integrationTip,
	})
	if err != nil || !opts.Integrate || !res.Edit.Changed {
		return res, err
	}
	ir, err := b.integrate(editor)
	res.Integrated = &ir
	return res, err
}

// keyChange is one changed key; value is empty for opaque files.
type keyChange struct {
	key, value string
	removed    bool
}

// changedKeys lists the keys that differ between two versions of an
// adapter's files.
func changedKeys(req editRequest, before, after map[string][]byte) []keyChange {
	paths := map[string]bool{}
	for p := range before {
		paths[p] = true
	}
	for p := range after {
		paths[p] = true
	}
	var keys []keyChange
	for p := range paths {
		old, nw := before[p], after[p]
		if string(old) == string(nw) && (old == nil) == (nw == nil) {
			continue
		}
		switch {
		case strings.HasSuffix(p, ".json"):
			ks, vs, err := mergedriver.JSONKeys(old, nw)
			if err == nil {
				for _, k := range ks {
					j, _ := json.Marshal(vs[k])
					_, ok := vs[k]
					keys = append(keys, keyChange{k, string(j), !ok})
				}
				continue
			}
		case strings.HasSuffix(p, ".ini"):
			ks, vs, err := mergedriver.INIKeys(old, nw)
			if err == nil {
				for _, k := range ks {
					name := k
					if req.keyName != nil {
						name = req.keyName(k)
					}
					v, ok := vs[k]
					keys = append(keys, keyChange{name, v, !ok})
				}
				continue
			}
		}
		keys = append(keys, keyChange{key: p, removed: nw == nil})
	}
	return keys
}

// describe produces the changed key names and a one-line summary, e.g.
// "vscode: set editor.fontSize = 14", from changedKeys output.
func describe(label string, changes []keyChange) ([]string, string) {
	values := map[string]string{}
	removed := map[string]bool{}
	seen := map[string]bool{}
	var keys []string
	for _, c := range changes {
		if !seen[c.key] {
			seen[c.key] = true
			keys = append(keys, c.key)
		}
		if !c.removed {
			values[c.key], removed[c.key] = c.value, false
		} else if _, set := values[c.key]; !set {
			removed[c.key] = true
		}
	}
	sort.Strings(keys)
	switch {
	case len(keys) == 0:
		return keys, label + ": update"
	case len(keys) == 1 && removed[keys[0]]:
		return keys, label + ": unset " + keys[0]
	case len(keys) == 1 && values[keys[0]] != "":
		v := values[keys[0]]
		if len(v) > 60 {
			v = v[:57] + "..."
		}
		return keys, fmt.Sprintf("%s: set %s = %s", label, keys[0], v)
	case len(keys) == 1:
		return keys, label + ": update " + keys[0]
	default:
		return keys, fmt.Sprintf("%s: %d changes (%s)", label, len(keys), joinKeys(keys))
	}
}

func joinKeys(keys []string) string {
	if len(keys) > 8 {
		return strings.Join(keys[:8], ", ") + fmt.Sprintf(", +%d more", len(keys)-8)
	}
	return strings.Join(keys, ", ")
}
