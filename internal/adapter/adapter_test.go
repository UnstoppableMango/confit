package adapter

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// dconf tests are opt-in so they never touch a developer's real database;
// scripts/test.sh runs them against a private one under dbus-run-session.
func needDconf(t *testing.T) {
	t.Helper()
	if os.Getenv("GB_DCONF_TESTS") != "1" {
		t.Skip("set GB_DCONF_TESTS=1 and run under dbus-run-session (scripts/test.sh)")
	}
}

func dconfWrite(t *testing.T, key, value string) {
	t.Helper()
	if out, err := exec.Command("dconf", "write", key, value).CombinedOutput(); err != nil {
		t.Fatalf("dconf write: %v %s", err, out)
	}
}

func dconfRead(t *testing.T, key string) string {
	t.Helper()
	out, _ := exec.Command("dconf", "read", key).Output()
	return strings.TrimSpace(string(out))
}

func TestDconfSnapshotAndApply(t *testing.T) {
	needDconf(t)
	d := &Dconf{Root: "/org/gbtest/"}
	dconfWrite(t, "/org/gbtest/desktop/interface/color-scheme", "'prefer-dark'")
	dconfWrite(t, "/org/gbtest/desktop/interface/font-name", "'Cantarell 11'")
	dconfWrite(t, "/org/gbtest/mouse/natural-scroll", "false")
	dconfWrite(t, "/org/gbtest/top", "1")

	snap, err := d.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	want := "[desktop/interface]\ncolor-scheme='prefer-dark'\nfont-name='Cantarell 11'\n"
	if string(snap["desktop/interface.ini"]) != want {
		t.Fatalf("snapshot file:\n%s", snap["desktop/interface.ini"])
	}
	if _, ok := snap[rootFile]; !ok {
		t.Fatalf("root keys missing: %v", snap)
	}

	// Target: change one key, drop the mouse dir, keep the rest.
	target := map[string][]byte{
		"desktop/interface.ini": []byte("[desktop/interface]\ncolor-scheme='default'\nfont-name='Cantarell 11'\n"),
		rootFile:                snap[rootFile],
	}
	dconfWrite(t, "/org/gbtest/desktop/interface/extra", "true") // live-only key: must be reset
	if err := d.Apply(target); err != nil {
		t.Fatal(err)
	}
	if got := dconfRead(t, "/org/gbtest/desktop/interface/color-scheme"); got != "'default'" {
		t.Fatalf("color-scheme = %s", got)
	}
	for _, k := range []string{"/org/gbtest/mouse/natural-scroll", "/org/gbtest/desktop/interface/extra"} {
		if got := dconfRead(t, k); got != "" {
			t.Fatalf("%s should be reset, is %s", k, got)
		}
	}
	after, _ := d.Snapshot()
	if len(after) != 2 || string(after["desktop/interface.ini"]) != string(target["desktop/interface.ini"]) {
		t.Fatalf("live state after apply: %v", after)
	}
}

func TestFilesApplyReplacesSymlink(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "store-settings.json")
	os.WriteFile(store, []byte("{}"), 0o444)
	live := filepath.Join(dir, "settings.json")
	os.Symlink(store, live)
	f := &Files{Map: map[string]string{"settings.json": live}}
	if err := f.Apply(map[string][]byte{"settings.json": []byte(`{"a":1}`)}); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Lstat(live)
	if !fi.Mode().IsRegular() {
		t.Fatal("symlink not replaced by a regular file")
	}
	if b, _ := os.ReadFile(store); string(b) != "{}" {
		t.Fatal("wrote through the symlink into the store")
	}
	if err := f.Apply(map[string][]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(live); !os.IsNotExist(err) {
		t.Fatal("file absent from the target was not removed")
	}
}

func TestExternalAdapter(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state.txt")
	os.WriteFile(state, []byte("v1\n"), 0o644)
	script := filepath.Join(dir, "gb-adapter-test")
	os.WriteFile(script, []byte(`#!/bin/sh
set -e
case "$1" in
  snapshot) cp "`+state+`" "$2/state.txt" ;;
  apply) cp "$2/state.txt" "`+state+`" ;;
esac
`), 0o755)
	a, err := New(Config{Name: "t", Type: "test", Command: script})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := a.Snapshot()
	if err != nil || string(snap["state.txt"]) != "v1\n" {
		t.Fatalf("snapshot %v %v", snap, err)
	}
	if err := a.Apply(map[string][]byte{"state.txt": []byte("v2\n")}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(state); string(b) != "v2\n" {
		t.Fatalf("apply wrote %q", b)
	}
}
