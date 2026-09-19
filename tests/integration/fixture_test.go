// Package integration exercises the tray service across real processes on a
// private session bus: real applications register, the service reads them over
// D-Bus, and a presenter consumes the socket exactly as the shell does.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/Nomadcxx/sysc-tray/internal/app"
	"github.com/Nomadcxx/sysc-tray/internal/dbustest"
	"github.com/Nomadcxx/sysc-tray/protocol"
)

const (
	watcherName   = "org.kde.StatusNotifierWatcher"
	watcherPath   = dbus.ObjectPath("/StatusNotifierWatcher")
	itemInterface = "org.kde.StatusNotifierItem"
	itemPath      = dbus.ObjectPath("/StatusNotifierItem")
	menuInterface = "com.canonical.dbusmenu"
	menuPath      = dbus.ObjectPath("/MenuBar")
)

// service starts the tray daemon and returns a connected presenter.
func service(t *testing.T) *client {
	t.Helper()
	requireSessionBus(t)
	// A Unix socket address is capped near 108 bytes, so the runtime directory
	// stays short rather than embedding the test name the way t.TempDir does.
	dir, err := os.MkdirTemp("", "syt")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	daemon := app.New(dir)
	if err := daemon.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = daemon.Close() })
	return connect(t, daemon.SocketPath())
}

type client struct {
	conn      net.Conn
	snapshot  protocol.Snapshot
	requestID uint64
	// pending holds deltas a previous helper set aside because they did not
	// match what it was waiting for; a removal can race the reply that
	// triggered it, so helpers must never discard cross-kind envelopes.
	pending []protocol.Envelope
}

