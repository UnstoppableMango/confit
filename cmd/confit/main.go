// Command confit is Confit's CLI, the public contract for editors, adapters
// and consumers. Every command is one-shot; nothing needs a daemon.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/UnstoppableMango/confit/internal/adapter"
	"github.com/UnstoppableMango/confit/internal/core"
	"github.com/UnstoppableMango/confit/internal/gitrepo"
	"github.com/UnstoppableMango/confit/internal/mergedriver"
	"github.com/UnstoppableMango/confit/internal/watch"
)

const usage = `usage: confit [-C <repo>] <command> [args]

  init [--bare] [--integration NAME] [--driver-command CMD] [path]
  adapter add <name> --type dconf|file|<external> [--path DIR] [--file [NAME=]PATH]...
                     [--root /dconf/root/] [--command CMD] [--editor NAME] [--no-integrate]
  consumer add <name> [--source BRANCH] [--adapter NAME]... [--drift adopt|revert|block]
  capture <adapter> [--no-integrate] [--json]
  watch <adapter> [--quiet DURATION]
  edit commit <editor> --dir DIR --prefix PATH [--message MSG] [--session ID] [--no-integrate] [--json]
  integrate [<editor>...] [--json]
  pending <consumer> [--json]
  prepare <consumer> [--json]
  apply <consumer> [--force] [--json]
  applied advance <consumer> <commit> [--expect OLD] [--result TEXT] [--json]
  status [--json]
  merge-driver json|ini %O %A %B [%P]
  version

The repository is -C, else $CONFIT_REPO, else the current directory.
Exit codes: 0 ok, 1 error, 2 usage, 3 conflict or blocked by drift policy.`

const (
	exitErr      = 1
	exitUsage    = 2
	exitConflict = 3
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	repo := os.Getenv("CONFIT_REPO")
	if repo == "" {
		repo = "."
	}
	if len(args) >= 2 && args[0] == "-C" {
		repo, args = args[1], args[2:]
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		return exitUsage
	}
	cmd, args := args[0], args[1:]
	if len(args) > 0 && (cmd == "adapter" || cmd == "consumer" || cmd == "edit" || cmd == "applied") {
		cmd, args = cmd+" "+args[0], args[1:]
	}
	switch cmd {
	case "version":
		fmt.Println("confit", core.Version)
		return 0
	case "help", "-h", "--help":
		fmt.Println(usage)
		return 0
	case "merge-driver":
		if len(args) < 4 {
			return usageErr("merge-driver needs: json|ini %O %A %B")
		}
		return mergedriver.Run(args[0], args[1], args[2], args[3])
	case "init":
		return cmdInit(repo, args)
	}
	b, err := core.Open(repo)
	if err != nil {
		return fail(err)
	}
	switch cmd {
	case "adapter add":
		return cmdAdapterAdd(b, args)
	case "consumer add":
		return cmdConsumerAdd(b, args)
	case "capture":
		return cmdCapture(b, args)
	case "watch":
		return cmdWatch(b, repo, args)
	case "edit commit":
		return cmdEditCommit(b, args)
	case "integrate":
		return cmdIntegrate(b, args)
	case "pending":
		return cmdPending(b, args)
	case "prepare":
		return cmdPrepare(b, args)
	case "apply":
		return cmdApply(b, args)
	case "applied advance":
		return cmdAdvance(b, args)
	case "status":
		return cmdStatus(b, args)
	}
	return usageErr("unknown command: " + cmd)
}

// parse parses flags that may appear before, between or after positionals.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(os.Stderr)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func usageErr(msg string) int {
	fmt.Fprintln(os.Stderr, "confit:", msg)
	fmt.Fprintln(os.Stderr, usage)
	return exitUsage
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "confit:", err)
	return exitErr
}

func emitJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func cmdInit(repo string, args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	bare := fs.Bool("bare", false, "create a bare repository")
	integ := fs.String("integration", "", "integration branch name (default desired)")
	driver := fs.String("driver-command", "", "command git uses to run confit's merge drivers (default confit)")
	pos, err := parse(fs, args)
	if err != nil {
		return exitUsage
	}
	path := repo
	if len(pos) > 0 {
		path = pos[0]
	}
	b, err := core.Init(path, core.InitOptions{Bare: *bare, Integration: *integ, DriverCommand: *driver})
	if err != nil {
		return fail(err)
	}
	fmt.Printf("initialized %s (integration branch %s)\n", b.Repo.GitDir, b.Config.Integration)
	return 0
}

