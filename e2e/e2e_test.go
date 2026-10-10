// Package e2e runs confit the way a desktop would: the VSCode settings UI
// writes settings.json, GNOME Settings and Tweaks write through GSettings to
// dconf, `confit watch` turns both into commits, and a home-manager style
// consumer applies desired and records it. Everything goes through the real
// binary, real git and a real (throwaway) dconf database.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	host     = "laptop"
	consumer = "home-manager@laptop"
	vscodeEd = "edits/vscode@laptop"
	dconfEd  = "edits/dconf@laptop"
	quiet    = 500 * time.Millisecond
)

// The settings.json a user has before confit: comments and trailing commas,
// which must survive every capture, merge and apply.
const initialSettings = `// Managed by confit: edit in the Settings UI or in git.
{
  // Bigger text for the laptop screen.
  "editor.fontSize": 14,
  "workbench.colorTheme": "Default Dark Modern",
  "files.trimTrailingWhitespace": true,
}
`

type desktop struct {
	root      *testing.T // watcher output outlives each step
	t         *testing.T
	dir       string
	bin       string
	repo      string
	settings  string
	gsettings bool
	watchers  []*exec.Cmd
	cancel    context.CancelFunc
}

func TestDesktop(t *testing.T) {
	if os.Getenv("CONFIT_DCONF_TESTS") != "1" {
		t.Skip("set CONFIT_DCONF_TESTS=1 and run under dbus-run-session (scripts/test.sh)")
	}
	d := setup(t)

	d.step("first run records the existing desktop", func(t *testing.T) {
		d.watch()
		d.waitFor("initial vscode capture", func() bool { return d.has(vscodeEd) })
		d.waitFor("initial capture integrated", func() bool {
			return strings.Contains(d.show("desired", "vscode/settings.json"), `"editor.fontSize": 14`)
		})
		// The first switch: home-manager applies desired and records it.
		d.confit(0, "apply", consumer)
		d.expectApplied("desired")
	})

	d.step("VSCode settings UI edit becomes a commit", func(t *testing.T) {
		d.vscodeSet(`"editor.fontSize": 14`, `"editor.fontSize": 16`, false)
		d.waitForSubject("desired", "vscode: set editor.fontSize = 16")
		got := d.show("desired", "vscode/settings.json")
		if !strings.Contains(got, "// Bigger text for the laptop screen.") || !strings.Contains(got, `"files.trimTrailingWhitespace": true,`) {
			t.Fatalf("comments or trailing comma lost:\n%s", got)
		}
		// Saving by write-and-rename (files.saveAtomically) is caught too.
		d.vscodeSet(`"Default Dark Modern"`, `"Solarized Light"`, true)
		d.waitForSubject("desired", `vscode: set workbench.colorTheme = "Solarized Light"`)
	})

	d.step("GNOME Settings toggle becomes a commit", func(t *testing.T) {
		d.gset("org.gnome.desktop.interface", "color-scheme", "'prefer-dark'")
		d.waitForSubject("desired", "dconf: set /org/gnome/desktop/interface/color-scheme = 'prefer-dark'")
		if got := d.git("log", "-1", "--format=%(trailers:key=Confit-Editor,valueonly)", dconfEd); got != "dconf@"+host {
			t.Fatalf("Confit-Editor = %q", got)
		}
	})

	d.step("a slider drag is one commit", func(t *testing.T) {
		before := d.count(dconfEd)
		for i := 0; i < 20; i++ {
			d.gset("org.gnome.desktop.interface", "text-scaling-factor", fmt.Sprintf("%.3f", 1.0+float64(i)/40))
			time.Sleep(quiet / 10)
		}
		// dconf stores the double as 1.4750000000000001; the subject shows that.
		d.waitForSubject("desired", "dconf: set /org/gnome/desktop/interface/text-scaling-factor = 1.475")
		if n := d.count(dconfEd) - before; n != 1 {
			t.Fatalf("slider drag made %d commits, want 1", n)
		}
	})

	d.step("home-manager switch applies and records", func(t *testing.T) {
		pending := d.pending()
		if pending.Empty || len(pending.Files) != 2 {
			t.Fatalf("pending: %+v", pending)
		}
		d.confit(0, "apply", consumer)
		d.expectApplied("desired")
		if note := d.git("notes", "--ref=confit-applied", "show", "desired"); !strings.Contains(note, "Consumer: "+consumer) {
			t.Fatalf("apply note:\n%s", note)
		}
	})

	d.step("an edit made in git reaches the desktop without echoing back", func(t *testing.T) {
		// Erik edits desired with plain git (here: in a worktree), as he
		// would from his dotfiles checkout.
		d.commitToDesired("vscode/settings.json", func(s string) string {
			return strings.Replace(s, `"files.trimTrailingWhitespace": true,`, "\"files.trimTrailingWhitespace\": true,\n  \"files.autoSave\": \"afterDelay\",", 1)
		}, "Turn on autosave")
		d.commitToDesired("dconf/desktop/interface.ini", func(s string) string {
			return s + "clock-show-seconds=true\n"
		}, "Show seconds")
		vsBefore, dcBefore := d.count(vscodeEd), d.count(dconfEd)
		d.confit(0, "apply", consumer)
		if b, _ := os.ReadFile(d.settings); !strings.Contains(string(b), `"files.autoSave": "afterDelay"`) {
			t.Fatalf("settings.json not applied:\n%s", b)
		}
		if got := d.gget("org.gnome.desktop.interface", "clock-show-seconds"); got != "true" {
			t.Fatalf("clock-show-seconds = %s", got)
		}
		// The watchers see the apply's writes and capture, but the live state
		// equals what was applied, so nothing new is committed (only the
		// apply's own sync commit on each editor branch).
		time.Sleep(4 * quiet)
		d.waitIdle()
		if n := d.count(vscodeEd) - vsBefore; n > 1 {
			t.Fatalf("apply echoed back into %s: %d commits", vscodeEd, n)
		}
		if n := d.count(dconfEd) - dcBefore; n > 1 {
			t.Fatalf("apply echoed back into %s: %d commits", dconfEd, n)
		}
		if !d.pending().Empty {
			t.Fatalf("still pending after apply: %+v", d.pending())
		}
	})

	d.step("drift made while nothing watched is adopted before the switch", func(t *testing.T) {
		d.stopWatchers()
		d.gset("org.gnome.desktop.interface", "enable-hot-corners", "false")
		d.vscodeSet(`"editor.fontSize": 16`, `"editor.fontSize": 13`, false)
		out := d.confit(0, "apply", consumer)
		if !strings.Contains(out, "drift:") {
			t.Fatalf("apply did not report drift:\n%s", out)
		}
		if b, _ := os.ReadFile(d.settings); !strings.Contains(string(b), `"editor.fontSize": 13`) {
			t.Fatalf("adopted drift was undone:\n%s", b)
		}
		if d.gget("org.gnome.desktop.interface", "enable-hot-corners") != "false" {
			t.Fatal("adopted dconf drift was undone")
		}
		d.expectApplied("desired")
		d.watch()
	})

	d.step("a UI edit that conflicts with unapplied intent waits for a human", func(t *testing.T) {
		d.commitToDesired("vscode/settings.json", func(s string) string {
			return strings.Replace(s, `"editor.fontSize": 13`, `"editor.fontSize": 18`, 1)
		}, "Font size 18 everywhere")
		d.vscodeSet(`"editor.fontSize": 13`, `"editor.fontSize": 20`, false)
		d.waitFor("conflict reported", func() bool {
			var st status
			json.Unmarshal([]byte(d.confit(0, "status", "--json")), &st)
			for _, e := range st.Editors {
				if e.Editor == "vscode@"+host && len(e.Conflicts) > 0 {
					return true
				}
			}
			return false
		})
		// Adopt can't adopt a conflict, so the switch refuses rather than
		// overwrite either side.
		d.confit(3, "apply", consumer)
		if b, _ := os.ReadFile(d.settings); !strings.Contains(string(b), `"editor.fontSize": 20`) {
			t.Fatal("a blocked apply touched the live file")
		}
		// Resolve with plain git: merge the editor branch, keep 20.
		wt := d.worktree("desired")
		if out, err := d.gitIn(wt, "merge", "--no-edit", vscodeEd); err == nil {
			t.Fatalf("expected a conflict, merge said:\n%s", out)
		}
		conflicted, _ := os.ReadFile(filepath.Join(wt, "vscode/settings.json"))
		resolved := resolveTheirs(string(conflicted))
		os.WriteFile(filepath.Join(wt, "vscode/settings.json"), []byte(resolved), 0o644)
		d.mustGitIn(wt, "commit", "-am", "Keep the font size from the UI")
		d.mustGitIn(wt, "checkout", "--detach")
		d.confit(0, "apply", consumer)
		if b, _ := os.ReadFile(d.settings); !strings.Contains(string(b), `"editor.fontSize": 20`) || strings.Contains(string(b), "<<<<<<<") {
			t.Fatalf("resolution not applied:\n%s", b)
		}
		var st status
		json.Unmarshal([]byte(d.confit(0, "status", "--json")), &st)
		for _, e := range st.Editors {
			if len(e.Conflicts) > 0 {
				t.Fatalf("conflict still reported: %+v", e)
			}
		}
	})

	d.step("revert policy records drift and restores desired", func(t *testing.T) {
		d.stopWatchers()
		d.confit(0, "consumer", "add", consumer, "--adapter", "vscode", "--adapter", "dconf", "--drift", "revert")
		d.gset("org.gnome.desktop.interface", "color-scheme", "'default'")
		d.confit(0, "apply", consumer)
		if got := d.gget("org.gnome.desktop.interface", "color-scheme"); got != "'prefer-dark'" {
			t.Fatalf("revert left color-scheme = %s", got)
		}
		// The drift is still on record, on the editor branch.
		if log := d.git("log", "--format=%s", dconfEd); !strings.Contains(log, "color-scheme = 'default'") {
			t.Fatalf("reverted drift not recorded:\n%s", log)
		}
	})

	d.step("block policy refuses until a human decides", func(t *testing.T) {
		d.confit(0, "consumer", "add", consumer, "--adapter", "vscode", "--adapter", "dconf", "--drift", "block")
		d.gset("org.gnome.desktop.interface", "color-scheme", "'prefer-light'")
		d.confit(0, "capture", "dconf")
		var st status
		json.Unmarshal([]byte(d.confit(0, "status", "--json")), &st)
		if len(st.Consumers) != 1 || strings.Join(st.Consumers[0].Drift, ",") != "dconf" {
			t.Fatalf("status does not show the drift: %+v", st.Consumers)
		}
		d.confit(3, "prepare", consumer)
		d.confit(3, "apply", consumer)
		// Keep it: integrate the editor branch, then the switch goes through.
		d.confit(0, "integrate", "dconf@"+host)
		d.confit(0, "apply", consumer)
		if got := d.gget("org.gnome.desktop.interface", "color-scheme"); got != "'prefer-light'" {
			t.Fatalf("kept drift lost: %s", got)
		}
	})
}

