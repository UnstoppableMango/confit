package core

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/UnstoppableMango/git-buffer/internal/gitrepo"
)

// gbBinary is built once so git can run gb's merge drivers during tests.
var gbBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gb-test-bin-")
	if err != nil {
		panic(err)
	}
	gbBinary = filepath.Join(dir, "gb")
	if out, err := exec.Command("go", "build", "-buildvcs=false", "-o", gbBinary, "../../cmd/gb").CombinedOutput(); err != nil {
		panic(string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type env struct {
	t    *testing.T
	dir  string
	repo string
}

func newEnv(t *testing.T, integration string) *env {
	t.Helper()
	e := &env{t: t, dir: t.TempDir()}
	e.repo = filepath.Join(e.dir, "repo.git")
	if _, err := Init(e.repo, InitOptions{Bare: true, Integration: integration, DriverCommand: gbBinary}); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) git(args ...string) string {
	e.t.Helper()
	out, err := exec.Command("git", append([]string{"-C", e.repo}, args...)...).CombinedOutput()
	if err != nil {
		e.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (e *env) open() *Buffer {
	e.t.Helper()
	b, err := Open(e.repo)
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

// fileAdapter configures a file adapter for one live settings.json.
func (e *env) fileAdapter(name, content string) string {
	live := filepath.Join(e.dir, name+"-settings.json")
	if content != "" {
		writeFile(e.t, live, content)
	}
	e.git("config", "gitbuffer.adapter."+name+".type", "file")
	e.git("config", "gitbuffer.adapter."+name+".file", "settings.json="+live)
	return live
}

func (e *env) consumer(name, policy string, adapters ...string) {
	e.git("config", "gitbuffer.consumer."+name+".drift", policy)
	for _, a := range adapters {
		e.git("config", "--add", "gitbuffer.consumer."+name+".adapter", a)
	}
}

func (e *env) show(rev, path string) string {
	return e.git("show", rev+":"+path)
}

func writeFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const settings = "{\n  // keep me\n  \"editor.fontSize\": 12,\n  \"editor.tabSize\": 4\n}\n"

func TestCaptureCommitsOnlyChangesWithTrailers(t *testing.T) {
	e := newEnv(t, "")
	live := e.fileAdapter("vscode", settings)
	b := e.open()

	r, err := b.Capture("vscode", true)
	if err != nil || !r.Edit.Changed || r.Integrated.Status != FastForward {
		t.Fatalf("first capture: %+v %v", r, err)
	}
	if r, _ = b.Capture("vscode", true); r.Edit.Changed {
		t.Fatal("capture without a live change made a commit")
	}
	writeFile(t, live, strings.Replace(settings, "12", "14", 1))
	r, err = b.Capture("vscode", true)
	if err != nil || r.Edit.Summary != "vscode: set editor.fontSize = 14" {
		t.Fatalf("summary: %+v %v", r.Edit, err)
	}
	host := b.Config.Host
	if got := e.git("log", "-1", "--format=%(trailers:key=Buffer-Editor,valueonly)", "desired"); got != "vscode@"+host {
		t.Fatalf("Buffer-Editor trailer = %q", got)
	}
	if got := e.git("log", "-1", "--format=%(trailers:key=Buffer-Keys,valueonly)", "desired"); got != "editor.fontSize" {
		t.Fatalf("Buffer-Keys trailer = %q", got)
	}
	if got := e.git("log", "-1", "--format=%cn", "desired"); got != "git-buffer (file)" {
		t.Fatalf("committer = %q", got)
	}
	if !strings.Contains(e.show("desired", "vscode/settings.json"), "// keep me") {
		t.Fatal("comment lost")
	}
}

func TestEditorsMergeThroughDriverAndConflictsStayOut(t *testing.T) {
	e := newEnv(t, "")
	b := e.open()
	if _, err := b.EditCommit("seed", EditOptions{Prefix: "vscode", Files: map[string][]byte{"settings.json": []byte(settings)}, Integrate: true}); err != nil {
		t.Fatal(err)
	}
	// Both editors start from the same desired and change adjacent lines,
	// which a line merge cannot resolve.
	b1, _ := b.EditCommit("laptop", EditOptions{Prefix: "vscode", Files: map[string][]byte{"settings.json": []byte(strings.Replace(settings, "12", "14", 1))}})
	b2, _ := b.EditCommit("desktop", EditOptions{Prefix: "vscode", Files: map[string][]byte{"settings.json": []byte(strings.Replace(settings, "4\n", "2\n", 1))}})
	b3, _ := b.EditCommit("tablet", EditOptions{Prefix: "vscode", Files: map[string][]byte{"settings.json": []byte(strings.Replace(settings, "12", "16", 1))}})
	if !b1.Edit.Changed || !b2.Edit.Changed || !b3.Edit.Changed {
		t.Fatal("edits not recorded")
	}
	rs, err := b.Integrate("laptop", "desktop")
	if err != nil || rs[0].Status != FastForward || rs[1].Status != Merged {
		t.Fatalf("integrate: %+v %v", rs, err)
	}
	got := e.show("desired", "vscode/settings.json")
	if !strings.Contains(got, `"editor.fontSize": 14`) || !strings.Contains(got, `"editor.tabSize": 2`) || !strings.Contains(got, "// keep me") {
		t.Fatalf("merged settings wrong:\n%s", got)
	}

	// A real conflict: tablet branched from the same base and set the same
	// key to another value. desired must not move.
	before := e.git("rev-parse", "desired")
	rs, err = b.Integrate("tablet")
	if err != nil || rs[0].Status != Conflict || rs[0].Conflicts[0] != "vscode/settings.json" {
		t.Fatalf("want conflict, got %+v %v", rs, err)
	}
	if e.git("rev-parse", "desired") != before {
		t.Fatal("desired moved on conflict")
	}
	st, err := b.Status()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, es := range st.Editors {
		if es.Editor == "tablet" {
			found = len(es.Conflicts) == 1 && es.Unintegrated == 1
		}
	}
	if !found {
		t.Fatalf("status does not report the conflict: %+v", st.Editors)
	}
}

func TestDriftAdopt(t *testing.T) {
	e := newEnv(t, "")
	live := e.fileAdapter("vscode", settings)
	e.consumer("hm", "adopt", "vscode")
	b := e.open()
	if _, err := b.Apply("hm", false); err != nil {
		t.Fatal(err)
	}
	// Someone edits the live file and no capture runs before the apply.
	writeFile(t, live, strings.Replace(settings, "12", "18", 1))
	res, err := b.Apply("hm", false)
	if err != nil || len(res.Prepare.Drift) != 1 {
		t.Fatalf("apply: %+v %v", res, err)
	}
	if !strings.Contains(e.show("desired", "vscode/settings.json"), "18") {
		t.Fatal("adopted drift is not in desired")
	}
	if !strings.Contains(readFile(t, live), "18") {
		t.Fatal("apply lost the live change")
	}
	if e.git("rev-parse", "applied/hm") != e.git("rev-parse", "desired") {
		t.Fatal("applied pointer not at desired")
	}
}

func TestDriftRevert(t *testing.T) {
	e := newEnv(t, "")
	live := e.fileAdapter("vscode", settings)
	e.consumer("hm", "revert", "vscode")
	b := e.open()
	if b.Config.Adapters["vscode"].Integrate {
		t.Fatal("an adapter feeding a revert consumer must not auto-integrate")
	}
	// Seed desired with an editor, then apply it.
	b.EditCommit("seed", EditOptions{Prefix: "vscode", Files: map[string][]byte{"settings.json": []byte(settings)}, Integrate: true})
	if _, err := b.Apply("hm", false); err != nil {
		t.Fatal(err)
	}
	writeFile(t, live, strings.Replace(settings, "12", "18", 1))
	if r, _ := b.Capture("vscode", true); r.Integrated != nil {
		t.Fatal("capture integrated drift under a revert policy")
	}
	res, err := b.Apply("hm", false)
	if err != nil {
		t.Fatal(err)
	}
	_ = res
	if strings.Contains(readFile(t, live), "18") {
		t.Fatal("revert did not restore the live file")
	}
	if strings.Contains(e.show("desired", "vscode/settings.json"), "18") {
		t.Fatal("reverted drift reached desired")
	}
	// The drift is recorded for audit on the editor branch.
	editor := "edits/vscode@" + b.Config.Host
	if !strings.Contains(e.git("log", "--format=%s", editor), "set editor.fontSize = 18") {
		t.Fatal("drift not recorded on the editor branch")
	}
	st, _ := b.Status()
	if len(st.Consumers[0].Drift) != 0 {
		t.Fatalf("status still reports drift after revert: %+v", st.Consumers[0])
	}
	// Integrating that branch later must not resurrect the reverted value.
	if _, err := b.Integrate("vscode@" + b.Config.Host); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(e.show("desired", "vscode/settings.json"), "18") {
		t.Fatal("integrating after revert resurrected the drift")
	}
}

func TestDriftBlock(t *testing.T) {
	e := newEnv(t, "")
	live := e.fileAdapter("vscode", settings)
	e.consumer("hm", "block", "vscode")
	b := e.open()
	b.EditCommit("seed", EditOptions{Prefix: "vscode", Files: map[string][]byte{"settings.json": []byte(settings)}, Integrate: true})
	if _, err := b.Apply("hm", false); err != nil {
		t.Fatal(err)
	}
	writeFile(t, live, strings.Replace(settings, "12", "18", 1))
	if _, err := b.Apply("hm", false); !errors.Is(err, ErrBlocked) {
		t.Fatalf("want ErrBlocked, got %v", err)
	}
	if !strings.Contains(readFile(t, live), "18") {
		t.Fatal("a blocked apply touched the live file")
	}
	// Still blocked on the next try, even though nothing new was captured.
	if _, err := b.Apply("hm", false); !errors.Is(err, ErrBlocked) {
		t.Fatalf("second apply: want ErrBlocked, got %v", err)
	}
	if _, err := b.Apply("hm", true); err != nil {
		t.Fatalf("forced apply: %v", err)
	}
	if strings.Contains(readFile(t, live), "18") {
		t.Fatal("forced apply did not discard the drift")
	}
	if _, err := b.Apply("hm", false); err != nil {
		t.Fatalf("after force, apply should be unblocked: %v", err)
	}
}

// Intent committed to desired but not applied yet must not be overwritten
// by the first capture of different live state; they meet as a conflict.
func TestFirstCaptureDoesNotOverrideUnappliedIntent(t *testing.T) {
	e := newEnv(t, "")
	e.fileAdapter("vscode", settings)
	b := e.open()
	intent := strings.Replace(settings, "12", "20", 1)
	b.EditCommit("human", EditOptions{Prefix: "vscode", Files: map[string][]byte{"settings.json": []byte(intent)}, Integrate: true})
	r, err := b.Capture("vscode", true)
	if err != nil {
		t.Fatal(err)
	}
	if r.Integrated.Status != Conflict {
		t.Fatalf("want conflict, got %+v", r.Integrated)
	}
	if !strings.Contains(e.show("desired", "vscode/settings.json"), "20") {
		t.Fatal("intent was overwritten")
	}
}

func TestAdvanceRules(t *testing.T) {
	e := newEnv(t, "main") // integration branch renamed
	b := e.open()
	r1, _ := b.EditCommit("x", EditOptions{Prefix: "a", Files: map[string][]byte{"f.json": []byte(`{"k":1}`)}, Integrate: true})
	r2, _ := b.EditCommit("x", EditOptions{Prefix: "a", Files: map[string][]byte{"f.json": []byte(`{"k":2}`)}, Integrate: true})
	if _, err := b.Advance("hm", r1.Edit.Commit, "", ""); err != nil {
		t.Fatal(err)
	}
	if got := e.git("rev-parse", "applied/hm"); got != r1.Edit.Commit {
		t.Fatalf("applied/hm = %s", got)
	}
	// Wrong expected value: compare-and-swap fails.
	if _, err := b.Advance("hm", r2.Edit.Commit, r2.Edit.Commit, ""); !errors.Is(err, gitrepo.ErrRefChanged) {
		t.Fatalf("want ErrRefChanged, got %v", err)
	}
	// A commit outside the source's history is refused.
	stray, _ := b.EditCommit("stray", EditOptions{Prefix: "a", Files: map[string][]byte{"f.json": []byte(`{"k":3}`)}})
	if _, err := b.Advance("hm", stray.Edit.Commit, "", ""); err == nil {
		t.Fatal("advanced to a commit outside main")
	}
	if _, err := b.Advance("hm", r2.Edit.Commit, r1.Edit.Commit, "ok"); err != nil {
		t.Fatal(err)
	}
	note := e.git("notes", "--ref=buffer-applied", "show", r2.Edit.Commit)
	if !strings.Contains(note, "Consumer: hm") || !strings.Contains(note, "Result: ok") {
		t.Fatalf("note: %s", note)
	}
	p, _ := b.Pending("hm")
	if !p.Empty || len(p.Commits) != 0 {
		t.Fatalf("pending after advance: %+v", p)
	}
	if exec.Command("git", "-C", e.repo, "rev-parse", "--verify", "-q", "refs/heads/desired").Run() == nil {
		t.Fatal("a desired branch exists although integration is main")
	}
}

func TestPendingListsCommitsAndFiles(t *testing.T) {
	e := newEnv(t, "")
	b := e.open()
	r1, _ := b.EditCommit("x", EditOptions{Prefix: "a", Files: map[string][]byte{"f.json": []byte(`{"k":1}`)}, Integrate: true})
	b.Advance("hm", r1.Edit.Commit, "", "")
	b.EditCommit("x", EditOptions{Prefix: "a", Files: map[string][]byte{"f.json": []byte(`{"k":2}`), "g.json": []byte(`{}`)}, Integrate: true})
	p, err := b.Pending("hm")
	if err != nil || len(p.Commits) != 1 || len(p.Files) != 2 || p.Files[0] != (FileChange{"a/f.json", "M"}) || p.Files[1] != (FileChange{"a/g.json", "A"}) {
		t.Fatalf("pending: %+v %v", p, err)
	}
	if p.Commits[0].Editor != "x" {
		t.Fatalf("editor not read from trailer: %+v", p.Commits[0])
	}
}

// Captures racing from separate handles (like separate processes) record a
// live change exactly once.
func TestConcurrentCapturesCommitOnce(t *testing.T) {
	e := newEnv(t, "")
	live := e.fileAdapter("vscode", settings)
	if _, err := e.open().Capture("vscode", true); err != nil {
		t.Fatal(err)
	}
	writeFile(t, live, strings.Replace(settings, "12", "13", 1))
	before := e.git("rev-list", "--count", "desired")
	done := make(chan error)
	for i := 0; i < 6; i++ {
		go func() {
			b, err := Open(e.repo)
			if err == nil {
				_, err = b.Capture("vscode", true)
			}
			done <- err
		}()
	}
	for i := 0; i < 6; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if after := e.git("rev-list", "--count", "desired"); after != fmt.Sprint(mustAtoi(before)+1) {
		t.Fatalf("commits before %s, after %s; want exactly one more", before, after)
	}
}

func mustAtoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		panic(err)
	}
	return n
}
