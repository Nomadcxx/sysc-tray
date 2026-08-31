package item

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/Nomadcxx/sysc-tray/protocol"
)

func TestProxyPublishesBoundedItemState(t *testing.T) {
	conn := privateConn(t)
	fake := exportFakeItem(t, conn, map[string]dbus.Variant{
		"Id":       dbus.MakeVariant("chat"),
		"Title":    dbus.MakeVariant("Chat"),
		"Category": dbus.MakeVariant("Communications"),
		"Status":   dbus.MakeVariant("NeedsAttention"),
		"IconName": dbus.MakeVariant("chat"),
		"IconPixmap": dbus.MakeVariant([]struct {
			Width  int32
			Height int32
			Data   []byte
		}{{Width: 1, Height: 1, Data: make([]byte, 4)}}),
		"AttentionIconName": dbus.MakeVariant("chat-attention"),
		"OverlayIconName":   dbus.MakeVariant("chat-overlay"),
		"IconThemePath":     dbus.MakeVariant("/usr/share/icons"),
		"ToolTip": dbus.MakeVariant(struct {
			IconName string
			Pixmaps  []struct {
				Width  int32
				Height int32
				Data   []byte
			}
			Title       string
			Description string
		}{IconName: "chat", Title: "Chat", Description: "Two unread"}),
		"Menu":       dbus.MakeVariant(dbus.ObjectPath("/Menu")),
		"ItemIsMenu": dbus.MakeVariant(true),
	})

	results := make(chan Result, 8)
	proxy := start(t, conn, fake.key, results)
	defer proxy.Close()

	item := awaitResult(t, results).Item
	if err := item.Validate(); err != nil {
		t.Fatalf("published item is invalid: %v", err)
	}
	if item.ID != "chat" || item.Title != "Chat" {
		t.Fatalf("item identity = %q/%q", item.ID, item.Title)
	}
	if item.Category != protocol.CategoryCommunications || item.Status != protocol.StatusNeedsAttention {
		t.Fatalf("item category/status = %q/%q", item.Category, item.Status)
	}
	if item.Icon.Name != "chat" || len(item.Icon.Pixmaps) != 1 {
		t.Fatalf("icon = %+v", item.Icon)
	}
	if item.AttentionIcon.Name != "chat-attention" || item.OverlayIcon.Name != "chat-overlay" {
		t.Fatalf("attention/overlay icons = %+v / %+v", item.AttentionIcon, item.OverlayIcon)
	}
	if item.Tooltip.Title != "Chat" || item.Tooltip.Description != "Two unread" {
		t.Fatalf("tooltip = %+v", item.Tooltip)
	}
	if item.MenuPath != "/Menu" || !item.ItemIsMenu {
		t.Fatalf("menu = %q, itemIsMenu = %v", item.MenuPath, item.ItemIsMenu)
	}
}

func TestProxyKeepsHealthyStateWhenFieldsAreMalformed(t *testing.T) {
	conn := privateConn(t)
	fake := exportFakeItem(t, conn, map[string]dbus.Variant{
		"Id":       dbus.MakeVariant("app"),
		"Title":    dbus.MakeVariant(strings.Repeat("x", protocol.MaxTextBytes*2)),
		"Category": dbus.MakeVariant("Nonsense"),
		"Status":   dbus.MakeVariant(int32(3)),
		"IconName": dbus.MakeVariant("app"),
		"IconPixmap": dbus.MakeVariant([]struct {
			Width  int32
			Height int32
			Data   []byte
		}{
			{Width: 4, Height: 4, Data: make([]byte, 3)},
			{Width: 1, Height: 1, Data: make([]byte, 4)},
		}),
		"ToolTip": dbus.MakeVariant("not a tooltip"),
		"Menu":    dbus.MakeVariant("/not/an/object/path/type"),
	})

	results := make(chan Result, 8)
	proxy := start(t, conn, fake.key, results)
	defer proxy.Close()

	item := awaitResult(t, results).Item
	if err := item.Validate(); err != nil {
		t.Fatalf("published item is invalid: %v", err)
	}
	if item.Category != protocol.CategoryApplicationStatus {
		t.Fatalf("invalid category became %q", item.Category)
	}
	if item.Status != protocol.StatusActive {
		t.Fatalf("invalid status became %q", item.Status)
	}
	if len(item.Title) > protocol.MaxTextBytes {
		t.Fatalf("title kept %d bytes", len(item.Title))
	}
	if len(item.Icon.Pixmaps) != 1 || item.Icon.Pixmaps[0].Width != 1 {
		t.Fatalf("healthy pixmap did not survive: %+v", item.Icon.Pixmaps)
	}
	if item.Tooltip.Title != "" || item.Tooltip.Description != "" {
		t.Fatalf("malformed tooltip produced %+v", item.Tooltip)
	}
}

