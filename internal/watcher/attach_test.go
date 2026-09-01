package watcher

import (
	"context"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/Nomadcxx/sysc-tray/internal/dbustest"
)

func TestWatcherAttachesToHealthyExternalWatcher(t *testing.T) {
	externalConn := privateConn(t)
	external := NewServer(externalConn, newRecorder())
	if err := external.Serve(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = external.Close() })

	existing := privateConn(t)
	registerItem(t, existing, string(DefaultItemPath))

	observer := newRecorder()
	conn := privateConn(t)
	w := New(conn, observer)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = w.Run(ctx) }()

	awaitMode(t, w, ModeAttached)
	if !hostRegistered(t, externalConn) {
		t.Fatal("attached watcher did not register a host identity")
	}
	if key := observer.awaitAdded(t); key.Owner != existing.Names()[0] {
		t.Fatalf("attached watcher reported %+v, want owner %q", key, existing.Names()[0])
	}

	later := privateConn(t)
	registerItem(t, later, string(DefaultItemPath))
	if key := observer.awaitAdded(t); key.Owner != later.Names()[0] {
		t.Fatalf("attached watcher reported %+v, want owner %q", key, later.Names()[0])
	}
	if owner := nameOwner(t, conn, BusName); owner != externalConn.Names()[0] {
		t.Fatalf("watcher name owner = %q, want the external watcher to keep it", owner)
	}
}

func TestWatcherAcquiresTheNameWhenTheExternalWatcherLeaves(t *testing.T) {
	externalConn, err := dbus.Connect(dbustest.Session(t))
	if err != nil {
		t.Fatal(err)
	}
	external := NewServer(externalConn, newRecorder())
	if err := external.Serve(); err != nil {
		t.Fatal(err)
	}

	conn := privateConn(t)
	w := New(conn, newRecorder())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = w.Run(ctx) }()
	awaitMode(t, w, ModeAttached)

	_ = external.Close()
	_ = externalConn.Close()

	awaitMode(t, w, ModeOwner)
	if owner := nameOwner(t, conn, BusName); owner != conn.Names()[0] {
		t.Fatalf("watcher name owner = %q, want %q", owner, conn.Names()[0])
	}
}

func TestWatcherStopsWhenItsContextIsCancelled(t *testing.T) {
	conn := privateConn(t)
	w := New(conn, newRecorder())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	awaitMode(t, w, ModeOwner)

	cancel()
	select {
	case err := <-done:
		if err != nil && !isCancellation(err) {
			t.Fatalf("Run() returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run() did not return after cancellation")
	}
}

func awaitMode(t *testing.T, w *Watcher, want Mode) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if w.Mode() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("watcher mode = %v, want %v", w.Mode(), want)
}

func isCancellation(err error) bool {
	return err == context.Canceled || err == context.DeadlineExceeded
}
