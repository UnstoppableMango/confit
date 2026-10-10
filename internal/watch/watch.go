// Package watch is confit's optional change trigger. It waits for a live
// system to change, waits for a quiet period, and runs a capture. It keeps no
// state and has no git logic: killing it loses latency, never changes,
// because the next capture compares whole states anyway.
package watch

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/UnstoppableMango/confit/internal/adapter"
)

// Options configures Run.
type Options struct {
	// Quiet is how long the system must stay unchanged before a capture
	// runs, so a slider drag becomes one commit instead of dozens.
	Quiet time.Duration
	// Capture runs one capture. Errors are reported and watching goes on.
	Capture func() error
	// Log receives one line per event worth reporting.
	Log func(format string, args ...any)
}

// Run captures once (to pick up anything changed while nothing watched), then
// captures after every burst of changes until ctx is done.
func Run(ctx context.Context, cfg adapter.Config, opts Options) error {
	if opts.Quiet <= 0 {
		opts.Quiet = 2 * time.Second
	}
	if opts.Log == nil {
		opts.Log = func(string, ...any) {}
	}
	changes := make(chan struct{}, 1)
	signal := func() {
		select {
		case changes <- struct{}{}:
		default:
		}
	}
	var source func(context.Context, func()) error
	switch cfg.Type {
	case "dconf":
		root := cfg.Root
		if root == "" {
			root = "/"
		}
		source = func(ctx context.Context, f func()) error { return dconfWatch(ctx, root, f) }
	case "file":
		var paths []string
		for _, live := range adapter.FileMap(cfg.Files) {
			paths = append(paths, live)
		}
		source = func(ctx context.Context, f func()) error { return fileWatch(ctx, paths, f) }
	default:
		return fmt.Errorf("adapter %s: confit watch supports dconf and file adapters; trigger %s captures from a timer instead", cfg.Name, cfg.Type)
	}
	errc := make(chan error, 1)
	go func() { errc <- source(ctx, signal) }()

	run := func() {
		if err := opts.Capture(); err != nil {
			opts.Log("capture %s: %v", cfg.Name, err)
		}
	}
	run()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errc:
			if ctx.Err() != nil {
				return nil
			}
			return err
		case <-changes:
		}
		// Debounce: wait until nothing has changed for a full quiet period.
		timer := time.NewTimer(opts.Quiet)
	quiet:
		for {
			select {
			case <-ctx.Done():
				timer.Stop()
				run() // don't leave a burst uncaptured on shutdown
				return nil
			case <-changes:
				timer.Reset(opts.Quiet)
			case <-timer.C:
				break quiet
			}
		}
		run()
	}
}

// dconfWatch follows `dconf watch`, which prints each changed key and its new
// value. Every output line counts as a change; the debounce merges them.
func dconfWatch(ctx context.Context, root string, changed func()) error {
	cmd := exec.CommandContext(ctx, "dconf", "watch", root)
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("dconf watch: %w", err)
	}
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		if sc.Text() != "" {
			changed()
		}
	}
	if err := cmd.Wait(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("dconf watch exited: %w", err)
	}
	if ctx.Err() == nil {
		return errors.New("dconf watch exited")
	}
	return nil
}

// fileWatch uses inotify on each file's directory, not the file itself,
// because editors (VSCode included) often save by writing a new file and
// renaming it over the old one, which would orphan a watch on the file.
func fileWatch(ctx context.Context, paths []string, changed func()) error {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return fmt.Errorf("inotify: %w", err)
	}
	f := os.NewFile(uintptr(fd), "inotify")
	defer f.Close()
	names := map[int]map[string]bool{}
	const mask = unix.IN_CLOSE_WRITE | unix.IN_MOVED_TO | unix.IN_CREATE | unix.IN_DELETE | unix.IN_MOVED_FROM | unix.IN_ATTRIB |
		unix.IN_DELETE_SELF | unix.IN_MOVE_SELF
	for _, p := range paths {
		dir := filepath.Dir(p)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		wd, err := unix.InotifyAddWatch(fd, dir, mask)
		if err != nil {
			return fmt.Errorf("watch %s: %w", dir, err)
		}
		if names[wd] == nil {
			names[wd] = map[string]bool{}
		}
		names[wd][filepath.Base(p)] = true
	}
	go func() {
		<-ctx.Done()
		f.Close() // unblocks Read
	}()
	buf := make([]byte, 64*(unix.SizeofInotifyEvent+unix.NAME_MAX+1))
	for {
		n, err := f.Read(buf)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inotify read: %w", err)
		}
		for off := 0; off+unix.SizeofInotifyEvent <= n; {
			ev := (*unix.InotifyEvent)(unsafe.Pointer(&buf[off]))
			nameBytes := buf[off+unix.SizeofInotifyEvent : off+unix.SizeofInotifyEvent+int(ev.Len)]
			off += unix.SizeofInotifyEvent + int(ev.Len)
			switch {
			case ev.Mask&unix.IN_Q_OVERFLOW != 0:
				changed() // events were lost; a capture compares whole states anyway
			case ev.Mask&(unix.IN_IGNORED|unix.IN_DELETE_SELF|unix.IN_MOVE_SELF) != 0:
				// The directory is gone; exit so systemd restarts the watch.
				return errors.New("watched directory was removed or moved")
			case names[int(ev.Wd)][string(trimNul(nameBytes))]:
				changed()
			}
		}
	}
}

func trimNul(b []byte) []byte {
	for i, c := range b {
		if c == 0 {
			return b[:i]
		}
	}
	return b
}