func TestProxyCoalescesSignalBursts(t *testing.T) {
	conn := privateConn(t)
	fake := exportFakeItem(t, conn, minimalProps())
	results := make(chan Result, 256)
	proxy := start(t, conn, fake.key, results)
	defer proxy.Close()
	awaitResult(t, results)

	before := fake.reads()
	for range 50 {
		fake.emit(t, "NewIcon")
		fake.emit(t, "NewTitle")
	}
	time.Sleep(300 * time.Millisecond)
	reads := fake.reads() - before
	if reads == 0 {
		t.Fatal("burst produced no refresh")
	}
	if reads > 12 {
		t.Fatalf("burst produced %d refreshes, want coalescing to a bounded few", reads)
	}
}

func TestProxyRefreshesOnLegacyAndPropertySignals(t *testing.T) {
	conn := privateConn(t)
	fake := exportFakeItem(t, conn, minimalProps())
	results := make(chan Result, 32)
	proxy := start(t, conn, fake.key, results)
	defer proxy.Close()
	awaitResult(t, results)

	for _, signal := range []string{"NewTitle", "NewStatus", "NewToolTip", "NewAttentionIcon", "NewOverlayIcon"} {
		before := fake.reads()
		fake.emit(t, signal)
		awaitResult(t, results)
		if fake.reads() <= before {
			t.Fatalf("%s did not trigger a property read", signal)
		}
		time.Sleep(40 * time.Millisecond)
	}
}

func TestProxyStopsPublishingAfterClose(t *testing.T) {
	conn := privateConn(t)
	fake := exportFakeItem(t, conn, minimalProps())
	results := make(chan Result, 32)
	proxy := start(t, conn, fake.key, results)
	awaitResult(t, results)
	proxy.Close()

	drain(results)
	for range 5 {
		fake.emit(t, "NewIcon")
	}
	select {
	case result := <-results:
		t.Fatalf("closed proxy published %+v", result)
	case <-time.After(250 * time.Millisecond):
	}
}

func TestProxyReportsOwnerLoss(t *testing.T) {
	host := privateConn(t)
	ownerConn, err := dbus.ConnectSessionBus()
	if err != nil {
		t.Fatal(err)
	}
	fake := exportFakeItemOn(t, ownerConn, minimalProps())
	results := make(chan Result, 8)
	proxy := start(t, host, fake.key, results)
	defer proxy.Close()
	awaitResult(t, results)

	if err := ownerConn.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case result := <-results:
			if result.Err != nil {
				return
			}
		case <-deadline:
			t.Fatal("owner loss was not reported")
		}
	}
}

func start(t *testing.T, conn *dbus.Conn, key protocol.ItemKey, results chan Result) *Proxy {
	t.Helper()
	proxy := NewProxy(conn, key, NewLimiter(time.Now), func(r Result) { results <- r })
	if err := proxy.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return proxy
}

func awaitResult(t *testing.T, results chan Result) Result {
	t.Helper()
	select {
	case result := <-results:
		if result.Err != nil {
			t.Fatalf("refresh failed: %v", result.Err)
		}
		return result
	case <-time.After(3 * time.Second):
		t.Fatal("no refresh published")
		return Result{}
	}
}

func drain(results chan Result) {
	for {
		select {
		case <-results:
		default:
			return
		}
	}
}

func minimalProps() map[string]dbus.Variant {
	return map[string]dbus.Variant{
		"Id":       dbus.MakeVariant("app"),
		"Title":    dbus.MakeVariant("App"),
		"Category": dbus.MakeVariant("ApplicationStatus"),
		"Status":   dbus.MakeVariant("Active"),
		"IconName": dbus.MakeVariant("app"),
	}
}

type fakeItem struct {
	conn  *dbus.Conn
	key   protocol.ItemKey
	count atomic.Int64

	mu       sync.Mutex
	props    map[string]dbus.Variant
	recorded []string
}

func exportFakeItem(t *testing.T, host *dbus.Conn, props map[string]dbus.Variant) *fakeItem {
	t.Helper()
	conn := privateConn(t)
	return exportFakeItemOn(t, conn, props)
}

func exportFakeItemOn(t *testing.T, conn *dbus.Conn, props map[string]dbus.Variant) *fakeItem {
	t.Helper()
	fake := &fakeItem{conn: conn, props: props}
	fake.key = protocol.ItemKey{
		Owner: conn.Names()[0], ObjectPath: "/StatusNotifierItem", Generation: 1,
	}
	if err := conn.Export(fake, dbus.ObjectPath(fake.key.ObjectPath), "org.freedesktop.DBus.Properties"); err != nil {
		t.Fatal(err)
	}
	return fake
}

func (f *fakeItem) reads() int64 { return f.count.Load() }

func (f *fakeItem) emit(t *testing.T, member string) {
	t.Helper()
	if err := f.conn.Emit(dbus.ObjectPath(f.key.ObjectPath), ItemInterface+"."+member); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeItem) Get(iface, name string) (dbus.Variant, *dbus.Error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	value, ok := f.props[name]
	if !ok {
		return dbus.Variant{}, dbus.NewError("org.freedesktop.DBus.Error.InvalidArgs", []any{name})
	}
	return value, nil
}

