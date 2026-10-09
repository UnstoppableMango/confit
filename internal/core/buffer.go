// Package core implements Git Buffer's model on top of plain git: editor
// branches, the integration branch, applied pointers moved only by
// compare-and-swap, and apply records in notes. See design/git-buffer-design.md.
package core

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/UnstoppableMango/git-buffer/internal/adapter"
	"github.com/UnstoppableMango/git-buffer/internal/gitrepo"
)

// Version is stamped into trailers and apply notes.
var Version = "0.1.0-dev"

// Buffer is an opened Git Buffer repository.
type Buffer struct {
	Repo   *gitrepo.Repo
	Config Config
	now    func() time.Time
}

// Open opens the repository containing path and loads its gitbuffer config.
func Open(path string) (*Buffer, error) {
	r, err := gitrepo.Open(path)
	if err != nil {
		return nil, err
	}
	cfg, err := loadConfig(r)
	if err != nil {
		return nil, err
	}
	return &Buffer{Repo: r, Config: cfg, now: time.Now}, nil
}

// InitOptions configures `gb init`.
type InitOptions struct {
	Bare          bool
	Integration   string // default "desired"
	DriverCommand string // default "gb"
}

const gitattributes = "*.json merge=gb-json\n*.ini merge=gb-ini\n"

// Init creates the repository if needed, registers the merge drivers (git
// config is not cloned, so this runs on every clone too) and creates the
// integration branch with a .gitattributes if it doesn't exist.
func Init(path string, opts InitOptions) (*Buffer, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	// Only path itself counts; a repository further up must not be reused.
	if r, err := gitrepo.Open(path); err != nil || (r.GitDir != path && r.GitDir != filepath.Join(path, ".git")) {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return nil, err
		}
		if err := gitrepo.Init(path, or(opts.Integration, "desired"), opts.Bare); err != nil {
			return nil, err
		}
	}
	r, err := gitrepo.Open(path)
	if err != nil {
		return nil, err
	}
	if opts.Integration != "" {
		if err := r.SetConfig("gitbuffer.integrationBranch", opts.Integration); err != nil {
			return nil, err
		}
	}
	if opts.DriverCommand != "" {
		if err := r.SetConfig("gitbuffer.driverCommand", opts.DriverCommand); err != nil {
			return nil, err
		}
	}
	b, err := Open(path)
	if err != nil {
		return nil, err
	}
	for _, kind := range []string{"json", "ini"} {
		if err := r.SetConfig("merge.gb-"+kind+".name", "git-buffer "+kind+" merge"); err != nil {
			return nil, err
		}
		if err := r.SetConfig("merge.gb-"+kind+".driver", b.Config.DriverCommand+" merge-driver "+kind+" %O %A %B %P"); err != nil {
			return nil, err
		}
	}
	unlock, err := r.Lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	tip, err := r.Resolve(b.Config.integrationRef())
	if err != nil || !tip.IsZero() {
		return b, err
	}
	blob, err := r.WriteBlob([]byte(gitattributes))
	if err != nil {
		return nil, err
	}
	tree, err := r.WriteTree(map[string]gitrepo.Entry{".gitattributes": {Mode: 0o100644, Hash: blob}})
	if err != nil {
		return nil, err
	}
	c, err := r.WriteCommit(gitrepo.Commit{Tree: tree, Author: b.human(), Committer: b.committer("init"),
		Message: "gb: initialize\n\nBuffer-Version: " + Version + "\n"})
	if err != nil {
		return nil, err
	}
	return b, r.UpdateRefs("gb init", gitrepo.RefUpdate{Name: b.Config.integrationRef(), New: c})
}

// human is the author of edits: git's user.name/email, else the login user.
func (b *Buffer) human() gitrepo.Signature {
	name := b.Repo.ConfigValue("user.name")
	email := b.Repo.ConfigValue("user.email")
	if name == "" || email == "" {
		login := os.Getenv("USER")
		if u, err := user.Current(); err == nil {
			login = u.Username
		}
		name = or(name, login)
		email = or(email, login+"@"+b.Config.Host)
	}
	return gitrepo.Signature{Name: name, Email: email, When: b.now()}
}

// committer is Git Buffer itself, naming the component that wrote the commit.
func (b *Buffer) committer(component string) gitrepo.Signature {
	return gitrepo.Signature{Name: "git-buffer (" + component + ")", Email: "git-buffer@" + b.Config.Host, When: b.now()}
}

func (b *Buffer) adapter(name string) (adapter.Config, adapter.Adapter, error) {
	cfg, ok := b.Config.Adapters[name]
	if !ok {
		return cfg, nil, fmt.Errorf("no adapter %q (configure gitbuffer.adapter.%s.type)", name, name)
	}
	a, err := adapter.New(cfg)
	return cfg, a, err
}

func (b *Buffer) consumer(name string) (Consumer, error) {
	cs, ok := b.Config.Consumers[name]
	if !ok {
		// An unconfigured consumer follows the integration branch and owns
		// no adapters, which is all an external tool like home-manager needs.
		return Consumer{Name: name, Source: b.Config.Integration, Drift: Adopt}, nil
	}
	return cs, nil
}

func short(h gitrepo.Hash) string {
	if h.IsZero() {
		return ""
	}
	return h.String()[:12]
}

func hashOrEmpty(h gitrepo.Hash) string {
	if h.IsZero() {
		return ""
	}
	return h.String()
}

// trailers renders "Key: value" lines, skipping empty values.
func trailers(kv ...string) string {
	var b strings.Builder
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			fmt.Fprintf(&b, "%s: %s\n", kv[i], kv[i+1])
		}
	}
	return b.String()
}

// trailerValue reads one trailer from a commit message.
func trailerValue(msg, key string) string {
	for _, l := range strings.Split(msg, "\n") {
		if v, ok := strings.CutPrefix(l, key+": "); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func subject(msg string) string {
	s, _, _ := strings.Cut(msg, "\n")
	return s
}