func cmdAdapterAdd(b *core.Buffer, args []string) int {
	fs := flag.NewFlagSet("adapter add", flag.ContinueOnError)
	typ := fs.String("type", "", "dconf, file, or the name of an external confit-adapter-<type>")
	path := fs.String("path", "", "directory in the repo (default: adapter name)")
	root := fs.String("root", "", "dconf: subtree to manage (default /)")
	command := fs.String("command", "", "external adapter executable")
	editor := fs.String("editor", "", "editor branch suffix (default <name>@<host>)")
	noInt := fs.Bool("no-integrate", false, "don't integrate captures automatically")
	var files multi
	fs.Var(&files, "file", "file adapter: [NAME=]PATH, repeatable")
	pos, err := parse(fs, args)
	if err != nil || len(pos) != 1 || *typ == "" {
		return usageErr("adapter add <name> --type T")
	}
	name := pos[0]
	cfg := adapter.Config{Name: name, Type: *typ, Files: files, Root: *root, Command: *command}
	if _, err := adapter.New(cfg); err != nil {
		return fail(err)
	}
	set := map[string][]string{"type": {*typ}, "path": {*path}, "root": {*root}, "command": {*command}, "editor": {*editor}, "file": files}
	if *noInt {
		set["integrate"] = []string{"false"}
	}
	for field, vals := range set {
		if len(vals) == 0 || vals[0] == "" {
			continue
		}
		if err := b.Repo.SetConfig("confit.adapter."+name+"."+field, vals...); err != nil {
			return fail(err)
		}
	}
	fmt.Printf("adapter %s (%s) added\n", name, *typ)
	return 0
}

func cmdConsumerAdd(b *core.Buffer, args []string) int {
	fs := flag.NewFlagSet("consumer add", flag.ContinueOnError)
	source := fs.String("source", "", "branch to follow (default: integration branch)")
	drift := fs.String("drift", "adopt", "drift policy: adopt, revert or block")
	var adapters multi
	fs.Var(&adapters, "adapter", "adapter this consumer writes, repeatable")
	pos, err := parse(fs, args)
	if err != nil || len(pos) != 1 {
		return usageErr("consumer add <name>")
	}
	switch core.DriftPolicy(*drift) {
	case core.Adopt, core.Revert, core.Block:
	default:
		return usageErr("--drift must be adopt, revert or block")
	}
	for _, a := range adapters {
		if _, ok := b.Config.Adapters[a]; !ok {
			return fail(fmt.Errorf("no adapter %q", a))
		}
	}
	name := pos[0]
	if *source != "" {
		if err := b.Repo.SetConfig("confit.consumer."+name+".source", *source); err != nil {
			return fail(err)
		}
	}
	if err := b.Repo.SetConfig("confit.consumer."+name+".drift", *drift); err != nil {
		return fail(err)
	}
	if err := b.Repo.SetConfig("confit.consumer."+name+".adapter", adapters...); err != nil {
		return fail(err)
	}
	fmt.Printf("consumer %s added (drift policy %s)\n", name, *drift)
	return 0
}

func printCapture(c core.CaptureResult) int {
	if !c.Edit.Changed {
		fmt.Println("no changes")
		return 0
	}
	fmt.Printf("%s %s\n", c.Edit.Commit[:12], c.Edit.Summary)
	if c.Integrated != nil {
		return printIntegrate([]core.IntegrateResult{*c.Integrated})
	}
	return 0
}