func (f *fakeItem) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	f.count.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	copied := make(map[string]dbus.Variant, len(f.props))
	for name, value := range f.props {
		copied[name] = value
	}
	return copied, nil
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

func TestLimiterBoundsPerItemAndGlobalRates(t *testing.T) {
	base := time.Unix(0, 0)
	clock := base
	limiter := NewLimiter(func() time.Time { return clock })
	first := protocol.ItemKey{Owner: ":1.1", ObjectPath: "/Item", Generation: 1}
	second := protocol.ItemKey{Owner: ":1.2", ObjectPath: "/Item", Generation: 1}

	if wait := limiter.Reserve(first); wait != 0 {
		t.Fatalf("first reservation waited %v", wait)
	}
	if wait := limiter.Reserve(first); wait != PerItemInterval {
		t.Fatalf("immediate repeat waited %v, want %v", wait, PerItemInterval)
	}
	if wait := limiter.Reserve(second); wait != GlobalInterval {
		t.Fatalf("sibling waited %v, want the global interval %v", wait, GlobalInterval)
	}

	clock = base.Add(GlobalInterval)
	if wait := limiter.Reserve(second); wait != 0 {
		t.Fatalf("sibling waited %v after the global interval", wait)
	}
	if wait := limiter.Reserve(first); wait == 0 {
		t.Fatal("item exceeded its own rate before its interval elapsed")
	}

	clock = base.Add(PerItemInterval)
	if wait := limiter.Reserve(first); wait != 0 {
		t.Fatalf("item waited %v after its interval elapsed", wait)
	}

	limiter.Forget(first)
	clock = base.Add(PerItemInterval + GlobalInterval)
	if wait := limiter.Reserve(first); wait != 0 {
		t.Fatalf("forgotten item waited %v", wait)
	}
}

func TestProxyInvokesItemCommandsAndRefusesStaleGenerations(t *testing.T) {
	conn := privateConn(t)
	fake := exportFakeItem(t, conn, minimalProps())
	fake.exportMethods(t)
	results := make(chan Result, 8)
	proxy := start(t, conn, fake.key, results)
	defer proxy.Close()
	awaitResult(t, results)

	if err := proxy.Activate(fake.key, 10, 20); err != nil {
		t.Fatal(err)
	}
	if err := proxy.SecondaryActivate(fake.key, 11, 21); err != nil {
		t.Fatal(err)
	}
	if err := proxy.Scroll(fake.key, -120, protocol.ScrollVertical); err != nil {
		t.Fatal(err)
	}
	got := fake.calls()
	if len(got) != 3 || got[0] != "Activate(10,20)" || got[1] != "SecondaryActivate(11,21)" {
		t.Fatalf("recorded calls = %v", got)
	}
	if got[2] != "Scroll(-120,vertical)" {
		t.Fatalf("scroll recorded as %q", got[2])
	}

	if err := proxy.Scroll(fake.key, 1, protocol.ScrollOrientation("diagonal")); err == nil {
		t.Fatal("an unknown scroll orientation was invoked")
	}

	other := fake.key
	other.Generation++
	for name, err := range map[string]error{
		"activate":  proxy.Activate(other, 1, 1),
		"secondary": proxy.SecondaryActivate(other, 1, 1),
		"scroll":    proxy.Scroll(other, 1, protocol.ScrollVertical),
	} {
		var refusal *protocol.ProtocolError
		if !errors.As(err, &refusal) || refusal.Code != protocol.ErrorStaleItem {
			t.Fatalf("%s for another generation returned %v, want stale_item", name, err)
		}
	}
	if len(fake.calls()) != 3 {
		t.Fatalf("a stale command reached the item: %v", fake.calls())
	}
}

func (f *fakeItem) exportMethods(t *testing.T) {
	t.Helper()
	if err := f.conn.Export(methods{fake: f}, dbus.ObjectPath(f.key.ObjectPath), ItemInterface); err != nil {
		t.Fatal(err)
	}
}

type methods struct {
	fake *fakeItem
}

func (m methods) Activate(x, y int32) *dbus.Error {
	m.fake.record(fmt.Sprintf("Activate(%d,%d)", x, y))
	return nil
}

func (m methods) SecondaryActivate(x, y int32) *dbus.Error {
	m.fake.record(fmt.Sprintf("SecondaryActivate(%d,%d)", x, y))
	return nil
}

func (m methods) Scroll(delta int32, orientation string) *dbus.Error {
	m.fake.record(fmt.Sprintf("Scroll(%d,%s)", delta, orientation))
	return nil
}

func (f *fakeItem) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recorded = append(f.recorded, call)
}

func (f *fakeItem) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.recorded...)
}
