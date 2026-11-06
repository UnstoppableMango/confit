package core

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// remote creates the bare repository hosts share and points e at it.
func (e *env) remote() string {
	e.t.Helper()
	r := filepath.Join(e.dir, "remote.git")
	if out, err := exec.Command("git", "init", "--quiet", "--bare", "--initial-branch=desired", r).CombinedOutput(); err != nil {
		e.t.Fatalf("%v\n%s", err, out)
	}
	e.git("remote", "add", "origin", r)
	return r
}

// clone makes a second host from the shared remote, as a new machine would.
func (e *env) clone(remote, host string) *env {
	e.t.Helper()
	c := &env{t: e.t, dir: e.t.TempDir()}
	c.repo = filepath.Join(c.dir, "repo.git")
	if out, err := exec.Command("git", "clone", "--quiet", "--bare", remote, c.repo).CombinedOutput(); err != nil {
		e.t.Fatalf("%v\n%s", err, out)
	}
	if _, err := Init(c.repo, InitOptions{DriverCommand: gbBinary}); err != nil {
		e.t.Fatal(err)
	}
	c.git("config", "confit.host", host)
	return c
}

func mustPush(t *testing.T, b *Buffer) SyncResult {
	t.Helper()
	r, err := b.Push()
	if err != nil || !r.Pushed || len(r.Warnings) > 0 {
		t.Fatalf("push: %+v %v", r, err)
	}
	return r
}

func remoteGit(t *testing.T, remote string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", remote}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// Two hosts share a remote: edits from either reach the other, a push that
// loses the race merges and retries, and each host's own refs stay its own.
func TestSyncTwoHosts(t *testing.T) {
	laptop := newEnv(t, "")
	remote := laptop.remote()
	live := laptop.fileAdapter("vscode", "{\"editor.fontSize\": 12}\n")
	laptop.consumer("hm", "adopt", "vscode")
	lb := laptop.open()
	if _, err := lb.Capture("vscode", true); err != nil {
		t.Fatal(err)
	}
	mustPush(t, lb)

	desktop := laptop.clone(remote, "desktop")
	db := desktop.open()
	if r, err := db.Pull(); err != nil || r.Integration != UpToDate {
		t.Fatalf("pull: %+v %v", r, err)
	}
	if _, err := db.EditCommit("vscode@desktop", EditOptions{Prefix: "vscode", Integrate: true,
		Files: map[string][]byte{"settings.json": []byte("{\"editor.fontSize\": 20}\n")}}); err != nil {
		t.Fatal(err)
	}
	mustPush(t, db)

	// The laptop captures without having seen the desktop's push, so its
	// push is rejected; it merges and pushes again.
	writeFile(t, live, "{\"editor.fontSize\": 12, \"files.autoSave\": \"on\"}\n")
	if _, err := lb.Capture("vscode", true); err != nil {
		t.Fatal(err)
	}
	if r := mustPush(t, lb); r.Integration != Merged {
		t.Fatalf("push after a race: %+v", r)
	}
	if got := remoteGit(t, remote, "show", "desired:vscode/settings.json"); !strings.Contains(got, "20") || !strings.Contains(got, "autoSave") {
		t.Fatalf("remote desired:\n%s", got)
	}
	if _, err := lb.Apply("hm", false); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, live); !strings.Contains(got, `"editor.fontSize": 20`) || !strings.Contains(got, "autoSave") {
		t.Fatalf("live after apply:\n%s", got)
	}
	mustPush(t, lb)
	if got := remoteGit(t, remote, "notes", "--ref=confit-applied/laptop", "show", "applied/hm"); !strings.Contains(got, "Consumer: hm") {
		t.Fatalf("remote apply note:\n%s", got)
	}

	// The desktop catches up and mirrors the laptop's refs, but keeps its own.
	own := desktop.git("rev-parse", "edits/vscode@desktop")
	if r, err := db.Pull(); err != nil || r.Integration != FastForward {
		t.Fatalf("desktop pull: %+v %v", r, err)
	}
	for _, ref := range []string{"desired", "applied/hm", "edits/vscode@laptop", "refs/notes/confit-applied/laptop"} {
		if desktop.git("rev-parse", ref) != remoteGit(t, remote, "rev-parse", ref) {
			t.Errorf("%s not mirrored", ref)
		}
	}
	if desktop.git("rev-parse", "edits/vscode@desktop") != own {
		t.Error("the desktop's own editor branch was overwritten")
	}
	if _, ok := db.Config.Manifest.Hosts["laptop"].Consumers["hm"]; !ok {
		t.Error("manifest not reloaded after pull")
	}
}

