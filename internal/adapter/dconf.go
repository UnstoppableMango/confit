package adapter

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"

	"github.com/UnstoppableMango/git-buffer/internal/mergedriver"
)

// rootFile holds keys that sit directly in the managed root.
const rootFile = "_root.ini"

// Dconf snapshots dconf with `dconf dump` and applies with `dconf load` and
// `dconf reset`. Each dconf directory becomes one file, <dir>.ini, with keys
// sorted, so diffs and merges stay per key.
type Dconf struct {
	Root string // e.g. "/" or "/org/gnome/"
}

func (d *Dconf) Snapshot() (map[string][]byte, error) {
	out, err := exec.Command("dconf", "dump", d.Root).Output()
	if err != nil {
		return nil, fmt.Errorf("dconf dump %s: %w", d.Root, err)
	}
	ini, err := mergedriver.ParseINI(out)
	if err != nil {
		return nil, fmt.Errorf("dconf dump: %w", err)
	}
	files := map[string][]byte{}
	for section, kv := range ini {
		files[sectionFile(section)] = mergedriver.INI{section: kv}.Bytes()
	}
	return files, nil
}

func (d *Dconf) Apply(files map[string][]byte) error {
	live, err := d.Snapshot()
	if err != nil {
		return err
	}
	want := mergedriver.INI{}
	for name, data := range files {
		ini, err := mergedriver.ParseINI(data)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		for s, kv := range ini {
			want[s] = kv
		}
	}
	// Reset keys that exist live but not in the target.
	for name, data := range live {
		ini, err := mergedriver.ParseINI(data)
		if err != nil {
			return err
		}
		for s, kv := range ini {
			for k := range kv {
				if _, ok := want[s][k]; !ok {
					if err := dconfRun(nil, "reset", d.keyPath(s, k)); err != nil {
						return fmt.Errorf("%s: %w", name, err)
					}
				}
			}
		}
	}
	// Load every directory whose content differs.
	for s, kv := range want {
		file := sectionFile(s)
		body := mergedriver.INI{"/": kv}.Bytes()
		if bytes.Equal(live[file], mergedriver.INI{s: kv}.Bytes()) {
			continue
		}
		if err := dconfRun(body, "load", d.dirPath(s)); err != nil {
			return err
		}
	}
	return nil
}

func sectionFile(section string) string {
	if section == "/" || section == "" {
		return rootFile
	}
	return strings.Trim(section, "/") + ".ini"
}

func (d *Dconf) dirPath(section string) string {
	if section == "/" || section == "" {
		return d.Root
	}
	return d.Root + strings.Trim(section, "/") + "/"
}

func (d *Dconf) keyPath(section, key string) string {
	return d.dirPath(section) + key
}

// KeyPath names a changed key for commit messages: "/org/gnome/x/key".
func (d *Dconf) KeyPath(sectionKey string) string {
	i := strings.LastIndex(sectionKey, "/")
	return d.keyPath(sectionKey[:i], sectionKey[i+1:])
}

func dconfRun(stdin []byte, args ...string) error {
	cmd := exec.Command("dconf", args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("dconf %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
