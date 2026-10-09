package core

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/UnstoppableMango/git-buffer/internal/adapter"
	"github.com/UnstoppableMango/git-buffer/internal/gitrepo"
)

// DriftPolicy says what a consumer does with live changes it finds before
// applying (design doc §4, Drift).
type DriftPolicy string

const (
	Adopt  DriftPolicy = "adopt"  // integrate the drift; the apply keeps it
	Revert DriftPolicy = "revert" // record the drift, then the apply overwrites it
	Block  DriftPolicy = "block"  // refuse to apply until someone decides
)

// Consumer is `gitbuffer.consumer.<name>.*`.
type Consumer struct {
	Name     string
	Source   string   // branch it follows; defaults to the integration branch
	Adapters []string // live systems it writes, checked for drift first
	Drift    DriftPolicy
}

// Config is everything under `gitbuffer.*` in the repo's git config. Branch
// names are configuration: gb never looks at HEAD or a remote's default branch.
type Config struct {
	Integration   string // short branch name, default "desired"
	EditsPrefix   string // default "edits/"
	AppliedPrefix string // default "applied/"
	Host          string
	DriverCommand string // how git invokes gb's merge drivers, default "gb"
	Adapters      map[string]adapter.Config
	Consumers     map[string]Consumer
}

const notesRef = "refs/notes/buffer-applied"

func loadConfig(r *gitrepo.Repo) (Config, error) {
	raw, err := r.Config("gitbuffer")
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
	c := Config{
		Integration:   or(last("gitbuffer.integrationBranch"), "desired"),
		EditsPrefix:   or(last("gitbuffer.editsPrefix"), "edits/"),
		AppliedPrefix: or(last("gitbuffer.appliedPrefix"), "applied/"),
		Host:          or(last("gitbuffer.host"), host),
		DriverCommand: or(last("gitbuffer.driverCommand"), "gb"),
		Adapters:      map[string]adapter.Config{},
		Consumers:     map[string]Consumer{},
	}
	// Subsection names keep their case in git config; variable names don't.
	for key, vals := range raw {
		parts := strings.SplitN(key, ".", 2)
		if len(parts) < 2 {
			continue
		}
		rest := parts[1]
		i := strings.LastIndex(rest, ".")
		if i < 0 {
			continue
		}
		kind, name, field := "", "", rest[i+1:]
		if k, n, ok := strings.Cut(rest[:i], "."); ok {
			kind, name = k, n
		}
		v := vals[len(vals)-1]
		switch kind {
		case "adapter":
			a := c.Adapters[name]
			a.Name = name
			switch field {
			case "type":
				a.Type = v
			case "path":
				a.Path = v
			case "editor":
				a.Editor = v
			case "file":
				a.Files = vals
			case "root":
				a.Root = v
			case "command":
				a.Command = v
			case "integrate":
				a.Integrate = v != "false"
			}
			if !hasKey(raw, "gitbuffer.adapter."+name+".integrate") {
				a.Integrate = true
			}
			c.Adapters[name] = a
		case "consumer":
			cs := c.Consumers[name]
			cs.Name = name
			switch field {
			case "source":
				cs.Source = v
			case "adapter":
				cs.Adapters = vals
			case "drift":
				cs.Drift = DriftPolicy(v)
			}
			c.Consumers[name] = cs
		}
	}
	for name, a := range c.Adapters {
		if a.Path == "" {
			a.Path = name
		}
		if a.Editor == "" {
			a.Editor = name + "@" + c.Host
		}
		c.Adapters[name] = a
	}
	// An adapter feeding a revert or block consumer must not integrate its
	// captures on its own, or the drift would be adopted behind the policy.
	for name, a := range c.Adapters {
		if hasKey(raw, "gitbuffer.adapter."+name+".integrate") {
			continue
		}
		for _, cs := range c.Consumers {
			for _, an := range cs.Adapters {
				if an == name && cs.Drift != "" && cs.Drift != Adopt {
					a.Integrate = false
				}
			}
		}
		c.Adapters[name] = a
	}
	for name, cs := range c.Consumers {
		if cs.Source == "" {
			cs.Source = c.Integration
		}
		if cs.Drift == "" {
			cs.Drift = Adopt
		}
		switch cs.Drift {
		case Adopt, Revert, Block:
		default:
			return c, fmt.Errorf("consumer %s: unknown drift policy %q", name, cs.Drift)
		}
		sort.Strings(cs.Adapters)
		c.Consumers[name] = cs
	}
	return c, nil
}

func hasKey(raw map[string][]string, key string) bool {
	_, ok := raw[strings.ToLower(key)]
	return ok
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
