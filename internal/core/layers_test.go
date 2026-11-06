package core

import (
	"strings"
	"testing"
)

func TestManifestParse(t *testing.T) {
	m, err := ParseManifest([]byte(`
[adapters.vscode]
type = "file"
files = { "settings.json" = "~/.config/Code/User/settings.json" }

[hosts.laptop]
track.vscode = ["settings.json"]
local.vscode = ["settings.json:window.zoomLevel"]

[hosts.desktop]
track.vscode = "shared"
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Hosts["laptop"].Local["vscode"].Prefixes; len(got) != 1 || got[0] != "settings.json:window.zoomLevel" {
		t.Fatalf("local = %v", got)
	}
	if !m.Hosts["desktop"].Track["vscode"].Shared {
		t.Fatal(`track = "shared" not parsed`)
	}
	data, err := m.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	again, err := ParseManifest(data)
	if err != nil {
		t.Fatalf("re-parse:\n%s\n%v", data, err)
	}
	if !again.Hosts["desktop"].Track["vscode"].Shared || again.Adapters["vscode"].Files["settings.json"] == "" {
		t.Fatalf("round trip lost data:\n%s", data)
	}
	if _, err := ParseManifest([]byte("[hosts.laptop]\ntrak.vscode = []\n")); err == nil || !strings.Contains(err.Error(), "trak") {
		t.Fatalf("misspelled key: %v", err)
	}
	if _, err := ParseManifest([]byte("[hosts.laptop]\ntrack.vscode = \"everything\"\n")); err == nil {
		t.Fatal("a selector string other than shared was accepted")
	}
}

func TestSelectorMatch(t *testing.T) {
	s := sel("/org/gnome/desktop/", "/org/gnome/mutter/center-new-windows", "settings.json")
	for name, want := range map[string]bool{
		"/org/gnome/desktop/interface/gtk-theme": true,
		"/org/gnome/desktopx/a":                  false,
		"/org/gnome/mutter/center-new-windows":   true,
		"/org/gnome/mutter/center-new-windows2":  false,
		"settings.json":                          true,
		"settings.json:editor.fontSize":          true,
		"keybindings.json":                       false,
	} {
		if s.match(name) != want {
			t.Errorf("match(%q) = %v", name, !want)
		}
	}
}

func TestDconfUnitsRoundTrip(t *testing.T) {
	tr := Tracking{DconfRoot: "/org/gnome/", Track: all()}
	files := map[string][]byte{
		"desktop/interface.ini": []byte("[desktop/interface]\ncolor-scheme='prefer-dark'\n"),
		"_root.ini":             []byte("[/]\ntop=1\n"),
	}
	u, err := tr.unitize(files)
	if err != nil {
		t.Fatal(err)
	}
	if u["/org/gnome/desktop/interface/color-scheme"] != "'prefer-dark'" || u["/org/gnome/top"] != "1" {
		t.Fatalf("units = %v", u)
	}
	u["/org/gnome/desktop/interface/font-name"] = "'Cantarell 11'"
	out, err := tr.compose(u, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out["desktop/interface.ini"]); got != "[desktop/interface]\ncolor-scheme='prefer-dark'\nfont-name='Cantarell 11'\n" {
		t.Fatalf("interface.ini = %q", got)
	}
	if string(out["_root.ini"]) != string(files["_root.ini"]) {
		t.Fatalf("_root.ini = %q", out["_root.ini"])
	}
}

// A local key goes to the host layer and wins on apply; everything else that
// is tracked goes to the shared layer.
func TestLocalKeysLiveInTheHostLayer(t *testing.T) {
	e := newEnv(t, "")
	live := e.fileAdapter("vscode", "{\n  // mine\n  \"editor.fontSize\": 12,\n  \"window.zoomLevel\": 1\n}\n")
	e.host(func(h *ManifestHost) { h.Local["vscode"] = sel("settings.json:window.zoomLevel") })
	e.consumer("hm", "adopt", "vscode")
	b := e.open()
	if _, err := b.Capture("vscode", true); err != nil {
		t.Fatal(err)
	}
	if got := e.show("desired", "vscode/settings.json"); strings.Contains(got, "zoomLevel") || !strings.Contains(got, `"editor.fontSize": 12`) || !strings.Contains(got, "// mine") {
		t.Fatalf("shared layer:\n%s", got)
	}
	if got := e.show("desired", "hosts/laptop/vscode/settings.json"); !strings.Contains(got, `"window.zoomLevel": 1`) || strings.Contains(got, "fontSize") {
		t.Fatalf("host layer:\n%s", got)
	}

	// Another host changes both keys in the shared layer.
	if _, err := b.EditCommit("vscode@desktop", EditOptions{Prefix: "vscode", Integrate: true,
		Files: map[string][]byte{"settings.json": []byte("{\n  // mine\n  \"editor.fontSize\": 20,\n  \"window.zoomLevel\": 5\n}\n")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Apply("hm", false); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, live)
	if !strings.Contains(got, `"editor.fontSize": 20`) || !strings.Contains(got, `"window.zoomLevel": 1`) || !strings.Contains(got, "// mine") {
		t.Fatalf("live after apply:\n%s", got)
	}

	// A live change to the local key stays in the host layer.
	writeFile(t, live, strings.Replace(got, `"window.zoomLevel": 1`, `"window.zoomLevel": 2`, 1))
	r, err := b.Capture("vscode", true)
	if err != nil || r.Edit.Summary != "vscode: set window.zoomLevel = 2" {
		t.Fatalf("capture: %+v %v", r.Edit, err)
	}
	if got := e.show("desired", "vscode/settings.json"); !strings.Contains(got, `"window.zoomLevel": 5`) {
		t.Fatalf("shared layer changed:\n%s", got)
	}
	if got := e.show("desired", "hosts/laptop/vscode/settings.json"); !strings.Contains(got, `"window.zoomLevel": 2`) {
		t.Fatalf("host layer:\n%s", got)
	}
}

// Anything outside the allowlist is never captured, and apply leaves it alone.
func TestUntrackedKeysAreLeftAlone(t *testing.T) {
	e := newEnv(t, "")
	live := e.fileAdapter("vscode", "{\"editor.fontSize\": 12, \"secret.token\": \"abc\"}\n")
	e.host(func(h *ManifestHost) { h.Track["vscode"] = sel("settings.json:editor.fontSize") })
	e.consumer("hm", "adopt", "vscode")
	b := e.open()
	if _, err := b.Capture("vscode", true); err != nil {
		t.Fatal(err)
	}
	if got := e.show("desired", "vscode/settings.json"); strings.Contains(got, "secret") || !strings.Contains(got, "12") {
		t.Fatalf("shared layer:\n%s", got)
	}
	writeFile(t, live, "{\"editor.fontSize\": 12, \"secret.token\": \"xyz\"}\n")
	if r, err := b.Capture("vscode", true); err != nil || r.Edit.Changed {
		t.Fatalf("untracked change captured: %+v %v", r.Edit, err)
	}
	// Another host's untracked key in the shared layer isn't applied here.
	if _, err := b.EditCommit("vscode@desktop", EditOptions{Prefix: "vscode", Integrate: true,
		Files: map[string][]byte{"settings.json": []byte("{\"editor.fontSize\": 14, \"files.autoSave\": \"on\"}\n")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Apply("hm", false); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, live); !strings.Contains(got, `"editor.fontSize": 14`) || !strings.Contains(got, "xyz") || strings.Contains(got, "autoSave") {
		t.Fatalf("live after apply:\n%s", got)
	}
}

// A host missing from the manifest adds itself, tracking only what the
// shared layer already holds.
func TestUnknownHostRegisters(t *testing.T) {
	e := newEnv(t, "")
	live := e.fileAdapter("vscode", "{\"editor.fontSize\": 12, \"window.zoomLevel\": 1}\n")
	e.manifest(func(m *Manifest) { delete(m.Hosts, "laptop") })
	b := e.open()
	// Registration comes first, so the adapter is known. The shared layer
	// is empty, so nothing is tracked yet.
	if r, err := b.Capture("vscode", true); err != nil || r.Edit.Changed {
		t.Fatalf("capture: %+v %v", r.Edit, err)
	}
	tip, err := b.Repo.Resolve("refs/heads/desired")
	if err != nil {
		t.Fatal(err)
	}
	m, err := b.readManifest(tip)
	if err != nil {
		t.Fatal(err)
	}
	if h, ok := m.Hosts["laptop"]; !ok || !h.Track["vscode"].Shared {
		t.Fatalf("host not registered: %+v", m.Hosts)
	}
	if got := e.git("ls-tree", "-r", "--name-only", "desired"); strings.Contains(got, "vscode/") {
		t.Fatalf("capture with an empty shared layer recorded something:\n%s", got)
	}
	if got := e.git("log", "-1", "--format=%s", "desired"); got != "manifest: register host laptop" {
		t.Fatalf("last commit = %q", got)
	}

	// Once the shared layer holds a key, a live change to it is captured,
	// and other live keys still aren't.
	if _, err := b.EditCommit("vscode@desktop", EditOptions{Prefix: "vscode", Integrate: true,
		Files: map[string][]byte{"settings.json": []byte("{\"editor.fontSize\": 12}\n")}}); err != nil {
		t.Fatal(err)
	}
	e.consumer("hm", "adopt", "vscode")
	b = e.open()
	if _, err := b.Apply("hm", false); err != nil {
		t.Fatal(err)
	}
	writeFile(t, live, "{\"editor.fontSize\": 16, \"window.zoomLevel\": 3}\n")
	r, err := b.Capture("vscode", true)
	if err != nil || r.Edit.Summary != "vscode: set editor.fontSize = 16" {
		t.Fatalf("capture: %+v %v", r.Edit, err)
	}
}

func TestEmptySelectorRoundTrips(t *testing.T) {
	m := Manifest{Hosts: map[string]ManifestHost{"laptop": {Track: map[string]Selector{"vscode": {}}}}}
	data, err := m.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseManifest(data); err != nil {
		t.Fatalf("%v\n%s", err, data)
	}
}

// An invalid manifest edit is refused before it reaches the integration branch.
func TestInvalidManifestIsNotCommitted(t *testing.T) {
	e := newEnv(t, "")
	before := e.git("rev-parse", "desired")
	_, err := e.open().UpdateManifest("bad", func(m *Manifest) error {
		m.Adapters = map[string]ManifestAdapter{"x": {Type: "file", Path: "hosts/laptop/x"}}
		m.Hosts = map[string]ManifestHost{"laptop": {Track: map[string]Selector{"x": all()}}}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("err = %v", err)
	}
	if e.git("rev-parse", "desired") != before {
		t.Fatal("invalid manifest was committed")
	}
}
