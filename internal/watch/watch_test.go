package watch

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/UnstoppableMango/confit/internal/adapter"
)

func TestFileWatchDebounces(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "User", "settings.json")
	other := filepath.Join(dir, "User", "keybindings.json")
	var captures atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() {
		done <- Run(ctx, adapter.Config{Name: "vscode", Type: "file", Files: []string{"settings.json=" + live}}, Options{
			Quiet:   100 * time.Millisecond,
			Capture: func() error { captures.Add(1); return nil },
		})
	}()
	wait := func(want int32) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for captures.Load() != want {
			if time.Now().After(deadline) {
				t.Fatalf("captures = %d, want %d", captures.Load(), want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	wait(1) // the startup capture
	time.Sleep(50 * time.Millisecond)

	for i := 0; i < 10; i++ { // one burst of saves
		os.WriteFile(live, []byte{'0' + byte(i)}, 0o644)
		time.Sleep(10 * time.Millisecond)
	}
	wait(2)
	os.WriteFile(other, []byte("[]"), 0o644) // not watched
	tmp := live + ".tmp"                     // save by rename
	os.WriteFile(tmp, []byte("x"), 0o644)
	os.Rename(tmp, live)
	wait(3)
	time.Sleep(300 * time.Millisecond)
	if n := captures.Load(); n != 3 {
		t.Fatalf("captures = %d after an unrelated write, want 3", n)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestUnsupportedAdapter(t *testing.T) {
	err := Run(context.Background(), adapter.Config{Name: "k8s", Type: "kubectl"}, Options{Capture: func() error { return nil }})
	if err == nil {
		t.Fatal("want an error for an adapter type with no watcher")
	}
}