func connect(t *testing.T, path string) *client {
	t.Helper()
	conn, err := net.DialTimeout("unix", path, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	c := &client{conn: conn}

	hello, err := json.Marshal(protocol.Hello{
		Major: protocol.ProtocolMajor, Minor: protocol.ProtocolMinor,
		Role: protocol.RolePresenter, Capabilities: []string{protocol.CapabilityTray},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.write(t, protocol.Envelope{Kind: protocol.KindHello, Payload: hello})
	if envelope := c.read(t); envelope.Kind != protocol.KindHello {
		t.Fatalf("first message = %q, want hello", envelope.Kind)
	}
	envelope := c.read(t)
	if envelope.Kind != protocol.KindSnapshot {
		t.Fatalf("second message = %q, want snapshot", envelope.Kind)
	}
	if err := protocol.DecodeStrict(envelope.Payload, &c.snapshot); err != nil {
		t.Fatal(err)
	}
	return c
}

func (c *client) write(t *testing.T, envelope protocol.Envelope) {
	t.Helper()
	frame, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteFrame(c.conn, frame); err != nil {
		t.Fatal(err)
	}
}

func (c *client) read(t *testing.T) protocol.Envelope {
	t.Helper()
	if err := c.conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	frame, err := protocol.ReadFrame(c.conn)
	if err != nil {
		t.Fatal(err)
	}
	var envelope protocol.Envelope
	if err := protocol.DecodeStrict(frame, &envelope); err != nil {
		t.Fatal(err)
	}
	if err := envelope.Validate(); err != nil {
		t.Fatal(err)
	}
	return envelope
}

// hold sets an envelope aside for a later helper. A helper must scan pending
// once per call and never re-hold what it popped: an envelope that is popped,
// rejected, and re-appended would spin between pending and the same helper
// forever, starving the wire.
func (c *client) hold(envelope protocol.Envelope) {
	c.pending = append(c.pending, envelope)
}

// fromPending returns the first set-aside envelope that satisfies match,
// removing it from pending. It reports whether one was found.
func (c *client) fromPending(match func(protocol.Envelope) bool) (protocol.Envelope, bool) {
	for i, envelope := range c.pending {
		if match(envelope) {
			c.pending = append(c.pending[:i], c.pending[i+1:]...)
			return envelope, true
		}
	}
	return protocol.Envelope{}, false
}

func (c *client) itemFrom(t *testing.T, envelope protocol.Envelope, match func(protocol.Item) bool) *protocol.Item {
	t.Helper()
	var item protocol.Item
	if err := protocol.DecodeStrict(envelope.Payload, &item); err != nil {
		t.Fatal(err)
	}
	if err := item.Validate(); err != nil {
		t.Fatalf("service published an invalid item: %v", err)
	}
	if match(item) {
		return &item
	}
	return nil
}

func (c *client) menuFrom(t *testing.T, envelope protocol.Envelope, match func(protocol.MenuUpdate) bool) *protocol.MenuUpdate {
	t.Helper()
	var update protocol.MenuUpdate
	if err := protocol.DecodeStrict(envelope.Payload, &update); err != nil {
		t.Fatal(err)
	}
	if err := update.Menu.Validate(); err != nil {
		t.Fatalf("service published an invalid menu: %v", err)
	}
	if match(update) {
		return &update
	}
	return nil
}

func (c *client) removalFrom(t *testing.T, envelope protocol.Envelope, key protocol.ItemKey) bool {
	t.Helper()
	var removed protocol.ItemRemoved
	if err := protocol.DecodeStrict(envelope.Payload, &removed); err != nil {
		t.Fatal(err)
	}
	return removed.Key == key
}

// awaitItem reads deltas until a published item satisfies match.
func (c *client) awaitItem(t *testing.T, match func(protocol.Item) bool) protocol.Item {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	itemEnvelope := func(envelope protocol.Envelope) bool {
		return envelope.Kind == protocol.KindItemAdded || envelope.Kind == protocol.KindItemChanged
	}
	if envelope, ok := c.fromPending(itemEnvelope); ok {
		return *c.itemFrom(t, envelope, match)
	}
	for time.Now().Before(deadline) {
		envelope := c.read(t)
		if !itemEnvelope(envelope) {
			c.hold(envelope)
			continue
		}
		if item := c.itemFrom(t, envelope, match); item != nil {
			return *item
		}
	}
	t.Fatal("no matching item was published")
	return protocol.Item{}
}

// awaitMenu reads deltas until a menu update satisfies match.
func (c *client) awaitMenu(t *testing.T, match func(protocol.MenuUpdate) bool) protocol.MenuUpdate {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	if envelope, ok := c.fromPending(func(e protocol.Envelope) bool { return e.Kind == protocol.KindMenuUpdated }); ok {
		return *c.menuFrom(t, envelope, match)
	}
	for time.Now().Before(deadline) {
		envelope := c.read(t)
		if envelope.Kind != protocol.KindMenuUpdated {
			c.hold(envelope)
			continue
		}
		if update := c.menuFrom(t, envelope, match); update != nil {
			return *update
		}
	}
	t.Fatal("no matching menu update was published")
	return protocol.MenuUpdate{}
}

func (c *client) awaitRemoval(t *testing.T, key protocol.ItemKey) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	if envelope, ok := c.fromPending(func(e protocol.Envelope) bool { return e.Kind == protocol.KindItemRemoved }); ok {
		c.removalFrom(t, envelope, key)
		return
	}
	for time.Now().Before(deadline) {
		envelope := c.read(t)
		if envelope.Kind != protocol.KindItemRemoved {
			c.hold(envelope)
			continue
		}
		if c.removalFrom(t, envelope, key) {
			return
		}
	}
	t.Fatal("the item was never retired")
}

func (c *client) send(t *testing.T, command protocol.Command) protocol.Reply {
	t.Helper()
	c.requestID++
	payload, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	c.write(t, protocol.Envelope{
		Kind: protocol.KindCommand, RequestID: c.requestID, Payload: payload,
	})
	// Replies are never set aside, so only the wire can carry this one.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		envelope := c.read(t)
		if envelope.Kind != protocol.KindReply {
			c.hold(envelope)
			continue
		}
		if envelope.RequestID != c.requestID {
			t.Fatalf("reply request ID = %d, want %d", envelope.RequestID, c.requestID)
		}
		var reply protocol.Reply
		if err := protocol.DecodeStrict(envelope.Payload, &reply); err != nil {
			t.Fatal(err)
		}
		return reply
	}
	t.Fatal("no reply arrived")
	return protocol.Reply{}
}

// trayApp is a fake tray application: it exports StatusNotifierItem, optionally
// a DBusMenu, and registers itself with whatever watcher owns the name.
type trayApp struct {
	conn *dbus.Conn

	mu       sync.Mutex
	props    map[string]dbus.Variant
	calls    []string
	menu     layoutNode
	revision uint32
}

func newTrayApp(t *testing.T, props map[string]dbus.Variant) *trayApp {
	t.Helper()
	conn := privateConn(t)
	fake := &trayApp{conn: conn, props: props}
	if err := conn.Export(itemProperties{app: fake}, itemPath, "org.freedesktop.DBus.Properties"); err != nil {
		t.Fatal(err)
	}
	if err := conn.Export(itemMethods{app: fake}, itemPath, itemInterface); err != nil {
		t.Fatal(err)
	}
	return fake
}

// register announces the item the way real applications do. The argument form
// is either the object path or the application's own bus name.
func (a *trayApp) register(t *testing.T, argument string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		call := a.conn.Object(watcherName, watcherPath).Call(
			watcherName+".RegisterStatusNotifierItem", 0, argument)
		if call.Err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("registration never succeeded: %v", call.Err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (a *trayApp) owner() string { return a.conn.Names()[0] }

func (a *trayApp) set(name string, value dbus.Variant) {
	a.mu.Lock()
	a.props[name] = value
	a.mu.Unlock()
}

func (a *trayApp) emit(t *testing.T, member string) {
	t.Helper()
	if err := a.conn.Emit(itemPath, itemInterface+"."+member); err != nil {
		t.Fatal(err)
	}
}

func (a *trayApp) invoked() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.calls...)
}

func (a *trayApp) record(call string) {
	a.mu.Lock()
	a.calls = append(a.calls, call)
	a.mu.Unlock()
}

type itemProperties struct{ app *trayApp }

func (p itemProperties) Get(iface, name string) (dbus.Variant, *dbus.Error) {
	p.app.mu.Lock()
	defer p.app.mu.Unlock()
	value, ok := p.app.props[name]
	if !ok {
		return dbus.Variant{}, dbus.NewError("org.freedesktop.DBus.Error.InvalidArgs", []any{name})
	}
	return value, nil
}

func (p itemProperties) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	p.app.mu.Lock()
	defer p.app.mu.Unlock()
	copied := make(map[string]dbus.Variant, len(p.app.props))
	for name, value := range p.app.props {
		copied[name] = value
	}
	return copied, nil
}