// step runs one part of the story as a subtest. The parts share one desktop,
// so a failure stops the rest.
func (d *desktop) step(name string, f func(t *testing.T)) {
	parent := d.t
	ok := parent.Run(name, func(t *testing.T) {
		d.t = t
		f(t)
	})
	d.t = parent
	if !ok {
		parent.FailNow()
	}
}

func setup(t *testing.T) *desktop {
	d := &desktop{root: t, t: t, dir: t.TempDir()}
	d.bin = filepath.Join(d.dir, "bin", "confit")
	if out, err := exec.Command("go", "build", "-buildvcs=false", "-o", d.bin, "../cmd/confit").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	d.repo = filepath.Join(d.dir, "confit.git")
	d.settings = filepath.Join(d.dir, "home/.config/Code/User/settings.json")
	os.MkdirAll(filepath.Dir(d.settings), 0o755)
	os.WriteFile(d.settings, []byte(initialSettings), 0o644)
	// Real GSettings needs the GNOME schemas and its dconf backend; without
	// them (as in the Nix dev shell), write the same keys straight to dconf,
	// which is all GSettings does underneath.
	d.gsettings = exec.Command("gsettings", "set", "org.gnome.desktop.interface", "cursor-blink", "true").Run() == nil
	if out, _ := exec.Command("dconf", "read", "/org/gnome/desktop/interface/cursor-blink").Output(); strings.TrimSpace(string(out)) != "true" {
		d.gsettings = false
	}
	if !d.gsettings {
		t.Log("GSettings with the GNOME schemas and dconf backend not found; writing dconf directly")
	}
	t.Cleanup(d.stopWatchers)

	d.confit(0, "init", "--bare", d.repo)
	d.git("config", "confit.host", host)
	d.git("config", "user.name", "Erik")
	d.git("config", "user.email", "erik@example.com")
	d.confit(0, "adapter", "add", "vscode", "--type", "file", "--file", "settings.json="+d.settings)
	d.confit(0, "adapter", "add", "dconf", "--type", "dconf", "--root", "/org/gnome/")
	d.confit(0, "consumer", "add", consumer, "--adapter", "vscode", "--adapter", "dconf")
	return d
}

