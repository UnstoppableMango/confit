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
	bin := filepath.Join(dir, "confit")
	if out, err := exec.Command("go", "build", "-buildvcs=false", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	repo := filepath.Join(dir, "repo.git")
	live := filepath.Join(dir, "settings.json")
	os.WriteFile(live, []byte(`{"a": 1}`+"\n"), 0o644)
	confit := func(wantCode int, args ...string) string {
		t.Helper()
		cmd := exec.Command(bin, append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
		out, err := cmd.Output()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		if code != wantCode {
			t.Fatalf("confit %v: exit %d, want %d\n%s", args, code, wantCode, out)
		}
		return string(out)
	}
	confit(0, "init", "--bare", repo)
	confit(0, "adapter", "add", "vscode", "--type", "file", "--file", "settings.json="+live)
	confit(0, "consumer", "add", "hm", "--adapter", "vscode", "--drift", "block")
	if out := confit(0, "capture", "vscode"); !strings.Contains(out, "vscode: set a = 1") {
		t.Fatalf("capture output: %s", out)
	}
	// Under a block policy captures are not integrated on their own.
	if out := confit(0, "integrate"); !strings.Contains(out, "fast-forward") {
		t.Fatalf("integrate output: %s", out)
	}
	var p struct {
		Commits []struct{ Subject string }
		Empty   bool
	}
	json.Unmarshal([]byte(confit(0, "pending", "hm", "--json")), &p)
	if p.Empty || len(p.Commits) != 2 {
		t.Fatalf("pending: %+v", p)
	}
	target := strings.TrimSpace(confit(0, "prepare", "hm"))
	confit(0, "applied", "advance", "hm", target)
	os.WriteFile(live, []byte(`{"a": 2}`+"\n"), 0o644)
	confit(0, "capture", "vscode") // block policy: recorded, not integrated
	confit(3, "prepare", "hm")
	confit(3, "apply", "hm")
	confit(0, "apply", "hm", "--force")
	if b, _ := os.ReadFile(live); !strings.Contains(string(b), `"a": 1`) {
		t.Fatalf("forced apply left %s", b)
	}
	var st struct {
		Consumers []struct{ Drift []string }
	}
	json.Unmarshal([]byte(confit(0, "status", "--json")), &st)
	if len(st.Consumers) != 1 || len(st.Consumers[0].Drift) != 0 {
		t.Fatalf("status: %+v", st)
	}
	confit(1, "integrate", "no-such-editor") // an error, not a panic
	confit(2, "bogus")
}
