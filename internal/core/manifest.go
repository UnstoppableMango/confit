package core

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/UnstoppableMango/confit/internal/gitrepo"
)

// ManifestFile is the manifest's path on the integration branch.
const ManifestFile = "confit.toml"

// Manifest is confit.toml: the adapters a repo knows, and for each host what
// it tracks, where its edits go and which consumers it runs (docs/multi-host.md).
// It is versioned with everything else, so every machine sharing the repo reads
// the same one. Only machine-local settings stay in git config.
type Manifest struct {
	Adapters map[string]ManifestAdapter `toml:"adapters,omitempty"`
	Hosts    map[string]ManifestHost    `toml:"hosts,omitempty"`
}

// ManifestAdapter is `[adapters.<name>]`.
type ManifestAdapter struct {
	Type    string `toml:"type"`
	Path    string `toml:"path,omitempty"`    // directory in the repo; defaults to the name
	Root    string `toml:"root,omitempty"`    // dconf: subtree to manage
	Command string `toml:"command,omitempty"` // external adapter executable
	// Files maps a file's name in the repo to its live path. A leading ~/ is
	// the home directory of whoever runs confit, so one entry fits every host.
	Files map[string]string `toml:"files,omitempty"`
	// Integrate false keeps captures on the editor branch until someone runs
	// confit integrate. Unset, it follows the consumers' drift policies.
	Integrate *bool `toml:"integrate,omitempty"`
}

// ManifestHost is `[hosts.<host>]`.
type ManifestHost struct {
	// Track is the allowlist per adapter: dconf keys or directories ("/org/gnome/desktop/"),
	// file names ("settings.json"), top-level JSON keys ("settings.json:window.zoomLevel"),
	// "*" for everything, or the string "shared" for whatever the shared layer holds.
	Track map[string]Selector `toml:"track,omitempty"`
	// Local names what goes to this host's own layer instead of the shared one.
	// Anything local is tracked too.
	Local     map[string]Selector         `toml:"local,omitempty"`
	Consumers map[string]ManifestConsumer `toml:"consumers,omitempty"`
}

// ManifestConsumer is `[hosts.<host>.consumers.<name>]`.
type ManifestConsumer struct {
	Source   string   `toml:"source,omitempty"`
	Adapters []string `toml:"adapters,omitempty"`
	Drift    string   `toml:"drift,omitempty"`
}

// Selector picks units (keys or files) of one adapter. See ManifestHost.Track.
type Selector struct {
	Shared   bool
	Prefixes []string
}

func (s *Selector) UnmarshalTOML(v any) error {
	switch v := v.(type) {
	case string:
		if v != "shared" {
			return fmt.Errorf(`a selector is a list of prefixes or "shared", not %q`, v)
		}
		s.Shared = true
	case []any:
		for _, p := range v {
			str, ok := p.(string)
			if !ok {
				return fmt.Errorf("selector entries are strings, not %v", p)
			}
			s.Prefixes = append(s.Prefixes, str)
		}
	default:
		return fmt.Errorf(`a selector is a list of prefixes or "shared", not %v`, v)
	}
	return nil
}

func (s Selector) MarshalTOML() ([]byte, error) {
	if s.Shared {
		return []byte(`"shared"`), nil
	}
	if len(s.Prefixes) == 0 {
		return []byte("[]"), nil
	}
	var b bytes.Buffer
	if err := toml.NewEncoder(&b).Encode(map[string][]string{"x": s.Prefixes}); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(bytes.TrimPrefix(b.Bytes(), []byte("x = "))), nil
}

// match reports whether a unit name is selected. An entry selects itself,
// everything under it when it ends in "/", and the keys of a file it names.
func (s Selector) match(name string) bool {
	for _, p := range s.Prefixes {
		if p == "*" || name == p ||
			(strings.HasSuffix(p, "/") && strings.HasPrefix(name, p)) ||
			strings.HasPrefix(name, p+":") {
			return true
		}
	}
	return false
}

// ParseManifest reads confit.toml. Empty input is an empty manifest.
func ParseManifest(data []byte) (Manifest, error) {
	var m Manifest
	md, err := toml.Decode(string(data), &m)
	if err != nil {
		return m, fmt.Errorf("%s: %w", ManifestFile, err)
	}
	if un := md.Undecoded(); len(un) > 0 {
		keys := make([]string, len(un))
		for i, k := range un {
			keys[i] = k.String()
		}
		return m, fmt.Errorf("%s: unknown keys %s", ManifestFile, strings.Join(keys, ", "))
	}
	return m, nil
}

