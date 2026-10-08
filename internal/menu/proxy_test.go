package menu

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/Nomadcxx/sysc-tray/internal/dbustest"

	"github.com/Nomadcxx/sysc-tray/protocol"
)

func TestProxyPublishesLayoutAndRevision(t *testing.T) {
	conn := privateConn(t)
	fake := exportFakeMenu(t, conn, 4, node(0, nil,
		node(1, props("label", "Open", "enabled", true, "visible", true)),
		node(2, props("type", "separator", "visible", true)),
	))
	updates := make(chan Update, 8)
	proxy := start(t, conn, fake, updates)
	defer proxy.Close()

	tree := awaitUpdate(t, updates).Menu
	if tree.Revision != 4 {
		t.Fatalf("revision = %d, want 4", tree.Revision)
	}
	if err := Validate(tree); err != nil {
		t.Fatalf("published tree is invalid: %v", err)
	}
	if len(tree.Root.Children) != 2 {
		t.Fatalf("root has %d children, want 2", len(tree.Root.Children))
	}
	if tree.Root.Children[0].Label != "Open" || !tree.Root.Children[0].Enabled {
		t.Fatalf("first child = %+v", tree.Root.Children[0])
	}
	if !tree.Root.Children[1].Separator {
		t.Fatalf("second child = %+v, want a separator", tree.Root.Children[1])
	}
	if proxy.Revision() != 4 {
		t.Fatalf("Revision() = %d, want 4", proxy.Revision())
	}
}

func TestProxyFollowsNewerRevisionsAndIgnoresOlderOnes(t *testing.T) {
	conn := privateConn(t)
	fake := exportFakeMenu(t, conn, 2, node(0, nil, node(1, props("label", "Open"))))
	updates := make(chan Update, 16)
	proxy := start(t, conn, fake, updates)
	defer proxy.Close()
	awaitUpdate(t, updates)

	fake.setLayout(5, node(0, nil, node(1, props("label", "Close"))))
	fake.emitLayoutUpdated(t, 5)
	tree := awaitUpdate(t, updates).Menu
	if tree.Revision != 5 || tree.Root.Children[0].Label != "Close" {
		t.Fatalf("tree after update = revision %d, label %q", tree.Revision, tree.Root.Children[0].Label)
	}

	before := fake.layoutReads()
	fake.emitLayoutUpdated(t, 3)
	time.Sleep(200 * time.Millisecond)
	if fake.layoutReads() != before {
		t.Fatal("an older revision triggered a refetch")
	}
	if proxy.Revision() != 5 {
		t.Fatalf("Revision() = %d, want the newer 5", proxy.Revision())
	}
}

func TestProxyKeepsRevisionOnPropertyOnlyUpdates(t *testing.T) {
	conn := privateConn(t)
	fake := exportFakeMenu(t, conn, 7, node(0, nil, node(1, props("label", "Open"))))
	updates := make(chan Update, 16)
	proxy := start(t, conn, fake, updates)
	defer proxy.Close()
	awaitUpdate(t, updates)

	fake.setLayout(7, node(0, nil, node(1, props("label", "Opened"))))
	fake.emitPropertiesUpdated(t)
	tree := awaitUpdate(t, updates).Menu
	if tree.Revision != 7 {
		t.Fatalf("property-only update changed the revision to %d", tree.Revision)
	}
	if tree.Root.Children[0].Label != "Opened" {
		t.Fatalf("property-only update kept label %q", tree.Root.Children[0].Label)
	}
	if tree.Root.Children[0].ID != 1 {
		t.Fatalf("property-only update changed entry identity to %d", tree.Root.Children[0].ID)
	}
}

