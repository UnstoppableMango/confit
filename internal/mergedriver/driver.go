// Package mergedriver holds confit's format-aware git merge drivers. git runs
// them through .gitattributes (`*.json merge=confit-json`), so the same resolver
// applies whether confit integrates or a person merges with plain git.
//
// Both drivers try git's own line merge first, which keeps the file byte for
// byte where it can. Only when that conflicts do they merge key by key. A
// key changed to different values on both sides is a real conflict: the
// driver leaves git's conflict markers in place for any mergetool and exits 1.
package mergedriver

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// Run implements `confit merge-driver <kind> %O %A %B`: base, ours (result is
// written here) and theirs. It returns the exit status git expects.
func Run(kind, base, ours, theirs string) int {
	o, err1 := os.ReadFile(base)
	a, err2 := os.ReadFile(ours)
	b, err3 := os.ReadFile(theirs)
	if err := errors.Join(err1, err2, err3); err != nil {
		fmt.Fprintln(os.Stderr, "confit merge-driver:", err)
		return 2
	}
	lineMerged, clean, err := lineMerge(base, ours, theirs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "confit merge-driver:", err)
		return 2
	}
	if clean {
		return write(ours, lineMerged, 0)
	}
	var merged []byte
	var conflicts []string
	switch kind {
	case "json":
		merged, conflicts, err = MergeJSON(o, a, b)
	case "ini":
		merged, conflicts, err = MergeINI(o, a, b)
	default:
		err = fmt.Errorf("unknown driver %q", kind)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "confit merge-driver %s: %v; leaving line conflict\n", kind, err)
		return write(ours, lineMerged, 1)
	}
	if len(conflicts) > 0 {
		fmt.Fprintf(os.Stderr, "confit merge-driver %s: both sides changed: %v\n", kind, conflicts)
		return write(ours, lineMerged, 1)
	}
	return write(ours, merged, 0)
}

func write(path string, data []byte, code int) int {
	if err := os.WriteFile(path, data, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "confit merge-driver:", err)
		return 2
	}
	return code
}

// lineMerge runs `git merge-file --stdout ours base theirs`.
func lineMerge(base, ours, theirs string) ([]byte, bool, error) {
	cmd := exec.Command("git", "merge-file", "--stdout", "-L", "ours", "-L", "base", "-L", "theirs", ours, base, theirs)
	out, err := cmd.Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() > 0 && ee.ExitCode() < 128 {
		return out, false, nil // exit code = number of conflicts
	}
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}
