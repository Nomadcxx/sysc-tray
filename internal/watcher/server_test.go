package watcher

import (
	"os"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

func TestServerOwnsWatcherAndPublishesRegistrations(t *testing.T) {
	conn := privateConn(t)
	observer := newRecorder()
	server := NewServer(conn, observer)
	if err := server.Serve(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	if owner := nameOwner(t, conn, BusName); owner != conn.Names()[0] {
		t.Fatalf("watcher owner = %q, want %q", owner, conn.Names()[0])
	}

	item := privateConn(t)
	registerItem(t, item, string(DefaultItemPath))

	added := observer.awaitAdded(t)
	if added.Owner != item.Names()[0] || added.ObjectPath != DefaultItemPath {
		t.Fatalf("registered key = %+v, want owner %q path %q", added, item.Names()[0], DefaultItemPath)
	}
	if added.Generation == 0 {
		t.Fatal("registered key has generation zero")
	}
	if got := registeredItems(t, conn); len(got) != 1 {
		t.Fatalf("RegisteredStatusNotifierItems = %v, want one entry", got)
	}
}

func TestServerResolvesServiceNamesAndRejectsMalformedRegistrations(t *testing.T) {
	conn := privateConn(t)
	server := NewServer(conn, newRecorder())
	if err := server.Serve(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	item := privateConn(t)
	wellKnown := "org.example.TrayApp"
	reply, err := item.RequestName(wellKnown, dbus.NameFlagDoNotQueue)
	if err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("RequestName(%q) = %v, %v", wellKnown, reply, err)
	}
	registerItem(t, item, wellKnown)
	if got := registeredItems(t, conn); len(got) != 1 {
		t.Fatalf("RegisteredStatusNotifierItems = %v, want one entry", got)
	}

	for _, argument := range []string{"notapath", "org.example.Unowned", ""} {
		if err := callRegisterItem(item, argument); err == nil {
			t.Fatalf("RegisterStatusNotifierItem accepted %q", argument)
		}
	}
}

func TestServerReplacesOwnerGenerationBeforeAdding(t *testing.T) {
	conn := privateConn(t)
	observer := newRecorder()
	server := NewServer(conn, observer)
	if err := server.Serve(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	item := privateConn(t)
	registerItem(t, item, string(DefaultItemPath))
	first := observer.awaitAdded(t)
	registerItem(t, item, string(DefaultItemPath))

	removed := observer.awaitRemoved(t)
	if removed != first {
		t.Fatalf("removed key = %+v, want %+v", removed, first)
	}
	second := observer.awaitAdded(t)
	if second.Generation <= first.Generation {
		t.Fatalf("replacement generation %d does not follow %d", second.Generation, first.Generation)
	}
	if got := registeredItems(t, conn); len(got) != 1 {
		t.Fatalf("RegisteredStatusNotifierItems = %v, want one entry", got)
	}
}

func TestServerDropsItemsWhenTheirOwnerLeaves(t *testing.T) {
	conn := privateConn(t)
	observer := newRecorder()
	server := NewServer(conn, observer)
	if err := server.Serve(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	item, err := dbus.ConnectSessionBus()
	if err != nil {
		t.Fatal(err)
	}
	registerItem(t, item, string(DefaultItemPath))
	added := observer.awaitAdded(t)
	if err := item.Close(); err != nil {
		t.Fatal(err)
	}

	removed := observer.awaitRemoved(t)
	if removed != added {
		t.Fatalf("removed key = %+v, want %+v", removed, added)
	}
	if got := registeredItems(t, conn); len(got) != 0 {
		t.Fatalf("RegisteredStatusNotifierItems = %v, want none", got)
	}
}

func TestServerRegistersHostIdentityOnce(t *testing.T) {
	conn := privateConn(t)
	server := NewServer(conn, newRecorder())
	if err := server.Serve(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	if hostRegistered(t, conn) {
		t.Fatal("host reported as registered before any host attached")
	}
	host := privateConn(t)
	object := host.Object(BusName, ObjectPath)
	for range 2 {
		if call := object.Call(Interface+".RegisterStatusNotifierHost", 0, HostName(host)); call.Err != nil {
			t.Fatal(call.Err)
		}
	}
	if !hostRegistered(t, conn) {
		t.Fatal("host registration was not reported")
	}
}

func privateConn(t *testing.T) *dbus.Conn {
	t.Helper()
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		t.Skip("no private session bus; run under dbus-run-session")
	}
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func registerItem(t *testing.T, conn *dbus.Conn, argument string) {
	t.Helper()
	if err := callRegisterItem(conn, argument); err != nil {
		t.Fatal(err)
	}
}

func callRegisterItem(conn *dbus.Conn, argument string) error {
	return conn.Object(BusName, ObjectPath).Call(Interface+".RegisterStatusNotifierItem", 0, argument).Err
}

func nameOwner(t *testing.T, conn *dbus.Conn, name string) string {
	t.Helper()
	var owner string
	if err := conn.BusObject().Call("org.freedesktop.DBus.GetNameOwner", 0, name).Store(&owner); err != nil {
		t.Fatal(err)
	}
	return owner
}

func registeredItems(t *testing.T, conn *dbus.Conn) []string {
	t.Helper()
	variant, err := conn.Object(BusName, ObjectPath).GetProperty(Interface + ".RegisteredStatusNotifierItems")
	if err != nil {
		t.Fatal(err)
	}
	items, ok := variant.Value().([]string)
	if !ok {
		t.Fatalf("RegisteredStatusNotifierItems has type %T", variant.Value())
	}
	return items
}

func hostRegistered(t *testing.T, conn *dbus.Conn) bool {
	t.Helper()
	variant, err := conn.Object(BusName, ObjectPath).GetProperty(Interface + ".IsStatusNotifierHostRegistered")
	if err != nil {
		t.Fatal(err)
	}
	registered, ok := variant.Value().(bool)
	if !ok {
		t.Fatalf("IsStatusNotifierHostRegistered has type %T", variant.Value())
	}
	return registered
}

type recorder struct {
	added   chan Key
	removed chan Key
}

func newRecorder() *recorder {
	return &recorder{added: make(chan Key, 16), removed: make(chan Key, 16)}
}

func (r *recorder) ItemAdded(key Key)   { r.added <- key }
func (r *recorder) ItemRemoved(key Key) { r.removed <- key }

func (r *recorder) awaitAdded(t *testing.T) Key {
	t.Helper()
	select {
	case key := <-r.added:
		return key
	case <-time.After(3 * time.Second):
		t.Fatal("no item addition observed")
		return Key{}
	}
}

func (r *recorder) awaitRemoved(t *testing.T) Key {
	t.Helper()
	select {
	case key := <-r.removed:
		return key
	case <-time.After(3 * time.Second):
		t.Fatal("no item removal observed")
		return Key{}
	}
}
