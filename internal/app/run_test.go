package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Nomadcxx/sysc-tray/internal/dbustest"
)

func TestAppStartsAndRemovesItsSocketOnShutdown(t *testing.T) {
	requireSessionBus(t)
	application := New(runtimeDir(t))
	if err := application.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	path := application.SocketPath()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("socket was not bound: %v", err)
	}
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("socket survived shutdown: %v", err)
	}
	if err := application.Close(); err != nil {
		t.Fatalf("second Close() reported %v", err)
	}
}

func TestAppUnwindsWhenThePresenterCannotBind(t *testing.T) {
	requireSessionBus(t)
	unsafe := t.TempDir()
	if err := os.Chmod(unsafe, 0o755); err != nil {
		t.Fatal(err)
	}
	application := New(unsafe)
	if err := application.Start(context.Background()); err == nil {
		t.Fatal("startup succeeded with an unsafe runtime directory")
	}
	// Every step taken before the failure must already be undone.
	if application.presenter != nil || application.conn != nil || application.owner != nil {
		t.Fatalf("startup left state behind: presenter=%v conn=%v owner=%v",
			application.presenter != nil, application.conn != nil, application.owner != nil)
	}
	if err := application.Close(); err != nil {
		t.Fatalf("Close() after a failed start reported %v", err)
	}

	// A service with a sound runtime directory still starts, so the failed
	// attempt released the bus and the watcher name.
	healthy := New(runtimeDir(t))
	if err := healthy.Start(context.Background()); err != nil {
		t.Fatalf("a later start failed: %v", err)
	}
	t.Cleanup(func() { _ = healthy.Close() })
}

func TestAppStopsWhenItsContextIsCancelled(t *testing.T) {
	requireSessionBus(t)
	application := New(runtimeDir(t))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- application.Run(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	for application.SocketPath() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	path := application.SocketPath()
	if path == "" {
		t.Fatal("service never bound its socket")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after cancellation")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("socket survived cancellation: %v", err)
	}
}

func runtimeDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "runtime")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func requireSessionBus(t *testing.T) {
	t.Helper()
	dbustest.Session(t)
}
