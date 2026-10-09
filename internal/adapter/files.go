package adapter

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// Files tracks plain files, such as VSCode's settings.json. Map is repo name
// -> live path. A missing live file is simply absent from the snapshot.
type Files struct {
	Map map[string]string
}

func (f *Files) Snapshot() (map[string][]byte, error) {
	out := map[string][]byte{}
	for name, live := range f.Map {
		data, err := os.ReadFile(live)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[name] = data
	}
	return out, nil
}

// Apply writes each file atomically (temp file + rename). A live path that is
// a symlink, such as a read-only home-manager link into the Nix store, is
// replaced by a regular file. A file missing from files is removed.
func (f *Files) Apply(files map[string][]byte) error {
	for name, live := range f.Map {
		data, ok := files[name]
		if !ok {
			if err := os.Remove(live); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			continue
		}
		if cur, err := os.ReadFile(live); err == nil && bytes.Equal(cur, data) {
			if fi, err := os.Lstat(live); err == nil && fi.Mode().IsRegular() {
				continue
			}
		}
		if err := os.MkdirAll(filepath.Dir(live), 0o755); err != nil {
			return err
		}
		tmp, err := os.CreateTemp(filepath.Dir(live), "."+filepath.Base(live)+".confit-*")
		if err != nil {
			return err
		}
		_, werr := tmp.Write(data)
		cerr := tmp.Close()
		if err := errors.Join(werr, cerr); err != nil {
			os.Remove(tmp.Name())
			return err
		}
		if err := os.Chmod(tmp.Name(), 0o644); err != nil {
			return err
		}
		if err := os.Rename(tmp.Name(), live); err != nil {
			os.Remove(tmp.Name())
			return err
		}
	}
	return nil
}
