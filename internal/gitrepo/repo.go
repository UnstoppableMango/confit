// Package gitrepo is gb's only door to git.
//
// The split follows the language spike (spike/README.md):
//   - go-git reads refs and objects and writes blobs, trees and commits.
//   - every ref write goes through `git update-ref --stdin`, because go-git's
//     own compare-and-swap is not safe against concurrent git processes.
//   - merges go through `git merge-tree`, so merge drivers behave exactly as
//     they do for the user's own git tools.
package gitrepo

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// ZeroHash is the hash used for "ref does not exist".
var ZeroHash = plumbing.ZeroHash

// ErrRefChanged means a compare-and-swap lost: some ref was not at the
// expected value. Callers re-read and retry, or report.
var ErrRefChanged = errors.New("ref changed concurrently")

type Hash = plumbing.Hash

// NewHash parses a full hex object id.
func NewHash(s string) Hash { return plumbing.NewHash(s) }

// Repo is a git repository, bare or not. gb never touches a worktree.
type Repo struct {
	GitDir string
	r      *git.Repository
}

// Open finds the repository containing path.
func Open(path string) (*Repo, error) {
	out, err := run(path, nil, nil, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, fmt.Errorf("not a git repository: %s", path)
	}
	gitDir := strings.TrimSpace(out)
	r, err := git.PlainOpenWithOptions(gitDir, &git.PlainOpenOptions{})
	if err != nil {
		return nil, err
	}
	return &Repo{GitDir: gitDir, r: r}, nil
}

// Init creates a bare repository whose HEAD names branch.
func Init(path, branch string, bare bool) error {
	args := []string{"init", "-q", "-b", branch}
	if bare {
		args = append(args, "--bare")
	}
	args = append(args, path)
	_, err := run("/", nil, nil, args...)
	return err
}

// Git runs git against this repository and returns stdout.
func (r *Repo) Git(args ...string) (string, error) {
	return run(r.GitDir, nil, nil, args...)
}

// GitInput runs git with stdin.
func (r *Repo) GitInput(stdin string, args ...string) (string, error) {
	return run(r.GitDir, nil, strings.NewReader(stdin), args...)
}

// GitError carries the exit code and stderr of a failed git command.
type GitError struct {
	Args   []string
	Code   int
	Stderr string
}

func (e *GitError) Error() string {
	return fmt.Sprintf("git %s: exit %d: %s", strings.Join(e.Args, " "), e.Code, strings.TrimSpace(e.Stderr))
}

func run(dir string, env []string, stdin *strings.Reader, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		code := -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		return out.String(), &GitError{Args: args, Code: code, Stderr: errb.String()}
	}
	return out.String(), nil
}

// Resolve returns the commit a full ref name points at, or ZeroHash.
func (r *Repo) Resolve(name string) (Hash, error) {
	ref, err := r.r.Reference(plumbing.ReferenceName(name), true)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return ZeroHash, nil
	}
	if err != nil {
		return ZeroHash, err
	}
	return ref.Hash(), nil
}

// ResolveRev resolves any revision expression (abbreviated hash, branch...).
func (r *Repo) ResolveRev(rev string) (Hash, error) {
	out, err := r.Git("rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err != nil {
		return ZeroHash, fmt.Errorf("unknown revision %q", rev)
	}
	return plumbing.NewHash(strings.TrimSpace(out)), nil
}

// ListRefs returns full ref name -> hash for refs under prefix.
func (r *Repo) ListRefs(prefix string) (map[string]Hash, error) {
	out, err := r.Git("for-each-ref", "--format=%(objectname) %(refname)", prefix)
	if err != nil {
		return nil, err
	}
	refs := map[string]Hash{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		h, name, ok := strings.Cut(l, " ")
		if ok {
			refs[name] = plumbing.NewHash(h)
		}
	}
	return refs, nil
}

// RefUpdate is one compare-and-swap in a transaction. Old == ZeroHash means
// the ref must not exist yet.
type RefUpdate struct {
	Name string
	New  Hash
	Old  Hash
}

