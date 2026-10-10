// Package adapter connects confit to a live system. Every adapter is two one-shot
// operations: Snapshot reads the whole live state as files, and Apply makes
// the live state match a set of files. There is no event stream to miss:
// capture always compares whole states.
package adapter

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Adapter reads and writes one live system. Paths in the file maps are
// relative to the adapter's directory in the repo.
type Adapter interface {
	Snapshot() (map[string][]byte, error)
	Apply(files map[string][]byte) error
}

// Config is an adapter's settings from `confit.adapter.<name>.*`.
type Config struct {
	Name    string
	Type    string   // dconf, file, or anything else for confit-adapter-<type>
	Path    string   // directory in the repo; defaults to the name
	Editor  string   // editor branch suffix; defaults to <name>@<host>
	Files   []string // file: "live/path" or "repo/name=live/path"
	Root    string   // dconf: subtree to manage, default "/"
	Command string   // external: executable, default confit-adapter-<type>
	// Integrate controls whether capture merges this editor into the
	// integration branch right away. Defaults to true.
	Integrate bool
}

// New builds the adapter for a config.
func New(c Config) (Adapter, error) {
	switch c.Type {
	case "dconf":
		root := c.Root
		if root == "" {
			root = "/"
		}
		if !strings.HasPrefix(root, "/") || !strings.HasSuffix(root, "/") {
			return nil, fmt.Errorf("dconf root must start and end with /: %q", root)
		}
		return &Dconf{Root: root}, nil
	case "file":
		f := &Files{Map: map[string]string{}}
		for _, spec := range c.Files {
			name, live, ok := strings.Cut(spec, "=")
			if !ok {
				live, name = spec, filepath.Base(spec)
			}
			f.Map[name] = live
		}
		if len(f.Map) == 0 {
			return nil, fmt.Errorf("adapter %s: type file needs at least one file", c.Name)
		}
		return f, nil
	case "":
		return nil, fmt.Errorf("adapter %s has no type", c.Name)
	default:
		cmd := c.Command
		if cmd == "" {
			cmd = "confit-adapter-" + c.Type
		}
		return &External{Command: cmd, Name: c.Name}, nil
	}
}

// External runs `<command> snapshot <dir>` and `<command> apply <dir>`, in
// the style of git's helper commands, so adapters can be written in anything.
type External struct {
	Command string
	Name    string
}

func (e *External) run(verb, dir string) error {
	cmd := exec.Command(e.Command, verb, dir)
	cmd.Env = append(os.Environ(), "CONFIT_ADAPTER="+e.Name)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", e.Command, verb, err)
	}
	return nil
}

func (e *External) Snapshot() (map[string][]byte, error) {
	dir, err := os.MkdirTemp("", "confit-snapshot-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	if err := e.run("snapshot", dir); err != nil {
		return nil, err
	}
	return ReadDir(dir)
}

func (e *External) Apply(files map[string][]byte) error {
	dir, err := os.MkdirTemp("", "confit-apply-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := WriteDir(dir, files); err != nil {
		return err
	}
	return e.run("apply", dir)
}

// ReadDir reads every regular file under dir, keyed by slash path.
func ReadDir(dir string) (map[string][]byte, error) {
	files := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		data, err := os.ReadFile(p)
		files[filepath.ToSlash(rel)] = data
		return err
	})
	return files, err
}

// WriteDir writes files under dir.
func WriteDir(dir string, files map[string][]byte) error {
	for rel, data := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}
