package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Drives the real binary the way a consumer script would.
func TestCLI(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "gb")
	if out, err := exec.Command("go", "build", "-buildvcs=false", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	repo := filepath.Join(dir, "repo.git")
	live := filepath.Join(dir, "settings.json")
	os.WriteFile(live, []byte(`{"a": 1}`+"\n"), 0o644)
	gb := func(wantCode int, args ...string) string {
		t.Helper()
		cmd := exec.Command(bin, append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
		out, err := cmd.Output()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		if code != wantCode {
			t.Fatalf("gb %v: exit %d, want %d\n%s", args, code, wantCode, out)
		}
		return string(out)
	}
	gb(0, "init", "--bare", repo)
	gb(0, "adapter", "add", "vscode", "--type", "file", "--file", "settings.json="+live)
	gb(0, "consumer", "add", "hm", "--adapter", "vscode", "--drift", "block")
	if out := gb(0, "capture", "vscode"); !strings.Contains(out, "vscode: set a = 1") {
		t.Fatalf("capture output: %s", out)
	}
	// Under a block policy captures are not integrated on their own.
	if out := gb(0, "integrate"); !strings.Contains(out, "fast-forward") {
		t.Fatalf("integrate output: %s", out)
	}
	var p struct {
		Commits []struct{ Subject string }
		Empty   bool
	}
	json.Unmarshal([]byte(gb(0, "pending", "hm", "--json")), &p)
	if p.Empty || len(p.Commits) != 2 {
		t.Fatalf("pending: %+v", p)
	}
	target := strings.TrimSpace(gb(0, "prepare", "hm"))
	gb(0, "applied", "advance", "hm", target)
	os.WriteFile(live, []byte(`{"a": 2}`+"\n"), 0o644)
	gb(0, "capture", "vscode") // block policy: recorded, not integrated
	gb(3, "prepare", "hm")
	gb(3, "apply", "hm")
	gb(0, "apply", "hm", "--force")
	if b, _ := os.ReadFile(live); !strings.Contains(string(b), `"a": 1`) {
		t.Fatalf("forced apply left %s", b)
	}
	var st struct {
		Consumers []struct{ Drift []string }
	}
	json.Unmarshal([]byte(gb(0, "status", "--json")), &st)
	if len(st.Consumers) != 1 || len(st.Consumers[0].Drift) != 0 {
		t.Fatalf("status: %+v", st)
	}
	gb(2, "bogus")
}