func (d *desktop) env() []string {
	return append(os.Environ(), "PATH="+filepath.Dir(d.bin)+":"+os.Getenv("PATH"))
}

func (d *desktop) confit(want int, args ...string) string {
	d.t.Helper()
	cmd := exec.Command(d.bin, append([]string{"-C", d.repo}, args...)...)
	cmd.Env = d.env()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		d.t.Fatal(err)
	}
	if code != want {
		d.t.Fatalf("confit %v: exit %d, want %d\n%s%s", args, code, want, out, stderr.String())
	}
	return string(out) + stderr.String()
}

func (d *desktop) git(args ...string) string {
	d.t.Helper()
	out, err := d.gitIn(d.repo, args...)
	if err != nil {
		d.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return out
}

func (d *desktop) gitIn(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = d.env()
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (d *desktop) mustGitIn(dir string, args ...string) {
	d.t.Helper()
	if out, err := d.gitIn(dir, args...); err != nil {
		d.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func (d *desktop) has(branch string) bool {
	_, err := d.gitIn(d.repo, "rev-parse", "--verify", "-q", "refs/heads/"+branch)
	return err == nil
}

func (d *desktop) show(branch, path string) string {
	out, _ := d.gitIn(d.repo, "show", branch+":"+path)
	return out
}

// count counts a branch's own commits, not the ones merged into it.
func (d *desktop) count(branch string) int {
	var n int
	fmt.Sscan(d.git("rev-list", "--count", "--first-parent", branch), &n)
	return n
}

func (d *desktop) waitFor(what string, ok func() bool) {
	d.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			d.t.Fatalf("timed out waiting for %s\n%s", what, d.git("log", "--all", "--oneline", "--graph", "-25"))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (d *desktop) waitForSubject(branch, subject string) {
	d.t.Helper()
	d.waitFor(fmt.Sprintf("%q on %s", subject, branch), func() bool {
		out, _ := d.gitIn(d.repo, "log", "--format=%s", "-20", branch)
		return strings.Contains(out, subject)
	})
}

// waitIdle waits until no capture holds the repo lock.
func (d *desktop) waitIdle() {
	time.Sleep(quiet)
	d.confit(0, "status")
}

func (d *desktop) watch() {
	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	for _, a := range []string{"vscode", "dconf"} {
		cmd := exec.CommandContext(ctx, d.bin, "-C", d.repo, "watch", a, "--quiet", quiet.String())
		cmd.Env = d.env()
		cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
		cmd.WaitDelay = 10 * time.Second
		log := &logWriter{t: d.root, prefix: "watch " + a + ": "}
		cmd.Stdout, cmd.Stderr = log, log
		if err := cmd.Start(); err != nil {
			d.t.Fatal(err)
		}
		d.watchers = append(d.watchers, cmd)
	}
	time.Sleep(quiet) // let dconf watch subscribe
}

func (d *desktop) stopWatchers() {
	if d.cancel == nil {
		return
	}
	d.cancel()
	for _, w := range d.watchers {
		w.Wait()
	}
	d.watchers, d.cancel = nil, nil
}

// vscodeSet edits settings.json the way VSCode's settings editor does: a
// text edit that leaves the rest of the file alone, saved in place, or by
// write-and-rename when atomic is set.
func (d *desktop) vscodeSet(old, nw string, atomic bool) {
	d.t.Helper()
	b, err := os.ReadFile(d.settings)
	if err != nil || !bytes.Contains(b, []byte(old)) {
		d.t.Fatalf("settings.json lacks %s (%v):\n%s", old, err, b)
	}
	b = bytes.Replace(b, []byte(old), []byte(nw), 1)
	if !atomic {
		if err := os.WriteFile(d.settings, b, 0o644); err != nil {
			d.t.Fatal(err)
		}
		return
	}
	tmp := d.settings + ".vsctmp"
	os.WriteFile(tmp, b, 0o644)
	if err := os.Rename(tmp, d.settings); err != nil {
		d.t.Fatal(err)
	}
}

// gset changes a key the way GNOME Settings and Tweaks do: through GSettings.
func (d *desktop) gset(schema, key, value string) {
	d.t.Helper()
	cmd := exec.Command("gsettings", "set", schema, key, value)
	if !d.gsettings {
		cmd = exec.Command("dconf", "write", schemaPath(schema)+key, value)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		d.t.Fatalf("%v: %v %s", cmd.Args, err, out)
	}
}

func (d *desktop) gget(schema, key string) string {
	cmd := exec.Command("gsettings", "get", schema, key)
	if !d.gsettings {
		cmd = exec.Command("dconf", "read", schemaPath(schema)+key)
	}
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

func schemaPath(schema string) string {
	return "/" + strings.ReplaceAll(schema, ".", "/") + "/"
}

type pendingResult struct {
	Empty bool
	Files []struct{ Path string }
}

func (d *desktop) pending() pendingResult {
	var p pendingResult
	json.Unmarshal([]byte(d.confit(0, "pending", consumer, "--json")), &p)
	return p
}

type status struct {
	Editors []struct {
		Editor    string
		Conflicts []string
	}
	Consumers []struct {
		Consumer string
		Drift    []string
	}
}

func (d *desktop) expectApplied(branch string) {
	d.t.Helper()
	if a, b := d.git("rev-parse", "applied/"+consumer), d.git("rev-parse", branch); a != b {
		d.t.Fatalf("applied/%s = %s, %s = %s", consumer, a, branch, b)
	}
}

// worktree checks out branch in a fresh worktree of the bare repo.
func (d *desktop) worktree(branch string) string {
	wt := filepath.Join(d.dir, fmt.Sprintf("wt-%d", time.Now().UnixNano()))
	d.git("worktree", "add", wt, branch)
	return wt
}

// commitToDesired edits one file on desired with plain git, as a person
// would from a checkout.
func (d *desktop) commitToDesired(path string, edit func(string) string, msg string) {
	d.t.Helper()
	wt := d.worktree("desired")
	p := filepath.Join(wt, path)
	b, err := os.ReadFile(p)
	if err != nil {
		d.t.Fatal(err)
	}
	os.WriteFile(p, []byte(edit(string(b))), 0o644)
	d.mustGitIn(wt, "commit", "-am", msg)
	d.mustGitIn(wt, "checkout", "--detach") // release the branch
}

// resolveTheirs keeps the editor branch's side of each conflict.
func resolveTheirs(s string) string {
	var out []string
	state := 0
	for _, line := range strings.SplitAfter(s, "\n") {
		switch {
		case strings.HasPrefix(line, "<<<<<<<"):
			state = 1
		case strings.HasPrefix(line, "=======") && state == 1:
			state = 2
		case strings.HasPrefix(line, ">>>>>>>") && state == 2:
			state = 0
		case state == 0 || state == 2:
			out = append(out, line)
		}
	}
	return strings.Join(out, "")
}

type logWriter struct {
	t      *testing.T
	prefix string
}

func (l *logWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		l.t.Log(l.prefix + line)
	}
	return len(p), nil
}
