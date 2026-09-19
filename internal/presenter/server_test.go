package presenter

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Nomadcxx/sysc-tray/internal/state"
	"github.com/Nomadcxx/sysc-tray/protocol"
)

func TestServerCreatesPrivateRuntimePaths(t *testing.T) {
	server, _, _ := serve(t)
	path := server.SocketPath()
	if filepath.Base(path) != socketName {
		t.Fatalf("socket path = %q", path)
	}
	socket, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if socket.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %o, want 0600", socket.Mode().Perm())
	}
	directory, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if directory.Mode().Perm() != 0o700 {
		t.Fatalf("runtime directory mode = %o, want 0700", directory.Mode().Perm())
	}
}

func TestServerRefusesUnsafeRuntimeDirectories(t *testing.T) {
	owner := state.Start()
	t.Cleanup(func() { _ = owner.Close() })

	loose := t.TempDir()
	if err := os.Chmod(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := NewAt(loose).Serve(owner, nil); err == nil {
		t.Fatal("a world-readable runtime directory was accepted")
	}

	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := NewAt(link).Serve(owner, nil); err == nil {
		t.Fatal("a symlinked runtime directory was accepted")
	}

	if err := NewAt("").Serve(owner, nil); err == nil {
		t.Fatal("an empty runtime directory was accepted")
	}
}

func TestServerRejectsForeignPeersBeforeReadingHello(t *testing.T) {
	server, _, _ := serve(t)
	server.peerUID.Store(server.uid + 1)

	conn := dial(t, server)
	// The write itself may fail: the peer check runs before the hello is read,
	// so the socket can already be closed. Either way nothing is served.
	frame, err := json.Marshal(helloEnvelope(protocol.ProtocolMajor, RequiredCapability))
	if err != nil {
		t.Fatal(err)
	}
	_ = protocol.WriteFrame(conn, frame)
	if _, err := readEnvelopeWithin(conn, time.Second); err == nil {
		t.Fatal("a foreign peer was served")
	}
}

func TestServerRejectsIncompatibleHandshakes(t *testing.T) {
	server, _, _ := serve(t)

	wrongMajor := dial(t, server)
	writeEnvelope(t, wrongMajor, helloEnvelope(protocol.ProtocolMajor+1, RequiredCapability))
	if _, err := readEnvelopeWithin(wrongMajor, time.Second); err == nil {
		t.Fatal("an incompatible major version was served")
	}

	noCapability := dial(t, server)
	writeEnvelope(t, noCapability, helloEnvelope(protocol.ProtocolMajor, "something-else"))
	if _, err := readEnvelopeWithin(noCapability, time.Second); err == nil {
		t.Fatal("a presenter without the required capability was served")
	}
}

func TestServerStreamsSnapshotThenContiguousDeltas(t *testing.T) {
	server, owner, _ := serve(t)

	key := itemKey(1)
	owner.Add(key)
	owner.Update(key, sample(key, "First"))

	conn := dial(t, server)
	snapshot := handshake(t, conn)
	if len(snapshot.Items) != 1 || snapshot.Items[0].Title != "First" {
		t.Fatalf("snapshot = %+v", snapshot.Items)
	}

	second := itemKey(2)
	second.Owner = ":1.8"
	owner.Add(second)
	owner.Update(second, sample(second, "Second"))
	owner.Update(second, sample(second, "Second (2)"))

	previous := snapshot.Sequence
	for _, want := range []string{protocol.KindItemAdded, protocol.KindItemChanged} {
		envelope := readEnvelope(t, conn)
		if envelope.Kind != want {
			t.Fatalf("delta kind = %q, want %q", envelope.Kind, want)
		}
		if err := protocol.ValidateNextSequence(previous, envelope.Sequence); err != nil {
			t.Fatal(err)
		}
		previous = envelope.Sequence
	}
}

func TestServerReplacesThePresenterGeneration(t *testing.T) {
	server, owner, _ := serve(t)
	key := itemKey(1)
	owner.Add(key)
	owner.Update(key, sample(key, "Chat"))

	first := dial(t, server)
	handshake(t, first)

	second := dial(t, server)
	if snapshot := handshake(t, second); len(snapshot.Items) != 1 {
		t.Fatalf("replacement snapshot = %+v", snapshot.Items)
	}

	if _, err := readEnvelopeWithin(first, 2*time.Second); err == nil {
		t.Fatal("the replaced presenter was left connected")
	}

	owner.Update(key, sample(key, "Chat (2)"))
	if envelope := readEnvelope(t, second); envelope.Kind != protocol.KindItemChanged {
		t.Fatalf("replacement received %q", envelope.Kind)
	}
}

func TestServerRepliesToEachCommandWithItsRequestID(t *testing.T) {
	server, owner, commands := serve(t)
	key := itemKey(1)
	owner.Add(key)
	owner.Update(key, sample(key, "Chat"))
	conn := dial(t, server)
	handshake(t, conn)

	writeEnvelope(t, conn, commandEnvelope(t, 7, protocol.Command{
		Kind: protocol.CommandActivate, Item: key, Output: 2, Serial: 3, X: 10, Y: 20,
	}))
	reply, envelope := readReply(t, conn)
	if envelope.RequestID != 7 {
		t.Fatalf("reply request ID = %d, want 7", envelope.RequestID)
	}
	if !reply.OK || reply.Error != nil || reply.Item != key || reply.Output != 2 {
		t.Fatalf("reply = %+v", reply)
	}

	commands.fail = &protocol.ProtocolError{Code: protocol.ErrorStaleRevision, Message: "changed"}
	writeEnvelope(t, conn, commandEnvelope(t, 8, protocol.Command{
		Kind: protocol.CommandMenuSelect, Item: key, MenuRevision: 4, MenuID: 1,
	}))
	reply, envelope = readReply(t, conn)
	if envelope.RequestID != 8 {
		t.Fatalf("reply request ID = %d, want 8", envelope.RequestID)
	}
	if reply.OK || reply.Error == nil || reply.Error.Code != protocol.ErrorStaleRevision {
		t.Fatalf("typed failure was not returned: %+v", reply)
	}

	writeEnvelope(t, conn, commandEnvelope(t, 9, protocol.Command{
		Kind: protocol.CommandScroll, Item: key, Delta: 0, Orientation: protocol.ScrollVertical,
	}))
	reply, _ = readReply(t, conn)
	if reply.OK || reply.Error == nil || reply.Error.Code != protocol.ErrorInvalid {
		t.Fatalf("an invalid command was accepted: %+v", reply)
	}
	if got := commands.calls(); len(got) != 2 {
		t.Fatalf("handler saw %v", got)
	}
}

func TestServerClosesOnReplayedOrMalformedInput(t *testing.T) {
	server, _, _ := serve(t)

	replay := dial(t, server)
	handshake(t, replay)
	command := commandEnvelope(t, 4, protocol.Command{
		Kind: protocol.CommandActivate, Item: itemKey(1), Output: 1, Serial: 1,
	})
	writeEnvelope(t, replay, command)
	readReply(t, replay)
	writeEnvelope(t, replay, command)
	if _, err := readEnvelopeWithin(replay, 2*time.Second); err == nil {
		t.Fatal("a replayed request ID was accepted")
	}

	malformed := dial(t, server)
	handshake(t, malformed)
	if err := protocol.WriteFrame(malformed, []byte(`{"kind":"command","request_id":1,`)); err != nil {
		t.Fatal(err)
	}
	if _, err := readEnvelopeWithin(malformed, 2*time.Second); err == nil {
		t.Fatal("a malformed frame was accepted")
	}

	unknown := dial(t, server)
	handshake(t, unknown)
	writeEnvelope(t, unknown, protocol.Envelope{
		Kind: protocol.KindSnapshot, Sequence: 1, Payload: json.RawMessage(`{}`),
	})
	if _, err := readEnvelopeWithin(unknown, 2*time.Second); err == nil {
		t.Fatal("an inbound state message was accepted")
	}
}

func TestServerResnapshotsOnReconnect(t *testing.T) {
	server, owner, _ := serve(t)
	key := itemKey(1)
	owner.Add(key)
	owner.Update(key, sample(key, "Chat"))

	first := dial(t, server)
	before := handshake(t, first)
	_ = first.Close()

	owner.Update(key, sample(key, "Chat (2)"))

	second := dial(t, server)
	after := handshake(t, second)
	if after.Sequence <= before.Sequence {
		t.Fatalf("reconnect sequence %d does not follow %d", after.Sequence, before.Sequence)
	}
	if len(after.Items) != 1 || after.Items[0].Title != "Chat (2)" {
		t.Fatalf("reconnect snapshot = %+v", after.Items)
	}
}

func serve(t *testing.T) (*Server, *state.Owner, *recordingCommands) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	owner := state.Start()
	commands := &recordingCommands{}
	server := NewAt(dir)
	if err := server.Serve(owner, commands); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = server.Close()
		_ = owner.Close()
	})
	return server, owner, commands
}

