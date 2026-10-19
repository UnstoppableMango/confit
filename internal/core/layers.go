package core

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/UnstoppableMango/confit/internal/adapter"
	"github.com/UnstoppableMango/confit/internal/gitrepo"
	"github.com/UnstoppableMango/confit/internal/mergedriver"
)

// Layers (docs/multi-host.md, decisions 12, 13 and 15). An adapter's files
// are split into units, the smallest thing tracking and layering act on:
//
//   - dconf: one key, named by its full path ("/org/gnome/desktop/interface/gtk-theme");
//   - a JSON file: one top-level key ("settings.json:editor.fontSize"), plus
//     the file itself ("settings.json", empty value) so an empty file survives;
//   - an INI file: one key ("app.ini:section/key"), plus the file itself;
//   - any other file: the whole file.
//
// A unit's value is what identifies it as changed: the raw dconf value,
// compact JSON, or the file's bytes.
type units map[string]string

// unitize splits an adapter's files into units.
func (t Tracking) unitize(files map[string][]byte) (units, error) {
	u := units{}
	for name, data := range files {
		if err := t.unitizeFile(u, name, data); err != nil {
			return nil, err
		}
	}
	return u, nil
}

func (t Tracking) unitizeFile(u units, name string, data []byte) error {
	switch {
	case t.DconfRoot != "":
		ini, err := mergedriver.ParseINI(data)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		for s, kv := range ini {
			for k, v := range kv {
				u[dconfKey(t.DconfRoot, s, k)] = v
			}
		}
	case strings.HasSuffix(name, ".json"):
		m, err := mergedriver.DecodeJSONC(data)
		if err != nil {
			return fmt.Errorf("%s is not a JSON object: %w", name, err)
		}
		u[name] = ""
		for k, v := range m {
			j, err := json.Marshal(v)
			if err != nil {
				return err
			}
			u[name+":"+k] = string(j)
		}
	case strings.HasSuffix(name, ".ini"):
		ini, err := mergedriver.ParseINI(data)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		u[name] = ""
		for s, kv := range ini {
			for k, v := range kv {
				u[name+":"+s+"/"+k] = v
			}
		}
	default:
		u[name] = string(data)
	}
	return nil
}

func dconfKey(root, section, key string) string {
	if section == "/" || section == "" {
		return root + key
	}
	return root + strings.Trim(section, "/") + "/" + key
}

// fileOf is the file a unit belongs to, and the key within it ("" for the
// file itself).
func (t Tracking) fileOf(unit string) (file, key string) {
	if t.DconfRoot != "" {
		rel := strings.TrimPrefix(unit, t.DconfRoot)
		i := strings.LastIndex(rel, "/")
		if i < 0 {
			return dconfFile("/"), unit
		}
		return dconfFile(rel[:i]), unit
	}
	if f, k, ok := strings.Cut(unit, ":"); ok && (strings.HasSuffix(f, ".json") || strings.HasSuffix(f, ".ini")) {
		return f, k
	}
	return unit, ""
}

// dconfFile mirrors adapter.sectionFile: one file per dconf directory.
func dconfFile(section string) string {
	if section == "/" || section == "" {
		return "_root.ini"
	}
	return strings.Trim(section, "/") + ".ini"
}

// compose turns units back into files. A file whose units equal its copy in
// one of the verbatim candidates keeps that copy's bytes, so formatting and
// comments survive. Otherwise JSON is patched from the first of patch that
// parses (keeping its comments), and everything else is rebuilt in the
// stable form.
func (t Tracking) compose(u units, verbatim []map[string][]byte, patch ...map[string][]byte) (map[string][]byte, error) {
	byFile := map[string]units{}
	for name, v := range u {
		f, _ := t.fileOf(name)
		if byFile[f] == nil {
			byFile[f] = units{}
		}
		byFile[f][name] = v
	}
	out := map[string][]byte{}
	for f, want := range byFile {
		data, err := t.composeFile(f, want, verbatim, patch)
		if err != nil {
			return nil, err
		}
		out[f] = data
	}
	return out, nil
}

func (t Tracking) composeFile(f string, want units, verbatim, patch []map[string][]byte) ([]byte, error) {
	for _, c := range verbatim {
		data, ok := c[f]
		if !ok {
			continue
		}
		have := units{}
		if t.unitizeFile(have, f, data) == nil && equalUnits(have, want) {
			return data, nil
		}
	}
	switch {
	case t.DconfRoot != "":
		ini := mergedriver.INI{}
		for name, v := range want {
			rel := strings.TrimPrefix(name, t.DconfRoot)
			section, key := "/", rel
			if i := strings.LastIndex(rel, "/"); i >= 0 {
				section, key = rel[:i], rel[i+1:]
			}
			if ini[section] == nil {
				ini[section] = map[string]string{}
			}
			ini[section][key] = v
		}
		return ini.Bytes(), nil
	case strings.HasSuffix(f, ".json"):
		keys := map[string]json.RawMessage{}
		for name, v := range want {
			if _, k := t.fileOf(name); name != f {
				keys[k] = json.RawMessage(v)
			}
		}
		for _, c := range patch {
			if data, ok := c[f]; ok {
				if out, err := mergedriver.RewriteJSON(data, keys); err == nil {
					return out, nil
				}
			}
		}
		out, err := json.MarshalIndent(keys, "", "  ")
		return append(out, '\n'), err
	case strings.HasSuffix(f, ".ini"):
		ini := mergedriver.INI{}
		for name, v := range want {
			_, k := t.fileOf(name)
			if name == f {
				continue
			}
			i := strings.LastIndex(k, "/")
			if ini[k[:i]] == nil {
				ini[k[:i]] = map[string]string{}
			}
			ini[k[:i]][k[i+1:]] = v
		}
		return ini.Bytes(), nil
	default:
		return []byte(want[f]), nil
	}
}

