package integration

import (
	"testing"

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

func TestReregisteringReplacesTheOldGeneration(t *testing.T) {
	presenter := service(t)
	application := newTrayApp(t, baseProps("chat", "Chat"))
	application.register(t, string(itemPath))
	first := presenter.awaitItem(t, func(i protocol.Item) bool { return i.ID == "chat" })

	application.set("Title", dbus.MakeVariant("Chat (2)"))
	application.register(t, string(itemPath))

	presenter.awaitRemoval(t, first.Key)
	second := presenter.awaitItem(t, func(i protocol.Item) bool {
		return i.ID == "chat" && i.Key.Generation > first.Key.Generation
	})
	if second.Key.Owner != first.Key.Owner {
		t.Fatalf("replacement owner = %q, want %q", second.Key.Owner, first.Key.Owner)
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
