package core

import (
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
	editor    string
	prefix    string
	files     map[string][]byte
	label     string // summary prefix: adapter or editor name
	component string // committer component, e.g. "dconf"
	source    string // Confit-Source trailer
	session   string
	message   string              // overrides the generated summary line
	keyName   func(string) string // renders changed keys for messages
	base      func() (gitrepo.Hash, error)
}

// commitEdit replaces prefix on the editor branch with files. When the
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
		pc, err := b.Repo.ReadCommit(parent)
		if err != nil {
			return res, err
		}
		tree, err := b.Repo.ReplaceSubtree(pc.Tree, req.prefix, req.files)
		if err != nil {
			return res, err
		}
		if tree == pc.Tree {
			return res, nil
		}
		before, err := b.Repo.ReadFiles(pc.Tree, req.prefix)
		if err != nil {
			return res, err
		}
		keys, summary := describe(req, before, req.files)
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
	files, err := a.Snapshot()
	if err != nil {
		return res, err
	}
	req := editRequest{
		editor: cfg.Editor, prefix: cfg.Path, files: files, label: name,
		component: cfg.Type, source: "unattributed", base: b.editorBase(name),
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
		editor: editor, prefix: opts.Prefix, files: opts.Files, label: editor,
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

// describe produces the changed keys and a one-line summary, e.g.
// "vscode: set editor.fontSize = 14".
func describe(req editRequest, before, after map[string][]byte) ([]string, string) {
	paths := map[string]bool{}
	for p := range before {
		paths[p] = true
	}
	for p := range after {
		paths[p] = true
	}
	var keys []string
	values := map[string]string{}
	removed := map[string]bool{}
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
					keys = append(keys, k)
					if v, ok := vs[k]; ok {
						j, _ := json.Marshal(v)
						values[k] = string(j)
					} else {
						removed[k] = true
					}
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
					keys = append(keys, name)
					if v, ok := vs[k]; ok {
						values[name] = v
					} else {
						removed[name] = true
					}
				}
				continue
			}
		}
		keys = append(keys, p)
		if nw == nil {
			removed[p] = true
		}
	}
	sort.Strings(keys)
	label := req.label
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
