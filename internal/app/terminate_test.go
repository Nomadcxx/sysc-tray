package app

import (
	"context"
	"errors"
	"testing"

	"github.com/Nomadcxx/sysc-tray/protocol"
)

func startedApp(t *testing.T) *App {
	t.Helper()
	requireSessionBus(t)
	application := New(runtimeDir(t))
	if err := application.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	return application
}

func TestTerminateRejectsUnknownKeys(t *testing.T) {
	application := startedApp(t)

	key := protocol.ItemKey{Owner: ":1.99", ObjectPath: "/StatusNotifierItem", Generation: 7}
	err := application.Terminate(key)
	var perr *protocol.ProtocolError
	if !errors.As(err, &perr) || perr.Code != protocol.ErrorStaleItem {
		t.Fatalf("terminate of an unknown key reported %v, want stale_item", err)
	}
}

func TestTerminateRejectsUnestablishedIdentity(t *testing.T) {
	application := startedApp(t)

	key := protocol.ItemKey{Owner: ":1.98", ObjectPath: "/StatusNotifierItem", Generation: 3}
	application.mu.Lock()
	application.workers[key] = &worker{key: key}
	application.mu.Unlock()

	err := application.Terminate(key)
	var perr *protocol.ProtocolError
	if !errors.As(err, &perr) || perr.Code != protocol.ErrorInvalid {
		t.Fatalf("terminate without recorded identity reported %v, want invalid", err)
	}
}
