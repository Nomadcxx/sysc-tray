package integration

import (
	"testing"

	"github.com/Nomadcxx/sysc-tray/protocol"
)

func TestMenuLayoutAndSubmenuReachThePresenter(t *testing.T) {
	presenter := service(t)
	application := newTrayApp(t, baseProps("chat", "Chat"))
	application.withMenu(t, 1, menuNode(0, nil,
		menuNode(1, variants("label", "Open", "enabled", true, "visible", true)),
		menuNode(2, variants("type", "separator", "visible", true)),
		menuNode(3, variants("label", "Status", "children-display", "submenu", "visible", true),
			menuNode(4, variants("label", "Away", "toggle-type", "radio", "toggle-state", int32(1))),
			menuNode(5, variants("label", "Busy", "toggle-type", "radio", "toggle-state", int32(0))),
		),
	))
	application.register(t, string(itemPath))

	update := presenter.awaitMenu(t, func(u protocol.MenuUpdate) bool { return u.Menu.Revision == 1 })
	root := update.Menu.Root
	if len(root.Children) != 3 {
		t.Fatalf("root has %d children", len(root.Children))
	}
	if root.Children[0].Label != "Open" || !root.Children[0].Enabled {
		t.Fatalf("first entry = %+v", root.Children[0])
	}
	if !root.Children[1].Separator {
		t.Fatalf("second entry = %+v, want a separator", root.Children[1])
	}
	submenu := root.Children[2]
	if submenu.ChildrenDisplay != "submenu" || len(submenu.Children) != 2 {
		t.Fatalf("submenu = %+v", submenu)
	}
	if submenu.Children[0].ToggleType != protocol.ToggleRadio || submenu.Children[0].ToggleState != 1 {
		t.Fatalf("submenu entry = %+v", submenu.Children[0])
	}
}

func TestNewerMenuRevisionsReplaceTheTree(t *testing.T) {
	presenter := service(t)
	application := newTrayApp(t, baseProps("chat", "Chat"))
	application.withMenu(t, 1, menuNode(0, nil, menuNode(1, variants("label", "Open"))))
	application.register(t, string(itemPath))
	presenter.awaitMenu(t, func(u protocol.MenuUpdate) bool { return u.Menu.Revision == 1 })

	application.setMenu(t, 2, menuNode(0, nil,
		menuNode(1, variants("label", "Close")),
		menuNode(2, variants("label", "Quit")),
	))
	update := presenter.awaitMenu(t, func(u protocol.MenuUpdate) bool { return u.Menu.Revision == 2 })
	if len(update.Menu.Root.Children) != 2 || update.Menu.Root.Children[0].Label != "Close" {
		t.Fatalf("updated tree = %+v", update.Menu.Root.Children)
	}
}

func TestMenuSelectionAtTheDrawnRevisionInvokesTheEntry(t *testing.T) {
	presenter := service(t)
	application := newTrayApp(t, baseProps("chat", "Chat"))
	application.withMenu(t, 4, menuNode(0, nil, menuNode(1, variants("label", "Open"))))
	application.register(t, string(itemPath))
	update := presenter.awaitMenu(t, func(u protocol.MenuUpdate) bool { return u.Menu.Revision == 4 })

	open := presenter.send(t, protocol.Command{
		Kind: protocol.CommandMenuOpen, Item: update.Key, MenuRevision: 4,
		Output: 1, Serial: 2, X: 10, Y: 30,
	})
	if !open.OK {
		t.Fatalf("menu open reply = %+v", open)
	}
	choose := presenter.send(t, protocol.Command{
		Kind: protocol.CommandMenuSelect, Item: update.Key, MenuRevision: 4, MenuID: 1, Serial: 77,
	})
	if !choose.OK {
		t.Fatalf("menu selection reply = %+v", choose)
	}
	closed := presenter.send(t, protocol.Command{
		Kind: protocol.CommandMenuClose, Item: update.Key, MenuRevision: 4,
	})
	if !closed.OK {
		t.Fatalf("menu close reply = %+v", closed)
	}

	want := []string{"Event(0,opened)", "Event(1,clicked)", "Event(0,closed)"}
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

func TestSelectionAgainstAStaleRevisionInvokesNothing(t *testing.T) {
	presenter := service(t)
	application := newTrayApp(t, baseProps("chat", "Chat"))
	application.withMenu(t, 1, menuNode(0, nil, menuNode(1, variants("label", "Open"))))
	application.register(t, string(itemPath))
	presenter.awaitMenu(t, func(u protocol.MenuUpdate) bool { return u.Menu.Revision == 1 })

	application.setMenu(t, 2, menuNode(0, nil, menuNode(1, variants("label", "Delete Everything"))))
	update := presenter.awaitMenu(t, func(u protocol.MenuUpdate) bool { return u.Menu.Revision == 2 })

	// The shell drew revision 1; the tree changed underneath it.
	reply := presenter.send(t, protocol.Command{
		Kind: protocol.CommandMenuSelect, Item: update.Key, MenuRevision: 1, MenuID: 1,
	})
	if reply.OK || reply.Error == nil || reply.Error.Code != protocol.ErrorStaleRevision {
		t.Fatalf("stale selection reply = %+v", reply)
	}
	if got := application.invoked(); len(got) != 0 {
		t.Fatalf("a stale selection invoked %v", got)
	}
}

func TestAboutToShowIsReportedToTheShell(t *testing.T) {
	presenter := service(t)
	application := newTrayApp(t, baseProps("chat", "Chat"))
	application.withMenu(t, 3, menuNode(0, nil, menuNode(1, variants("label", "Open"))))
	application.register(t, string(itemPath))
	update := presenter.awaitMenu(t, func(u protocol.MenuUpdate) bool { return u.Menu.Revision == 3 })

	reply := presenter.send(t, protocol.Command{
		Kind: protocol.CommandAboutToShow, Item: update.Key, MenuRevision: 3, MenuID: 0,
	})
	if !reply.OK || reply.Error != nil {
		t.Fatalf("AboutToShow reply = %+v", reply)
	}
	if got := application.invoked(); len(got) != 1 || got[0] != "AboutToShow(0)" {
		t.Fatalf("application saw %v", got)
	}
}

// An item whose menu is unusable still activates and scrolls.
func TestPointerCommandsSurviveAMissingMenu(t *testing.T) {
	presenter := service(t)
	application := newTrayApp(t, baseProps("chat", "Chat"))
	application.set("Menu", menuPathVariant())
	application.register(t, string(itemPath))
	item := presenter.awaitItem(t, func(i protocol.Item) bool { return i.ID == "chat" })

	reply := presenter.send(t, protocol.Command{
		Kind: protocol.CommandActivate, Item: item.Key, Output: 1, Serial: 1, X: 5, Y: 6,
	})
	if !reply.OK || reply.Error != nil {
		t.Fatalf("activate reply = %+v", reply)
	}
	if got := application.invoked(); len(got) != 1 || got[0] != "Activate(5,6)" {
		t.Fatalf("application saw %v", got)
	}
}