// UpdateRefs applies all updates atomically, or none of them. It returns
// ErrRefChanged when any ref was not at its expected old value.
func (r *Repo) UpdateRefs(msg string, ups ...RefUpdate) error {
	var b strings.Builder
	b.WriteString("start\n")
	for _, u := range ups {
		if u.Old.IsZero() {
			fmt.Fprintf(&b, "create %s %s\n", u.Name, u.New)
		} else {
			fmt.Fprintf(&b, "update %s %s %s\n", u.Name, u.New, u.Old)
		}
	}
	b.WriteString("prepare\ncommit\n")
	_, err := r.GitInput(b.String(), "update-ref", "-m", msg, "--stdin")
	var ge *GitError
	if errors.As(err, &ge) {
		s := ge.Stderr
		if strings.Contains(s, "cannot lock ref") || strings.Contains(s, "but expected") ||
			strings.Contains(s, "already exists") || strings.Contains(s, "unable to resolve reference") {
			return fmt.Errorf("%w: %s", ErrRefChanged, strings.TrimSpace(s))
		}
	}
	return err
}

// IsAncestor reports whether a is an ancestor of (or equal to) b.
func (r *Repo) IsAncestor(a, b Hash) (bool, error) {
	if a == b {
		return true, nil
	}
	_, err := r.Git("merge-base", "--is-ancestor", a.String(), b.String())
	var ge *GitError
	if errors.As(err, &ge) && ge.Code == 1 {
		return false, nil
	}
	return err == nil, err
}

// RootCommit returns the oldest parentless commit reachable from tip.
func (r *Repo) RootCommit(tip Hash) (Hash, error) {
	out, err := r.Git("rev-list", "--max-parents=0", tip.String())
	if err != nil {
		return ZeroHash, err
	}
	lines := strings.Fields(out)
	if len(lines) == 0 {
		return ZeroHash, fmt.Errorf("no root commit below %s", tip)
	}
	return plumbing.NewHash(lines[len(lines)-1]), nil
}

// MergeResult is the outcome of a tree-level merge.
type MergeResult struct {
	Tree      Hash
	Clean     bool
	Conflicts []string
}

// MergeTree merges theirs into ours without a worktree. Attributes (and so
// merge drivers) come from attrTree, never from HEAD: in a bare repository
// git would otherwise read .gitattributes from whatever HEAD happens to be.
func (r *Repo) MergeTree(ours, theirs Hash, attrTree string) (MergeResult, error) {
	out, err := r.Git("-c", "attr.tree="+attrTree, "merge-tree", "--write-tree", "--name-only", "--no-messages", ours.String(), theirs.String())
	var ge *GitError
	clean := err == nil
	if err != nil && !(errors.As(err, &ge) && ge.Code == 1) {
		return MergeResult{}, err
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	res := MergeResult{Tree: plumbing.NewHash(lines[0]), Clean: clean}
	for _, l := range lines[1:] {
		if l != "" {
			res.Conflicts = append(res.Conflicts, l)
		}
	}
	return res, nil
}

// RevList returns commits in range (e.g. "a..b" or "b"), newest first.
func (r *Repo) RevList(args ...string) ([]Hash, error) {
	out, err := r.Git(append([]string{"rev-list"}, args...)...)
	if err != nil {
		return nil, err
	}
	var hs []Hash
	for _, f := range strings.Fields(out) {
		hs = append(hs, plumbing.NewHash(f))
	}
	return hs, nil
}

// Lock takes the per-repository lock that serializes capture and apply. It
// is an flock, so it is released if the process dies.
func (r *Repo) Lock() (unlock func(), err error) {
	f, err := os.OpenFile(filepath.Join(r.GitDir, "gitbuffer.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

// Config reads every key under section (e.g. "gitbuffer") as
// lowercased-key -> values, in file order.
func (r *Repo) Config(section string) (map[string][]string, error) {
	out, err := r.Git("config", "-z", "--get-regexp", "^"+section+`\.`)
	var ge *GitError
	if errors.As(err, &ge) && ge.Code == 1 {
		return map[string][]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	cfg := map[string][]string{}
	for _, rec := range strings.Split(out, "\x00") {
		if rec == "" {
			continue
		}
		k, v, _ := strings.Cut(rec, "\n")
		cfg[k] = append(cfg[k], v)
	}
	return cfg, nil
}

// ConfigValue returns one config value or "".
func (r *Repo) ConfigValue(key string) string {
	out, err := r.Git("config", "--get", key)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// SetConfig sets (replaces) a config key; with several values the key
// becomes multi-valued.
func (r *Repo) SetConfig(key string, values ...string) error {
	r.Git("config", "--unset-all", key)
	for _, v := range values {
		if _, err := r.Git("config", "--add", key, v); err != nil {
			return err
		}
	}
	return nil
}