func TestProxyRefreshesWhenAboutToShowRequestsIt(t *testing.T) {
	conn := privateConn(t)
	fake := exportFakeMenu(t, conn, 1, node(0, nil, node(1, props("label", "Open"))))
	updates := make(chan Update, 16)
	proxy := start(t, conn, fake, updates)
	defer proxy.Close()
	awaitUpdate(t, updates)

	fake.setNeedUpdate(true)
	fake.setLayout(2, node(0, nil, node(1, props("label", "Fresh"))))
	needed, err := proxy.AboutToShow(fake.key, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !needed {
		t.Fatal("AboutToShow() reported no update was needed")
	}
	tree := awaitUpdate(t, updates).Menu
	if tree.Root.Children[0].Label != "Fresh" {
		t.Fatalf("AboutToShow did not refresh the tree: %+v", tree.Root.Children[0])
	}
}

func TestProxyRefusesStaleSelectionWithoutInvoking(t *testing.T) {
	conn := privateConn(t)
	fake := exportFakeMenu(t, conn, 9, node(0, nil, node(1, props("label", "Open"))))
	updates := make(chan Update, 8)
	proxy := start(t, conn, fake, updates)
	defer proxy.Close()
	awaitUpdate(t, updates)

	err := proxy.Select(fake.key, 8, 1, 100)
	var refusal *protocol.ProtocolError
	if !errors.As(err, &refusal) || refusal.Code != protocol.ErrorStaleRevision {
		t.Fatalf("stale selection returned %v, want a stale_revision error", err)
	}
	if got := fake.events(); len(got) != 0 {
		t.Fatalf("stale selection invoked %v", got)
	}
}

func TestProxyRefusesCommandsForAnotherGeneration(t *testing.T) {
	conn := privateConn(t)
	fake := exportFakeMenu(t, conn, 1, node(0, nil, node(1, props("label", "Open"))))
	updates := make(chan Update, 8)
	proxy := start(t, conn, fake, updates)
	defer proxy.Close()
	awaitUpdate(t, updates)

	other := fake.key
	other.Generation++
	err := proxy.Select(other, 1, 1, 100)
	var refusal *protocol.ProtocolError
	if !errors.As(err, &refusal) || refusal.Code != protocol.ErrorStaleItem {
		t.Fatalf("selection for another generation returned %v, want stale_item", err)
	}
	if got := fake.events(); len(got) != 0 {
		t.Fatalf("stale generation invoked %v", got)
	}
}

func TestProxyInvokesSelectionAtTheCurrentRevision(t *testing.T) {
	conn := privateConn(t)
	fake := exportFakeMenu(t, conn, 3, node(0, nil, node(1, props("label", "Open"))))
	updates := make(chan Update, 8)
	proxy := start(t, conn, fake, updates)
	defer proxy.Close()
	awaitUpdate(t, updates)

	if err := proxy.Select(fake.key, 3, 1, 4242); err != nil {
		t.Fatal(err)
	}
	got := fake.events()
	if len(got) != 1 || got[0].id != 1 || got[0].event != "clicked" || got[0].timestamp != 4242 {
		t.Fatalf("recorded events = %+v", got)
	}
	if err := proxy.Opened(fake.key, 3, 0); err != nil {
		t.Fatal(err)
	}
	if err := proxy.Closed(fake.key, 3, 0); err != nil {
		t.Fatal(err)
	}
	if got := fake.events(); len(got) != 3 || got[1].event != "opened" || got[2].event != "closed" {
		t.Fatalf("recorded events = %+v", got)
	}
}

func TestProxyReportsUnavailableMenusAndStopsAfterClose(t *testing.T) {
	conn := privateConn(t)
	fake := exportFakeMenu(t, conn, 1, node(0, nil, node(1, props("label", "Open"))))
	updates := make(chan Update, 16)
	proxy := start(t, conn, fake, updates)
	awaitUpdate(t, updates)

	fake.setFailing(true)
	fake.emitLayoutUpdated(t, 2)
	deadline := time.After(3 * time.Second)
	for {
		select {
		case update := <-updates:
			if update.Err != nil {
				goto closed
			}
		case <-deadline:
			t.Fatal("a failing menu never reported an error")
		}
	}
closed:
	proxy.Close()
	for len(updates) > 0 {
		<-updates
	}
	fake.setFailing(false)
	fake.emitLayoutUpdated(t, 3)
	select {
	case update := <-updates:
		t.Fatalf("closed proxy published %+v", update)
	case <-time.After(250 * time.Millisecond):
	}
}

func start(t *testing.T, conn *dbus.Conn, fake *fakeMenu, updates chan Update) *Proxy {
	t.Helper()
	proxy := NewProxy(conn, fake.key, string(fake.path), func(u Update) { updates <- u })
	if err := proxy.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return proxy
}

func awaitUpdate(t *testing.T, updates chan Update) Update {
	t.Helper()
	select {
	case update := <-updates:
		if update.Err != nil {
			t.Fatalf("menu update failed: %v", update.Err)
		}
		return update
	case <-time.After(3 * time.Second):
		t.Fatal("no menu update published")
		return Update{}
	}
}

type layoutNode struct {
	ID       int32
	Props    map[string]dbus.Variant
	Children []dbus.Variant
}

func node(id int32, properties map[string]dbus.Variant, children ...layoutNode) layoutNode {
	if properties == nil {
		properties = map[string]dbus.Variant{}
	}
	wrapped := make([]dbus.Variant, len(children))
	for i, child := range children {
		wrapped[i] = dbus.MakeVariant(child)
	}
	return layoutNode{ID: id, Props: properties, Children: wrapped}
}

func props(pairs ...any) map[string]dbus.Variant {
	result := make(map[string]dbus.Variant, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		result[pairs[i].(string)] = dbus.MakeVariant(pairs[i+1])
	}
	return result
}

type recordedEvent struct {
	id        int32
	event     string
	timestamp uint32
}

type fakeMenu struct {
	conn *dbus.Conn
	path dbus.ObjectPath
	key  protocol.ItemKey

	mu         sync.Mutex
	revision   uint32
	root       layoutNode
	recorded   []recordedEvent
	reads      int
	needUpdate bool
	failing    bool
}

func exportFakeMenu(t *testing.T, host *dbus.Conn, revision uint32, root layoutNode) *fakeMenu {
	t.Helper()
	conn := privateConn(t)
	fake := &fakeMenu{conn: conn, path: "/Menu", revision: revision, root: root}
	fake.key = protocol.ItemKey{Owner: conn.Names()[0], ObjectPath: "/StatusNotifierItem", Generation: 1}
	if err := conn.Export(fake, fake.path, Interface); err != nil {
		t.Fatal(err)
	}
	return fake
}

func (f *fakeMenu) GetLayout(parent int32, depth int32, names []string) (uint32, layoutNode, *dbus.Error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.failing {
		return 0, layoutNode{}, dbus.NewError("org.freedesktop.DBus.Error.Failed", []any{"menu unavailable"})
	}
	return f.revision, f.root, nil
}

func (f *fakeMenu) Event(id int32, event string, data dbus.Variant, timestamp uint32) *dbus.Error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recorded = append(f.recorded, recordedEvent{id: id, event: event, timestamp: timestamp})
	return nil
}

