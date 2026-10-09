package core

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// End to end against a real dconf database (run under dbus-run-session).
func TestDconfCaptureAndApply(t *testing.T) {
	// Opt-in, so a developer's real desktop database is never touched; the
	// keys also live under a root no real application uses. See scripts/test.sh.
	if os.Getenv("CONFIT_DCONF_TESTS") != "1" {
		t.Skip("set CONFIT_DCONF_TESTS=1 and run under dbus-run-session (scripts/test.sh)")
	}
	write := func(k, v string) {
		if out, err := exec.Command("dconf", "write", k, v).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	read := func(k string) string {
		out, _ := exec.Command("dconf", "read", k).Output()
		return strings.TrimSpace(string(out))
	}
	write("/org/gbtest-core/desktop/interface/color-scheme", "'prefer-dark'")
	write("/org/gbtest-core/desktop/interface/font-name", "'Cantarell 11'")

	e := newEnv(t, "")
	e.git("config", "confit.adapter.dconf.type", "dconf")
	e.git("config", "confit.adapter.dconf.root", "/org/gbtest-core/")
	e.consumer("nixos", "adopt", "dconf")
	b := e.open()

	if _, err := b.Apply("nixos", false); err != nil {
		t.Fatal(err)
	}
	if got := e.show("desired", "dconf/desktop/interface.ini"); !strings.Contains(got, "color-scheme='prefer-dark'") {
		t.Fatalf("initial capture not integrated:\n%s", got)
	}
	// A toggle in GNOME Settings, captured by a watcher or timer.
	write("/org/gbtest-core/desktop/interface/color-scheme", "'default'")
	r, err := b.Capture("dconf", true)
	if err != nil || r.Edit.Summary != "dconf: set /org/gbtest-core/desktop/interface/color-scheme = 'default'" {
		t.Fatalf("capture: %+v %v", r.Edit, err)
	}
	if got := e.git("log", "-1", "--format=%(trailers:key=Confit-Source,valueonly)", "desired"); got != "unattributed" {
		t.Fatalf("Confit-Source = %q", got)
	}

	// Someone edits desired in git (e.g. by hand) and applies it: the live
	// database follows, including removing a key.
	b.EditCommit("human", EditOptions{Prefix: "dconf", Integrate: true, Files: map[string][]byte{
		"desktop/interface.ini": []byte("[desktop/interface]\ncolor-scheme='prefer-light'\n"),
	}})
	if _, err := b.Apply("nixos", false); err != nil {
		t.Fatal(err)
	}
	if read("/org/gbtest-core/desktop/interface/color-scheme") != "'prefer-light'" || read("/org/gbtest-core/desktop/interface/font-name") != "" {
		t.Fatal("apply did not make dconf match desired")
	}
	// Live now equals what was applied, so a capture finds nothing.
	if r, _ := b.Capture("dconf", true); r.Edit.Changed {
		t.Fatalf("capture after apply committed: %+v", r.Edit)
	}
}