type itemMethods struct{ app *trayApp }

func (m itemMethods) Activate(x, y int32) *dbus.Error {
	m.app.record(fmt.Sprintf("Activate(%d,%d)", x, y))
	return nil
}

func (m itemMethods) SecondaryActivate(x, y int32) *dbus.Error {
	m.app.record(fmt.Sprintf("SecondaryActivate(%d,%d)", x, y))
	return nil
}

func (m itemMethods) Scroll(delta int32, orientation string) *dbus.Error {
	m.app.record(fmt.Sprintf("Scroll(%d,%s)", delta, orientation))
	return nil
}

type layoutNode struct {
	ID       int32
	Props    map[string]dbus.Variant
	Children []dbus.Variant
}

func menuNode(id int32, properties map[string]dbus.Variant, children ...layoutNode) layoutNode {
	if properties == nil {
		properties = map[string]dbus.Variant{}
	}
	wrapped := make([]dbus.Variant, len(children))
	for i, child := range children {
		wrapped[i] = dbus.MakeVariant(child)
	}
	return layoutNode{ID: id, Props: properties, Children: wrapped}
}

func variants(pairs ...any) map[string]dbus.Variant {
	result := make(map[string]dbus.Variant, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		result[pairs[i].(string)] = dbus.MakeVariant(pairs[i+1])
	}
	return result
}

// withMenu exports a DBusMenu and points the item at it.
func (a *trayApp) withMenu(t *testing.T, revision uint32, root layoutNode) {
	t.Helper()
	a.mu.Lock()
	a.revision, a.menu = revision, root
	a.props["Menu"] = dbus.MakeVariant(menuPath)
	a.mu.Unlock()
	if err := a.conn.Export(menuMethods{app: a}, menuPath, menuInterface); err != nil {
		t.Fatal(err)
	}
}

func (a *trayApp) setMenu(t *testing.T, revision uint32, root layoutNode) {
	t.Helper()
	a.mu.Lock()
	a.revision, a.menu = revision, root
	a.mu.Unlock()
	if err := a.conn.Emit(menuPath, menuInterface+".LayoutUpdated", revision, int32(0)); err != nil {
		t.Fatal(err)
	}
}

type menuMethods struct{ app *trayApp }

func (m menuMethods) GetLayout(parent, depth int32, names []string) (uint32, layoutNode, *dbus.Error) {
	m.app.mu.Lock()
	defer m.app.mu.Unlock()
	return m.app.revision, m.app.menu, nil
}

func (m menuMethods) Event(id int32, event string, data dbus.Variant, timestamp uint32) *dbus.Error {
	m.app.record(fmt.Sprintf("Event(%d,%s)", id, event))
	return nil
}

func (m menuMethods) AboutToShow(id int32) (bool, *dbus.Error) {
	m.app.record(fmt.Sprintf("AboutToShow(%d)", id))
	return false, nil
}

// menuPathVariant points an item at a menu object it never exports.
func menuPathVariant() dbus.Variant { return dbus.MakeVariant(menuPath) }

func baseProps(id, title string) map[string]dbus.Variant {
	return map[string]dbus.Variant{
		"Id":       dbus.MakeVariant(id),
		"Title":    dbus.MakeVariant(title),
		"Category": dbus.MakeVariant("ApplicationStatus"),
		"Status":   dbus.MakeVariant("Active"),
		"IconName": dbus.MakeVariant(id),
	}
}

func privateConn(t *testing.T) *dbus.Conn {
	t.Helper()
	return dbustest.Connect(t)
}

func requireSessionBus(t *testing.T) {
	t.Helper()
	dbustest.Session(t)
}
