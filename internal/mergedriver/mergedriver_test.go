package mergedriver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMergeJSONKeepsCommentsAndMergesAdjacentKeys(t *testing.T) {
	base := "{\n  // font\n  \"editor.fontSize\": 12,\n  \"editor.tabSize\": 4,\n}\n"
	ours := "{\n  // font\n  \"editor.fontSize\": 14,\n  \"editor.tabSize\": 4,\n}\n"
	theirs := "{\n  // font\n  \"editor.fontSize\": 12,\n  \"editor.tabSize\": 2,\n  \"files.eol\": \"\\n\",\n}\n"
	out, conflicts, err := MergeJSON([]byte(base), []byte(ours), []byte(theirs))
	if err != nil || len(conflicts) > 0 {
		t.Fatalf("err=%v conflicts=%v", err, conflicts)
	}
	s := string(out)
	for _, want := range []string{"// font", `"editor.fontSize": 14`, `"editor.tabSize": 2`, "\n  \"files.eol\""} {
		if !strings.Contains(s, want) {
			t.Errorf("merged output lacks %q:\n%s", want, s)
		}
	}
}

func TestMergeJSONConflictAndRemoval(t *testing.T) {
	base := `{"a": 1, "b": {"x": 1, "y": 2}, "c": true}`
	ours := `{"a": 2, "b": {"x": 1, "y": 2}, "c": true}`
	theirs := `{"a": 3, "b": {"x": 1}, "c": true}`
	_, conflicts, err := MergeJSON([]byte(base), []byte(ours), []byte(theirs))
	if err != nil || len(conflicts) != 1 || conflicts[0] != "a" {
		t.Fatalf("want conflict on a, got %v %v", conflicts, err)
	}
	theirs = `{"a": 1, "b": {"x": 1}, "c": null}`
	out, conflicts, err := MergeJSON([]byte(base), []byte(ours), []byte(theirs))
	if err != nil || len(conflicts) > 0 {
		t.Fatalf("err=%v conflicts=%v", err, conflicts)
	}
	m, _ := decodeJSONC(out)
	if m["a"] != float64(2) || m["c"] != nil || len(m["b"].(map[string]any)) != 1 {
		t.Fatalf("bad merge: %s", out)
	}
	if _, ok := m["c"]; !ok {
		t.Fatalf("null value was dropped instead of set: %s", out)
	}
}

func TestMergeINI(t *testing.T) {
	base := "[a]\nk1=1\nk2=2\n"
	ours := "[a]\nk1=10\nk2=2\n"
	theirs := "[a]\nk1=1\nk2=20\n\n[b]\nz=1\n"
	out, conflicts, err := MergeINI([]byte(base), []byte(ours), []byte(theirs))
	if err != nil || len(conflicts) > 0 {
		t.Fatalf("err=%v conflicts=%v", err, conflicts)
	}
	if string(out) != "[a]\nk1=10\nk2=20\n\n[b]\nz=1\n" {
		t.Fatalf("got %q", out)
	}
	_, conflicts, _ = MergeINI([]byte(base), []byte(ours), []byte("[a]\nk1=11\nk2=2\n"))
	if len(conflicts) != 1 || conflicts[0] != "a/k1" {
		t.Fatalf("want conflict a/k1, got %v", conflicts)
	}
}

// Run is what git invokes; it must leave markers and exit 1 on conflicts.
func TestRunExitCodes(t *testing.T) {
	dir := t.TempDir()
	write := func(name, s string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(s), 0o644)
		return p
	}
	o := write("o", "{\n  \"a\": 1,\n  \"b\": 1\n}\n")
	a := write("a", "{\n  \"a\": 2,\n  \"b\": 1\n}\n")
	b := write("b", "{\n  \"a\": 1,\n  \"b\": 2\n}\n")
	if code := Run("json", o, a, b); code != 0 {
		t.Fatalf("clean merge exited %d", code)
	}
	got, _ := os.ReadFile(a)
	if !strings.Contains(string(got), `"a": 2`) || !strings.Contains(string(got), `"b": 2`) {
		t.Fatalf("bad result %s", got)
	}
	a = write("a", "{\n  \"a\": 3,\n  \"b\": 1\n}\n")
	b = write("b", "{\n  \"a\": 4,\n  \"b\": 2\n}\n")
	if code := Run("json", o, a, b); code != 1 {
		t.Fatalf("conflict exited %d", code)
	}
	got, _ = os.ReadFile(a)
	if !strings.Contains(string(got), "<<<<<<<") {
		t.Fatalf("no conflict markers: %s", got)
	}
}
