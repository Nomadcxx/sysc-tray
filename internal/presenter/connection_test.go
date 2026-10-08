package presenter

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/Nomadcxx/sysc-tray/protocol"
)

func TestConnectionStopsAtItsMessageBound(t *testing.T) {
	c, _ := loopbackConnection(t)
	if err := c.start(0); err != nil {
		t.Fatal(err)
	}
	// The writer is never started, so nothing drains the queue.
	for i := 1; i <= protocol.MaxPresenterQueueMessages; i++ {
		if err := c.Send(delta(uint64(i))); err != nil {
			t.Fatalf("message %d was refused: %v", i, err)
		}
	}
	if err := c.Send(delta(uint64(protocol.MaxPresenterQueueMessages + 1))); err == nil {
		t.Fatal("the queue accepted a message past its bound")
	}
	if err := c.Send(delta(uint64(protocol.MaxPresenterQueueMessages + 2))); err == nil {
		t.Fatal("a failed connection kept accepting messages")
	}
}

func TestConnectionStopsAtItsByteBound(t *testing.T) {
	c, _ := loopbackConnection(t)
	if err := c.start(0); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.queuedBytes = protocol.MaxPresenterDecodedBytes
	c.mu.Unlock()
	if err := c.Send(delta(1)); err == nil {
		t.Fatal("the queue accepted bytes past its bound")
	}
}

func TestConnectionRefusesSequenceGaps(t *testing.T) {
	c, _ := loopbackConnection(t)
	if err := c.start(4); err != nil {
		t.Fatal(err)
	}
	if err := c.Send(delta(6)); err == nil {
		t.Fatal("a sequence gap was accepted")
	}
}

func TestConnectionDropsPendingDeltasCoveredByTheSnapshot(t *testing.T) {
	c, _ := loopbackConnection(t)
	// Deltas that arrive while the snapshot is being written are filtered
	// against the baseline, so the shell never applies one twice.
	if err := c.Send(delta(3)); err != nil {
		t.Fatal(err)
	}
	if err := c.Send(delta(4)); err != nil {
		t.Fatal(err)
	}
	if err := c.start(3); err != nil {
		t.Fatal(err)
	}
	if got := len(c.queue); got != 1 {
		t.Fatalf("queued %d messages after the baseline, want only the newer delta", got)
	}
}

func TestDispatchReturnsTypedOutcomes(t *testing.T) {
	key := protocol.ItemKey{Owner: ":1.2", ObjectPath: "/Item", Generation: 1}
	commands := &recordingCommands{}

	reply := dispatch(commands, protocol.Command{Kind: protocol.CommandActivate, Item: key, Output: 3})
	if !reply.OK || reply.Error != nil || reply.Output != 3 {
		t.Fatalf("successful dispatch = %+v", reply)
	}

	reply = dispatch(commands, protocol.Command{Kind: "teleport", Item: key})
	if reply.OK || reply.Error == nil || reply.Error.Code != protocol.ErrorInvalid {
		t.Fatalf("unknown command = %+v", reply)
	}

	commands.fail = errors.New("bus timeout")
	reply = dispatch(commands, protocol.Command{Kind: protocol.CommandMenuClose, Item: key, MenuRevision: 2})
	if reply.OK || reply.Error == nil || reply.Error.Code != protocol.ErrorUnavailable {
		t.Fatalf("untyped failure = %+v", reply)
	}

	commands.fail = &protocol.ProtocolError{Code: protocol.ErrorStaleItem}
	reply = dispatch(commands, protocol.Command{Kind: protocol.CommandMenuOpen, Item: key, MenuRevision: 2})
	if reply.OK || reply.Error == nil || reply.Error.Code != protocol.ErrorStaleItem {
		t.Fatalf("typed failure = %+v", reply)
	}

	reply = dispatch(nil, protocol.Command{Kind: protocol.CommandActivate, Item: key})
	if reply.OK || reply.Error == nil || reply.Error.Code != protocol.ErrorUnavailable {
		t.Fatalf("dispatch without a handler = %+v", reply)
	}
}

// loopbackConnection returns a connection over a real Unix socket pair, so the
// queue bounds are exercised against the same socket type the server uses.
func loopbackConnection(t *testing.T) (*connection, *net.UnixConn) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	left := unixConn(t, fds[0], "left")
	right := unixConn(t, fds[1], "right")
	return newConnection(left, 1), right
}

func unixConn(t *testing.T, fd int, name string) *net.UnixConn {
	t.Helper()
	file := os.NewFile(uintptr(fd), name)
	conn, err := net.FileConn(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	unixSocket, ok := conn.(*net.UnixConn)
	if !ok {
		t.Fatalf("socket pair produced %T", conn)
	}
	t.Cleanup(func() { _ = unixSocket.Close() })
	return unixSocket
}

func delta(sequence uint64) protocol.Envelope {
	payload, _ := json.Marshal(protocol.ItemRemoved{Key: protocol.ItemKey{
		Owner: ":1.2", ObjectPath: "/Item", Generation: 1,
	}})
	return protocol.Envelope{Kind: protocol.KindItemRemoved, Sequence: sequence, Payload: payload}
}
