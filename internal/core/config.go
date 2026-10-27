package core

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/UnstoppableMango/confit/internal/adapter"
	"github.com/UnstoppableMango/confit/internal/gitrepo"
)

// DriftPolicy says what a consumer does with live changes it finds before
// applying (design doc §4, Drift).
type DriftPolicy string

const (
	Adopt  DriftPolicy = "adopt"  // integrate the drift; the apply keeps it
	Revert DriftPolicy = "revert" // record the drift, then the apply overwrites it
	Block  DriftPolicy = "block"  // refuse to apply until someone decides
)

// Consumer is `[hosts.<host>.consumers.<name>]` in the manifest.
type Consumer struct {
	Name     string
	Source   string   // branch it follows; defaults to the integration branch
	Adapters []string // live systems it writes, checked for drift first
	Drift    DriftPolicy
}

// Tracking is what this host captures and applies through one adapter, and
// which of it lives in the host's own layer.
type Tracking struct {
	Track, Local Selector
	// DconfRoot names dconf keys by their full path; empty for other adapters.
	DconfRoot string
}

// Config is the machine-local settings under `confit.*` in git config, plus
// this host's view of the manifest. Branch names are configuration: confit
// never looks at HEAD or a remote's default branch.
type Config struct {
	Integration   string // short branch name, default "desired"
	EditsPrefix   string // default "edits/"
	AppliedPrefix string // default "applied/"
	Host          string
	DriverCommand string // how git invokes confit's merge drivers, default "confit"
	Manifest      Manifest
	// Adapters, Consumers and Tracking are this host's, from the manifest.
	Adapters  map[string]adapter.Config
	Consumers map[string]Consumer
	Tracking  map[string]Tracking
}

const notesRef = "refs/notes/confit-applied"

func loadConfig(r *gitrepo.Repo) (Config, error) {
	raw, err := r.Config("confit")
	if err != nil {
		return Config{}, err
	}
	last := func(k string) string {
		v := raw[strings.ToLower(k)]
		if len(v) == 0 {
			return ""
		}
		return v[len(v)-1]
	}
	host, _ := os.Hostname()
	return Config{
		Integration:   or(last("confit.integrationBranch"), "desired"),
		EditsPrefix:   or(last("confit.editsPrefix"), "edits/"),
		AppliedPrefix: or(last("confit.appliedPrefix"), "applied/"),
		Host:          or(last("confit.host"), host),
		DriverCommand: or(last("confit.driverCommand"), "confit"),
	}, nil
}

// loadManifest derives this host's adapters, consumers and tracking.
func (b *Buffer) loadManifest(m Manifest) error {
	c := &b.Config
	c.Manifest = m
	c.Adapters, c.Consumers, c.Tracking = map[string]adapter.Config{}, map[string]Consumer{}, map[string]Tracking{}
	h := m.Hosts[c.Host]
	for _, name := range sortedKeys(m.Adapters) {
		track, tracked := h.Track[name]
		local, isLocal := h.Local[name]
		if !tracked && !isLocal {
			continue
		}
		ma := m.Adapters[name]
		a := adapter.Config{Name: name, Type: ma.Type, Path: or(strings.Trim(ma.Path, "/"), name),
			Editor: name + "@" + c.Host, Root: ma.Root, Command: ma.Command, Integrate: true}
		if a.Path == hostsDir || strings.HasPrefix(a.Path, hostsDir+"/") {
			return fmt.Errorf("adapter %s: path %q is reserved for host layers", name, hostsDir)
		}
		for _, repoName := range sortedKeys(ma.Files) {
			a.Files = append(a.Files, repoName+"="+expandHome(ma.Files[repoName]))
		}
		c.Adapters[name] = a
		t := Tracking{Track: track, Local: local}
		if a.Type == "dconf" {
			t.DconfRoot = or(a.Root, "/")
		}
		c.Tracking[name] = t
	}
	for _, name := range sortedKeys(h.Consumers) {
		mc := h.Consumers[name]
		cs := Consumer{Name: name, Source: or(mc.Source, c.Integration), Adapters: append([]string(nil), mc.Adapters...),
			Drift: DriftPolicy(or(mc.Drift, string(Adopt)))}
		switch cs.Drift {
		case Adopt, Revert, Block:
		default:
			return fmt.Errorf("consumer %s: unknown drift policy %q", name, cs.Drift)
		}
		for _, a := range cs.Adapters {
			if _, ok := c.Adapters[a]; !ok {
				return fmt.Errorf("consumer %s writes adapter %s, which host %s doesn't track", name, a, c.Host)
			}
		}
		sort.Strings(cs.Adapters)
		c.Consumers[name] = cs
	}
	// An adapter feeding a revert or block consumer must not integrate its
	// captures on its own, or the drift would be adopted behind the policy.
	for name, a := range c.Adapters {
		if set := m.Adapters[name].Integrate; set != nil {
			a.Integrate = *set
		} else {
			for _, cs := range c.Consumers {
				for _, an := range cs.Adapters {
					if an == name && cs.Drift != Adopt {
						a.Integrate = false
					}
				}
			}
		}
		c.Adapters[name] = a
	}
	return nil
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// Ref names. These are the only places branch names are built.
func (c Config) integrationRef() string { return "refs/heads/" + c.Integration }
func (c Config) editRef(editor string) string {
	return "refs/heads/" + c.EditsPrefix + editor
}
func (c Config) appliedRef(consumer string) string {
	return "refs/heads/" + c.AppliedPrefix + consumer
}
func (c Config) sourceRef(cs Consumer) string { return "refs/heads/" + cs.Source }

// hostsDir holds the per-host layers: hosts/<host>/<adapter path>/...
const hostsDir = "hosts"

// hostPath is where this host's layer of an adapter lives.
func (c Config) hostPath(a adapter.Config) string { return hostsDir + "/" + c.Host + "/" + a.Path }