func dial(t *testing.T, server *Server) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("unix", server.SocketPath(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func handshake(t *testing.T, conn net.Conn) protocol.Snapshot {
	t.Helper()
	writeEnvelope(t, conn, helloEnvelope(protocol.ProtocolMajor, RequiredCapability))
	if envelope := readEnvelope(t, conn); envelope.Kind != protocol.KindHello {
		t.Fatalf("first message = %q, want hello", envelope.Kind)
	}
	envelope := readEnvelope(t, conn)
	if envelope.Kind != protocol.KindSnapshot {
		t.Fatalf("second message = %q, want snapshot", envelope.Kind)
	}
	var snapshot protocol.Snapshot
	if err := protocol.DecodeStrict(envelope.Payload, &snapshot); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func helloEnvelope(major uint16, capability string) protocol.Envelope {
	payload, _ := json.Marshal(protocol.Hello{
		Major: major, Minor: protocol.ProtocolMinor,
		Role: protocol.RolePresenter, Capabilities: []string{capability},
	})
	return protocol.Envelope{Kind: protocol.KindHello, Payload: payload}
}

func commandEnvelope(t *testing.T, requestID uint64, command protocol.Command) protocol.Envelope {
	t.Helper()
	payload, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	return protocol.Envelope{Kind: protocol.KindCommand, RequestID: requestID, Payload: payload}
}

func writeEnvelope(t *testing.T, conn net.Conn, envelope protocol.Envelope) {
	t.Helper()
	frame, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteFrame(conn, frame); err != nil {
		t.Fatal(err)
	}
}

func readEnvelope(t *testing.T, conn net.Conn) protocol.Envelope {
	t.Helper()
	envelope, err := readEnvelopeWithin(conn, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

func readEnvelopeWithin(conn net.Conn, timeout time.Duration) (protocol.Envelope, error) {
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return protocol.Envelope{}, err
	}
	frame, err := protocol.ReadFrame(conn)
	if err != nil {
		return protocol.Envelope{}, err
	}
	var envelope protocol.Envelope
	if err := protocol.DecodeStrict(frame, &envelope); err != nil {
		return protocol.Envelope{}, err
	}
	return envelope, envelope.Validate()
}

func readReply(t *testing.T, conn net.Conn) (protocol.Reply, protocol.Envelope) {
	t.Helper()
	envelope := readEnvelope(t, conn)
	if envelope.Kind != protocol.KindReply {
		t.Fatalf("message kind = %q, want reply", envelope.Kind)
	}
	var reply protocol.Reply
	if err := protocol.DecodeStrict(envelope.Payload, &reply); err != nil {
		t.Fatal(err)
	}
	if err := reply.Validate(); err != nil {
		t.Fatal(err)
	}
	return reply, envelope
}

type recordingCommands struct {
	mu       sync.Mutex
	recorded []string
	fail     error
}

func (c *recordingCommands) record(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recorded = append(c.recorded, name)
	return c.fail
}

func (c *recordingCommands) calls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.recorded...)
}

func (c *recordingCommands) Activate(protocol.ItemKey, int32, int32) error {
	return c.record("activate")
}

func (c *recordingCommands) SecondaryActivate(protocol.ItemKey, int32, int32) error {
	return c.record("secondary")
}

func (c *recordingCommands) Scroll(protocol.ItemKey, int32, protocol.ScrollOrientation) error {
	return c.record("scroll")
}

func (c *recordingCommands) MenuOpen(protocol.ItemKey, uint32, int32) error {
	return c.record("menu-open")
}

func (c *recordingCommands) MenuSelect(protocol.ItemKey, uint32, int32, uint32) error {
	return c.record("menu-select")
}

func (c *recordingCommands) AboutToShow(protocol.ItemKey, uint32, int32) (bool, error) {
	return false, c.record("about-to-show")
}

func (c *recordingCommands) MenuClose(protocol.ItemKey, uint32, int32) error {
	return c.record("menu-close")
}

func (c *recordingCommands) Terminate(protocol.ItemKey) error {
	return c.record("terminate")
}

func itemKey(generation uint64) protocol.ItemKey {
	return protocol.ItemKey{Owner: ":1.7", ObjectPath: "/StatusNotifierItem", Generation: generation}
}

func sample(key protocol.ItemKey, title string) protocol.Item {
	return protocol.Item{
		Key: key, ID: "sample", Title: title,
		Category: protocol.CategoryApplicationStatus, Status: protocol.StatusActive,
		Icon: protocol.Icon{Name: "sample"},
	}
}