func (f *fakeMenu) AboutToShow(id int32) (bool, *dbus.Error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.needUpdate, nil
}

func (f *fakeMenu) setLayout(revision uint32, root layoutNode) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revision, f.root = revision, root
}

func (f *fakeMenu) setNeedUpdate(value bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.needUpdate = value
}

func (f *fakeMenu) setFailing(value bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failing = value
}

func (f *fakeMenu) layoutReads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

func (f *fakeMenu) events() []recordedEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedEvent(nil), f.recorded...)
}

func (f *fakeMenu) emitLayoutUpdated(t *testing.T, revision uint32) {
	t.Helper()
	if err := f.conn.Emit(f.path, Interface+".LayoutUpdated", revision, int32(0)); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeMenu) emitPropertiesUpdated(t *testing.T) {
	t.Helper()
	updated := []struct {
		ID    int32
		Props map[string]dbus.Variant
	}{{ID: 1, Props: props("label", "Opened")}}
	removed := []struct {
		ID    int32
		Names []string
	}{}
	if err := f.conn.Emit(f.path, Interface+".ItemsPropertiesUpdated", updated, removed); err != nil {
		t.Fatal(err)
	}
}

func privateConn(t *testing.T) *dbus.Conn {
	t.Helper()
	return dbustest.Connect(t)
}