func cmdCapture(b *core.Buffer, args []string) int {
	fs := flag.NewFlagSet("capture", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	noInt := fs.Bool("no-integrate", false, "commit to the editor branch only")
	pos, err := parse(fs, args)
	if err != nil || len(pos) != 1 {
		return usageErr("capture <adapter>")
	}
	res, err := b.Capture(pos[0], !*noInt)
	if err != nil {
		return fail(err)
	}
	if *asJSON {
		emitJSON(res)
		return conflictCode(res.Integrated)
	}
	return printCapture(res)
}

// cmdWatch is the optional trigger: it runs `confit capture` after each burst
// of live changes. The capture is a separate process, so the watcher holds no
// git state and a crash in either loses nothing.
func cmdWatch(b *core.Buffer, repo string, args []string) int {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	quiet := fs.Duration("quiet", 2*time.Second, "capture once the system has been unchanged this long")
	pos, err := parse(fs, args)
	if err != nil || len(pos) != 1 {
		return usageErr("watch <adapter>")
	}
	cfg, ok := b.Config.Adapters[pos[0]]
	if !ok {
		return fail(fmt.Errorf("no adapter %q", pos[0]))
	}
	self, err := os.Executable()
	if err != nil {
		return fail(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = watch.Run(ctx, cfg, watch.Options{
		Quiet: *quiet,
		Capture: func() error {
			cmd := exec.Command(self, "-C", repo, "capture", cfg.Name)
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
			err := cmd.Run()
			var exit *exec.ExitError
			if errors.As(err, &exit) && exit.ExitCode() == exitConflict {
				return nil // reported by capture; the editor waits for a human
			}
			return err
		},
		Log: func(format string, args ...any) { fmt.Fprintf(os.Stderr, "confit watch: "+format+"\n", args...) },
	})
	if err != nil {
		return fail(err)
	}
	return 0
}

func conflictCode(r *core.IntegrateResult) int {
	if r != nil && r.Status == core.Conflict {
		return exitConflict
	}
	return 0
}

func cmdEditCommit(b *core.Buffer, args []string) int {
	fs := flag.NewFlagSet("edit commit", flag.ContinueOnError)
	dir := fs.String("dir", "", "directory holding the editor's full content")
	prefix := fs.String("prefix", "", "path in the repo the directory maps to (required)")
	msg := fs.String("message", "", "commit summary (default: generated)")
	session := fs.String("session", "", "groups commits from one UI session")
	noInt := fs.Bool("no-integrate", false, "commit to the editor branch only")
	asJSON := fs.Bool("json", false, "machine-readable output")
	pos, err := parse(fs, args)
	if err != nil || len(pos) != 1 || *dir == "" {
		return usageErr("edit commit <editor> --dir DIR")
	}
	files, err := adapter.ReadDir(*dir)
	if err != nil {
		return fail(err)
	}
	res, err := b.EditCommit(pos[0], core.EditOptions{Prefix: *prefix, Files: files, Message: *msg, Session: *session, Integrate: !*noInt})
	if err != nil {
		return fail(err)
	}
	if *asJSON {
		emitJSON(res)
		return conflictCode(res.Integrated)
	}
	return printCapture(res)
}

func printIntegrate(rs []core.IntegrateResult) int {
	code := 0
	for _, r := range rs {
		switch r.Status {
		case core.Conflict:
			code = exitConflict
			fmt.Printf("%s: conflict in %s (left unintegrated; resolve with git and rerun confit integrate)\n", r.Editor, strings.Join(r.Conflicts, ", "))
		case core.UpToDate:
			fmt.Printf("%s: up to date\n", r.Editor)
		case "":
			// failed before an outcome; the caller reports the error
		default:
			fmt.Printf("%s: %s -> %s\n", r.Editor, r.Status, r.Commit[:12])
		}
	}
	return code
}

func cmdIntegrate(b *core.Buffer, args []string) int {
	fs := flag.NewFlagSet("integrate", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	pos, err := parse(fs, args)
	if err != nil {
		return exitUsage
	}
	rs, err := b.Integrate(pos...)
	if *asJSON {
		emitJSON(rs)
	} else if code := printIntegrate(rs); code != 0 && err == nil {
		return code
	}
	if err != nil {
		return fail(err)
	}
	for _, r := range rs {
		if r.Status == core.Conflict {
			return exitConflict
		}
	}
	return 0
}

func cmdPending(b *core.Buffer, args []string) int {
	fs := flag.NewFlagSet("pending", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	pos, err := parse(fs, args)
	if err != nil || len(pos) != 1 {
		return usageErr("pending <consumer>")
	}
	p, err := b.Pending(pos[0])
	if err != nil {
		return fail(err)
	}
	if *asJSON {
		emitJSON(p)
		return 0
	}
	if p.Empty && len(p.Commits) == 0 {
		fmt.Printf("%s is up to date with %s\n", p.Consumer, p.Source)
		return 0
	}
	fmt.Printf("%s: %d commits, %d files to apply (target %s)\n", p.Consumer, len(p.Commits), len(p.Files), p.Target[:12])
	for _, c := range p.Commits {
		fmt.Printf("  %s %s\n", c.Commit[:12], c.Subject)
	}
	for _, f := range p.Files {
		fmt.Printf("  %s %s\n", f.Status, f.Path)
	}
	return 0
}

func printPrepare(w io.Writer, p core.PrepareResult) {
	for _, d := range p.Drift {
		fmt.Fprintf(w, "drift: %s %s\n", d.Commit[:12], d.Summary)
	}
	if p.Reason != "" {
		fmt.Fprintln(w, p.Reason)
	}
}

func cmdPrepare(b *core.Buffer, args []string) int {
	fs := flag.NewFlagSet("prepare", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	pos, err := parse(fs, args)
	if err != nil || len(pos) != 1 {
		return usageErr("prepare <consumer>")
	}
	p, err := b.Prepare(pos[0])
	if err != nil {
		return fail(err)
	}
	if *asJSON {
		emitJSON(p)
	} else {
		// Only the target goes to stdout, so `target=$(confit prepare c)` works.
		printPrepare(os.Stderr, p)
		if !p.Blocked {
			fmt.Println(p.Target)
		}
	}
	if p.Blocked {
		return exitConflict
	}
	return 0
}

func cmdApply(b *core.Buffer, args []string) int {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	force := fs.Bool("force", false, "apply even when the drift policy blocks")
	pos, err := parse(fs, args)
	if err != nil || len(pos) != 1 {
		return usageErr("apply <consumer>")
	}
	res, err := b.Apply(pos[0], *force)
	if *asJSON {
		emitJSON(res)
	} else {
		printPrepare(os.Stdout, res.Prepare)
		if err == nil {
			fmt.Printf("applied %s\n", res.Applied[:12])
		}
	}
	if errors.Is(err, core.ErrBlocked) {
		fmt.Fprintln(os.Stderr, "confit:", err)
		return exitConflict
	}
	if err != nil {
		return fail(err)
	}
	return 0
}

func cmdAdvance(b *core.Buffer, args []string) int {
	fs := flag.NewFlagSet("applied advance", flag.ContinueOnError)
	expect := fs.String("expect", "", "fail unless the pointer is currently here")
	result := fs.String("result", "ok", "result recorded in the apply note")
	asJSON := fs.Bool("json", false, "machine-readable output")
	pos, err := parse(fs, args)
	if err != nil || len(pos) != 2 {
		return usageErr("applied advance <consumer> <commit>")
	}
	h, err := b.Advance(pos[0], pos[1], *expect, *result)
	if errors.Is(err, gitrepo.ErrRefChanged) {
		fmt.Fprintln(os.Stderr, "confit: applied pointer moved concurrently:", err)
		return exitConflict
	}
	if err != nil {
		return fail(err)
	}
	if *asJSON {
		emitJSON(map[string]string{"consumer": pos[0], "applied": h})
	} else {
		fmt.Printf("%s%s -> %s\n", b.Config.AppliedPrefix, pos[0], h[:12])
	}
	return 0
}

func cmdStatus(b *core.Buffer, args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	if _, err := parse(fs, args); err != nil {
		return exitUsage
	}
	s, err := b.Status()
	if err != nil {
		return fail(err)
	}
	if *asJSON {
		emitJSON(s)
		return 0
	}
	fmt.Printf("%s at %s\n", s.Integration, s.Tip[:12])
	for _, e := range s.Editors {
		switch {
		case len(e.Conflicts) > 0:
			fmt.Printf("  editor %s: %d unintegrated, CONFLICT in %s\n", e.Editor, e.Unintegrated, strings.Join(e.Conflicts, ", "))
		case e.Unintegrated > 0 && e.Changes:
			fmt.Printf("  editor %s: %d unintegrated commits\n", e.Editor, e.Unintegrated)
		case e.Unintegrated > 0:
			fmt.Printf("  editor %s: %d unintegrated commits, no content changes\n", e.Editor, e.Unintegrated)
		default:
			fmt.Printf("  editor %s: integrated\n", e.Editor)
		}
	}
	for _, c := range s.Consumers {
		applied := "never applied"
		if c.Applied != "" {
			applied = "applied " + c.Applied[:12]
		}
		fmt.Printf("  consumer %s (%s, %s): %s, %d pending commits, %d files", c.Consumer, c.Source, c.Policy, applied, c.Pending, c.Files)
		if len(c.Drift) > 0 {
			fmt.Printf(", drift in %s", strings.Join(c.Drift, ", "))
		}
		fmt.Println()
	}
	return 0
}
