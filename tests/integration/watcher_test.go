package integration

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/Nomadcxx/sysc-tray/protocol"
)

func TestItemRegisteredByObjectPathReachesThePresenter(t *testing.T) {
	presenter := service(t)
	application := newTrayApp(t, baseProps("chat", "Chat"))
	application.register(t, string(itemPath))

	item := presenter.awaitItem(t, func(i protocol.Item) bool { return i.ID == "chat" })
	if item.Key.Owner != application.owner() {
		t.Fatalf("item owner = %q, want %q", item.Key.Owner, application.owner())
	}
	if item.Key.ObjectPath != string(itemPath) {
		t.Fatalf("item path = %q", item.Key.ObjectPath)
	}
	if item.Key.Generation == 0 {
		t.Fatal("item reached the presenter without a generation")
	}
}

// Applications in the wild register by their own bus name as often as by path.
func TestItemRegisteredByBusNameResolvesToItsUniqueOwner(t *testing.T) {
	presenter := service(t)
	application := newTrayApp(t, baseProps("mail", "Mail"))
	wellKnown := "org.example.MailTray"
	reply, err := application.conn.RequestName(wellKnown, dbus.NameFlagDoNotQueue)
	if err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("RequestName(%q) = %v, %v", wellKnown, reply, err)
	}
	application.register(t, wellKnown)

	item := presenter.awaitItem(t, func(i protocol.Item) bool { return i.ID == "mail" })
	if item.Key.Owner != application.owner() {
		t.Fatalf("item owner = %q, want the unique name %q", item.Key.Owner, application.owner())
	}
}

func TestItemIsRetiredWhenItsApplicationLeavesTheBus(t *testing.T) {
	presenter := service(t)
	application := newTrayApp(t, baseProps("chat", "Chat"))
	application.register(t, string(itemPath))
	item := presenter.awaitItem(t, func(i protocol.Item) bool { return i.ID == "chat" })

	if err := application.conn.Close(); err != nil {
		t.Fatal(err)
	}
	presenter.awaitRemoval(t, item.Key)
}

func TestDuplicateRegistrationKeepsTheExistingGeneration(t *testing.T) {
	presenter := service(t)
	application := newTrayApp(t, baseProps("chat", "Chat"))
	application.register(t, string(itemPath))
	first := presenter.awaitItem(t, func(i protocol.Item) bool { return i.ID == "chat" })

	application.register(t, string(itemPath))
	if err := presenter.conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	_, err := protocol.ReadFrame(presenter.conn)
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("duplicate registration published a delta: %v", err)
	}

	application.set("Title", dbus.MakeVariant("Chat (2)"))
	application.emit(t, "NewTitle")
	updated := presenter.awaitItem(t, func(i protocol.Item) bool {
		return i.ID == "chat" && i.Title == "Chat (2)"
	})
	if updated.Key != first.Key {
		t.Fatalf("updated key = %+v, want original %+v", updated.Key, first.Key)
	}
}

func TestSiblingsSurviveOneMalformedApplication(t *testing.T) {
	presenter := service(t)
	healthy := newTrayApp(t, baseProps("healthy", "Healthy"))
	healthy.register(t, string(itemPath))
	presenter.awaitItem(t, func(i protocol.Item) bool { return i.ID == "healthy" })

	// This application answers with wrong types and an oversized pixmap.
	broken := newTrayApp(t, map[string]dbus.Variant{
		"Id":         dbus.MakeVariant("broken"),
		"Title":      dbus.MakeVariant(int32(7)),
		"Category":   dbus.MakeVariant("Nonsense"),
		"Status":     dbus.MakeVariant([]string{"Active"}),
		"IconPixmap": dbus.MakeVariant([]struct{ Width, Height int32 }{{Width: 9999, Height: 9999}}),
		"ToolTip":    dbus.MakeVariant(int64(3)),
	})
	broken.register(t, string(itemPath))

	healthy.set("Title", dbus.MakeVariant("Healthy (2)"))
	healthy.emit(t, "NewTitle")
	item := presenter.awaitItem(t, func(i protocol.Item) bool {
		return i.ID == "healthy" && i.Title == "Healthy (2)"
	})
	if item.Title != "Healthy (2)" {
		t.Fatalf("healthy sibling = %+v", item)
	}
}