// Bytes encodes the manifest. Comments are not preserved.
func (m Manifest) Bytes() ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("# confit manifest; see docs/multi-host.md\n\n")
	enc := toml.NewEncoder(&b)
	enc.Indent = ""
	err := enc.Encode(m)
	return b.Bytes(), err
}

// readManifest reads the manifest from a commit; a missing file is empty.
func (b *Buffer) readManifest(commit gitrepo.Hash) (Manifest, error) {
	if commit.IsZero() {
		return Manifest{}, nil
	}
	c, err := b.Repo.ReadCommit(commit)
	if err != nil {
		return Manifest{}, err
	}
	data, _, err := b.Repo.ReadFile(c.Tree, ManifestFile)
	if err != nil {
		return Manifest{}, err
	}
	return ParseManifest(data)
}

// UpdateManifest edits the manifest on the integration branch and commits
// the result when it changed. Caller must not hold the repo lock.
func (b *Buffer) UpdateManifest(summary string, edit func(*Manifest) error) (bool, error) {
	unlock, err := b.Repo.Lock()
	if err != nil {
		return false, err
	}
	defer unlock()
	return b.updateManifest(summary, edit)
}

func (b *Buffer) updateManifest(summary string, edit func(*Manifest) error) (bool, error) {
	for attempt := 0; attempt < 5; attempt++ {
		tip, err := b.integrationTip()
		if err != nil {
			return false, err
		}
		m, err := b.readManifest(tip)
		if err != nil {
			return false, err
		}
		before, err := m.Bytes()
		if err != nil {
			return false, err
		}
		if err := edit(&m); err != nil {
			return false, err
		}
		after, err := m.Bytes()
		if err != nil {
			return false, err
		}
		if bytes.Equal(before, after) {
			return false, b.loadManifest(m)
		}
		// A manifest this host can't load must not reach the shared branch,
		// or every command would fail on Open until someone fixes it by hand.
		probe := *b
		if err := probe.loadManifest(m); err != nil {
			return false, err
		}
		tc, err := b.Repo.ReadCommit(tip)
		if err != nil {
			return false, err
		}
		tree, err := b.Repo.WithFile(tc.Tree, ManifestFile, after)
		if err != nil {
			return false, err
		}
		msg := "manifest: " + summary + "\n\n" + trailers("Confit-Editor", "manifest@"+b.Config.Host)
		c, err := b.Repo.WriteCommit(gitrepo.Commit{Tree: tree, Parents: []gitrepo.Hash{tip},
			Author: b.human(), Committer: b.committer("manifest"), Message: msg})
		if err != nil {
			return false, err
		}
		err = b.Repo.UpdateRefs("confit manifest", gitrepo.RefUpdate{Name: b.Config.integrationRef(), New: c, Old: tip})
		if errors.Is(err, gitrepo.ErrRefChanged) {
			continue
		}
		if err != nil {
			return false, err
		}
		return true, b.loadManifest(m)
	}
	return false, fmt.Errorf("%s kept changing while updating the manifest", b.Config.Integration)
}

// refresh reloads the manifest from the integration branch, which another
// process may have changed since Open, and registers this host if it is
// missing. Caller holds the repo lock.
func (b *Buffer) refresh() error {
	tip, err := b.Repo.Resolve(b.Config.integrationRef())
	if err != nil || tip.IsZero() {
		return err
	}
	m, err := b.readManifest(tip)
	if err != nil {
		return err
	}
	if err := b.loadManifest(m); err != nil {
		return err
	}
	return b.register()
}

// register adds this host to the manifest when it isn't there yet, tracking
// only what the shared layer holds (docs/multi-host.md, decision 16). A repo
// with no adapters has nothing to track, so nothing is written. Caller holds
// the repo lock.
func (b *Buffer) register() error {
	if _, ok := b.Config.Manifest.Hosts[b.Config.Host]; ok || len(b.Config.Manifest.Adapters) == 0 {
		return nil
	}
	_, err := b.updateManifest("register host "+b.Config.Host, func(m *Manifest) error {
		m.SeedHost(b.Config.Host)
		return nil
	})
	return err
}

// SeedHost adds host to the manifest when it is missing, tracking only the
// shared layer of every adapter (decision 16). A known host is left as is.
func (m *Manifest) SeedHost(host string) {
	if _, ok := m.Hosts[host]; ok {
		return
	}
	h := ManifestHost{Track: map[string]Selector{}}
	for name := range m.Adapters {
		h.Track[name] = Selector{Shared: true}
	}
	if m.Hosts == nil {
		m.Hosts = map[string]ManifestHost{}
	}
	m.Hosts[host] = h
}

// expandHome resolves a leading ~/ against the current user's home.
func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return p
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