// Offline, everything works locally and the next push sends it.
func TestSyncOffline(t *testing.T) {
	e := newEnv(t, "")
	missing := filepath.Join(e.dir, "not-yet.git")
	e.git("remote", "add", "origin", missing)
	e.fileAdapter("vscode", "{\"editor.fontSize\": 12}\n")
	b := e.open()
	if r, err := b.Pull(); err != nil || len(r.Warnings) != 1 || r.Integration != "" {
		t.Fatalf("offline pull: %+v %v", r, err)
	}
	if _, err := b.Capture("vscode", true); err != nil {
		t.Fatal(err)
	}
	if r, err := b.Push(); err != nil || r.Pushed || len(r.Warnings) != 1 {
		t.Fatalf("offline push: %+v %v", r, err)
	}
	if out, err := exec.Command("git", "init", "--quiet", "--bare", missing).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	mustPush(t, b)
	if remoteGit(t, missing, "rev-parse", "desired") != e.git("rev-parse", "desired") ||
		remoteGit(t, missing, "rev-parse", "edits/vscode@laptop") != e.git("rev-parse", "edits/vscode@laptop") {
		t.Fatal("refs not pushed once back online")
	}
}

// No remote configured: sync does nothing.
func TestSyncWithoutRemote(t *testing.T) {
	e := newEnv(t, "")
	b := e.open()
	if r, err := b.Pull(); err != nil || r.Remote != "" || len(r.Warnings) > 0 {
		t.Fatalf("pull: %+v %v", r, err)
	}
	if r, err := b.Push(); err != nil || r.Pushed {
		t.Fatalf("push: %+v %v", r, err)
	}
}

// A conflicting integration branch isn't pushed; the host's own refs are,
// so the other host can see the edit.
func TestSyncConflictKeepsDesiredLocal(t *testing.T) {
	laptop := newEnv(t, "")
	remote := laptop.remote()
	live := laptop.fileAdapter("vscode", "{\"editor.fontSize\": 12}\n")
	lb := laptop.open()
	if _, err := lb.Capture("vscode", true); err != nil {
		t.Fatal(err)
	}
	mustPush(t, lb)
	desktop := laptop.clone(remote, "desktop")
	db := desktop.open()
	if _, err := db.EditCommit("vscode@desktop", EditOptions{Prefix: "vscode", Integrate: true,
		Files: map[string][]byte{"settings.json": []byte("{\"editor.fontSize\": 20}\n")}}); err != nil {
		t.Fatal(err)
	}
	mustPush(t, db)
	theirs := remoteGit(t, remote, "rev-parse", "desired")

	writeFile(t, live, "{\"editor.fontSize\": 16}\n")
	if _, err := lb.Capture("vscode", true); err != nil {
		t.Fatal(err)
	}
	ours := laptop.git("rev-parse", "desired")
	r, err := lb.Push()
	if err != nil || r.Integration != Conflict || !r.Pushed || len(r.Warnings) != 1 {
		t.Fatalf("push: %+v %v", r, err)
	}
	if remoteGit(t, remote, "rev-parse", "desired") != theirs || laptop.git("rev-parse", "desired") != ours {
		t.Fatal("a conflicting integration branch moved")
	}
	if remoteGit(t, remote, "rev-parse", "edits/vscode@laptop") != laptop.git("rev-parse", "edits/vscode@laptop") {
		t.Fatal("the laptop's editor branch wasn't pushed")
	}
}

// A host that ran confit init before adding the remote takes the remote's
// history instead of failing on unrelated histories.
func TestSyncReplacesFreshInit(t *testing.T) {
	laptop := newEnv(t, "")
	remote := laptop.remote()
	laptop.fileAdapter("vscode", "{\"editor.fontSize\": 12}\n")
	mustPush(t, laptop.open())

	fresh := newEnv(t, "")
	fresh.git("config", "confit.host", "desktop")
	fresh.git("remote", "add", "origin", remote)
	b := fresh.open()
	if r, err := b.Pull(); err != nil || r.Integration != FastForward {
		t.Fatalf("pull: %+v %v", r, err)
	}
	if fresh.git("rev-parse", "desired") != laptop.git("rev-parse", "desired") {
		t.Fatal("fresh desired not replaced by the remote's")
	}
}
