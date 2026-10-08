package integration

import (
	"testing"

	"github.com/godbus/dbus/v5"

	"github.com/Nomadcxx/sysc-tray/protocol"
)

type wirePixmap struct {
	Width  int32
	Height int32
	Data   []byte
}

func pixmaps(entries ...wirePixmap) dbus.Variant { return dbus.MakeVariant(entries) }

func TestIconsPixmapsAndTooltipSurviveIndependently(t *testing.T) {
	presenter := service(t)
	props := baseProps("chat", "Chat")
	props["Category"] = dbus.MakeVariant("Communications")
	props["Status"] = dbus.MakeVariant("NeedsAttention")
	props["IconThemePath"] = dbus.MakeVariant("/usr/share/icons/hicolor")
	props["IconPixmap"] = pixmaps(wirePixmap{Width: 2, Height: 2, Data: make([]byte, 16)})
	props["AttentionIconName"] = dbus.MakeVariant("chat-attention")
	props["AttentionIconPixmap"] = pixmaps(wirePixmap{Width: 1, Height: 1, Data: []byte{1, 2, 3, 4}})
	props["OverlayIconName"] = dbus.MakeVariant("chat-overlay")
	props["ToolTip"] = dbus.MakeVariant(struct {
		IconName    string
		Pixmaps     []wirePixmap
		Title       string
		Description string
	}{IconName: "chat", Title: "Chat", Description: "Two unread messages"})

	application := newTrayApp(t, props)
	application.register(t, string(itemPath))
	item := presenter.awaitItem(t, func(i protocol.Item) bool { return i.ID == "chat" })

	if item.Category != protocol.CategoryCommunications || item.Status != protocol.StatusNeedsAttention {
		t.Fatalf("category/status = %q/%q", item.Category, item.Status)
	}
	if item.Icon.Name != "chat" || item.Icon.ThemePath != "/usr/share/icons/hicolor" {
		t.Fatalf("icon = %+v", item.Icon)
	}
	if len(item.Icon.Pixmaps) != 1 || len(item.Icon.Pixmaps[0].ARGB) != 16 {
		t.Fatalf("icon pixmaps = %+v", item.Icon.Pixmaps)
	}
	if item.AttentionIcon.Name != "chat-attention" || len(item.AttentionIcon.Pixmaps) != 1 {
		t.Fatalf("attention icon = %+v", item.AttentionIcon)
	}
	if item.OverlayIcon.Name != "chat-overlay" {
		t.Fatalf("overlay icon = %+v", item.OverlayIcon)
	}
	if item.Tooltip.Title != "Chat" || item.Tooltip.Description != "Two unread messages" {
		t.Fatalf("tooltip = %+v", item.Tooltip)
	}
	// The service stays renderer-neutral: it carries bytes, never a raster.
	if item.Icon.Pixmaps[0].Width != 2 || item.Icon.Pixmaps[0].Height != 2 {
		t.Fatalf("pixmap geometry = %dx%d", item.Icon.Pixmaps[0].Width, item.Icon.Pixmaps[0].Height)
	}
}

func TestPropertySignalsRepublishTheItem(t *testing.T) {
	presenter := service(t)
	application := newTrayApp(t, baseProps("chat", "Chat"))
	application.register(t, string(itemPath))
	presenter.awaitItem(t, func(i protocol.Item) bool { return i.ID == "chat" })

	application.set("Status", dbus.MakeVariant("NeedsAttention"))
	application.emit(t, "NewStatus")
	item := presenter.awaitItem(t, func(i protocol.Item) bool {
		return i.ID == "chat" && i.Status == protocol.StatusNeedsAttention
	})
	if item.Status != protocol.StatusNeedsAttention {
		t.Fatalf("status after NewStatus = %q", item.Status)
	}

	application.set("Title", dbus.MakeVariant("Chat (3)"))
	application.emit(t, "NewTitle")
	presenter.awaitItem(t, func(i protocol.Item) bool { return i.Title == "Chat (3)" })
}

func TestPointerCommandsReachTheApplication(t *testing.T) {
	presenter := service(t)
	application := newTrayApp(t, baseProps("chat", "Chat"))
	application.register(t, string(itemPath))
	item := presenter.awaitItem(t, func(i protocol.Item) bool { return i.ID == "chat" })

	commands := []protocol.Command{
		{Kind: protocol.CommandActivate, Item: item.Key, Output: 1, Serial: 2, X: 100, Y: 20},
		{Kind: protocol.CommandSecondaryActivate, Item: item.Key, Output: 1, Serial: 3, X: 101, Y: 21},
		{Kind: protocol.CommandScroll, Item: item.Key, Delta: -120, Orientation: protocol.ScrollVertical},
		{Kind: protocol.CommandScroll, Item: item.Key, Delta: 15, Orientation: protocol.ScrollHorizontal},
	}
	for _, command := range commands {
		reply := presenter.send(t, command)
		if !reply.OK || reply.Error != nil {
			t.Fatalf("%s reply = %+v", command.Kind, reply)
		}
		if reply.Item != item.Key {
			t.Fatalf("%s reply named %+v", command.Kind, reply.Item)
		}
	}

	want := []string{
		"Activate(100,20)", "SecondaryActivate(101,21)",
		"Scroll(-120,vertical)", "Scroll(15,horizontal)",
	}
	got := application.invoked()
	if len(got) != len(want) {
		t.Fatalf("application saw %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestCommandsForARetiredGenerationAreRefused(t *testing.T) {
	presenter := service(t)
	application := newTrayApp(t, baseProps("chat", "Chat"))
	application.register(t, string(itemPath))
	item := presenter.awaitItem(t, func(i protocol.Item) bool { return i.ID == "chat" })

	stale := item.Key
	stale.Generation += 5
	reply := presenter.send(t, protocol.Command{
		Kind: protocol.CommandActivate, Item: stale, Output: 1, Serial: 1,
	})
	if reply.OK || reply.Error == nil || reply.Error.Code != protocol.ErrorStaleItem {
		t.Fatalf("stale command reply = %+v", reply)
	}
	if got := application.invoked(); len(got) != 0 {
		t.Fatalf("a stale command reached the application: %v", got)
	}
}