func equalUnits(a, b units) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// tracked reports whether this host captures and applies a unit. "shared"
// tracking means whatever the layers already hold (known).
func (t Tracking) tracked(name string, known units) bool {
	if t.Track.match(name) || t.Local.match(name) {
		return true
	}
	_, ok := known[name]
	return t.Track.Shared && ok
}

// toHost reports whether a tracked unit's edits belong in the host layer:
// it is listed as local, or the host layer already overrides it (writing it
// to the shared layer instead would be undone by the next apply).
func (t Tracking) toHost(name string, host units) bool {
	_, ok := host[name]
	return ok || t.Local.match(name)
}

// route records live state in the layers: every tracked unit takes its live
// value (or is removed) in the layer it belongs to. Untracked units, live or
// committed, are left as they are. known adds units "shared" tracking covers
// beyond these layers, such as the integration branch's.
func (t Tracking) route(live, shared, host, known units) (units, units) {
	newShared, newHost := clone(shared), clone(host)
	known = union(known, shared, host)
	for _, name := range unionKeys(live, shared, host) {
		if !t.tracked(name, known) {
			continue
		}
		dst := newShared
		if t.toHost(name, host) {
			dst = newHost
		}
		if v, ok := live[name]; ok {
			dst[name] = v
		} else {
			delete(dst, name)
		}
	}
	return newShared, newHost
}

// target is what an apply makes live: the shared layer overlaid with the
// host layer for tracked units, and the live value of everything untracked.
func (t Tracking) target(live, shared, host units) units {
	out := units{}
	known := union(shared, host)
	for name, v := range live {
		if !t.tracked(name, known) {
			out[name] = v
		}
	}
	for _, layer := range []units{shared, host} {
		for name, v := range layer {
			if t.tracked(name, known) {
				out[name] = v
			}
		}
	}
	return out
}

func clone(u units) units {
	out := make(units, len(u))
	for k, v := range u {
		out[k] = v
	}
	return out
}

// union merges unit sets; later ones win.
func union(us ...units) units {
	out := units{}
	for _, u := range us {
		for k, v := range u {
			out[k] = v
		}
	}
	return out
}

func unionKeys(us ...units) []string {
	seen := map[string]bool{}
	var keys []string
	for _, u := range us {
		for k := range u {
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	sort.Strings(keys)
	return keys
}

// layers is one adapter's two layers in a tree, as files and as units.
type layers struct {
	shared, host           map[string][]byte
	sharedUnits, hostUnits units
}

func (b *Buffer) readLayers(cfg adapter.Config, tree gitrepo.Hash) (layers, error) {
	t := b.Config.Tracking[cfg.Name]
	var l layers
	var err error
	if l.shared, err = b.Repo.ReadFiles(tree, cfg.Path); err != nil {
		return l, err
	}
	if l.host, err = b.Repo.ReadFiles(tree, b.Config.hostPath(cfg)); err != nil {
		return l, err
	}
	if l.sharedUnits, err = t.unitize(l.shared); err != nil {
		return l, fmt.Errorf("%s: %w", cfg.Path, err)
	}
	if l.hostUnits, err = t.unitize(l.host); err != nil {
		return l, fmt.Errorf("%s: %w", b.Config.hostPath(cfg), err)
	}
	return l, nil
}

// applyFiles is what an adapter's live system should hold after applying
// tree: its tracked units from the layers, and its untracked units as they
// are live now.
func (b *Buffer) applyFiles(cfg adapter.Config, a adapter.Adapter, tree gitrepo.Hash) (map[string][]byte, error) {
	t := b.Config.Tracking[cfg.Name]
	l, err := b.readLayers(cfg, tree)
	if err != nil {
		return nil, err
	}
	liveFiles, err := a.Snapshot()
	if err != nil {
		return nil, err
	}
	live, err := t.unitize(liveFiles)
	if err != nil {
		return nil, err
	}
	candidates := []map[string][]byte{liveFiles, l.shared, l.host}
	return t.compose(t.target(live, l.sharedUnits, l.hostUnits), candidates, candidates...)
}

// layersEqual reports whether two trees hold the same layers for an adapter.
func (b *Buffer) layersEqual(x, y gitrepo.Hash, cfg adapter.Config) (bool, error) {
	for _, prefix := range []string{cfg.Path, b.Config.hostPath(cfg)} {
		if eq, err := b.Repo.SubtreeEqual(x, y, prefix); err != nil || !eq {
			return false, err
		}
	}
	return true, nil
}
