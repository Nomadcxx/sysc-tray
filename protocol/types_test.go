package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSnapshotRoundTripAndValidate(t *testing.T) {
	want := Snapshot{Sequence: 4, Items: []Item{{
		Key: ItemKey{Owner: ":1.42", ObjectPath: "/StatusNotifierItem", Generation: 3},
		ID:  "chat", Title: "Chat", Category: CategoryCommunications, Status: StatusNeedsAttention,
		Icon:          Icon{Name: "chat", Pixmaps: []Pixmap{{Width: 2, Height: 1, ARGB: make([]byte, 8)}}},
		AttentionIcon: Icon{Name: "chat-attention"}, OverlayIcon: Icon{Name: "chat-overlay"},
		Tooltip:  Tooltip{Title: "Chat", Description: "Two unread messages"},
		MenuPath: "/Menu", ItemIsMenu: true,
	}}}
	if err := want.Validate(); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got Snapshot
	if err := DecodeStrict(data, &got); err != nil {
		t.Fatal(err)
	}
	if err := got.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestProtocolIdentityAndEnvelopeValidation(t *testing.T) {
	validKey := ItemKey{Owner: ":1.2", ObjectPath: "/Item", Generation: 1}
	if err := validKey.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, key := range []ItemKey{
		{Owner: "org.example.Item", ObjectPath: "/Item", Generation: 1},
		{Owner: ":1.2", ObjectPath: "Item", Generation: 1},
		{Owner: ":1.2", ObjectPath: "/Item"},
	} {
		if err := key.Validate(); err == nil {
			t.Fatalf("Validate() accepted %#v", key)
		}
	}
	for _, envelope := range []Envelope{
		{Kind: KindHello, Payload: json.RawMessage(`{}`)},
		{Kind: KindSnapshot, Sequence: 1, Payload: json.RawMessage(`{}`)},
		{Kind: KindItemAdded, Sequence: 2, Payload: json.RawMessage(`{}`)},
		{Kind: KindCommand, RequestID: 1, Payload: json.RawMessage(`{}`)},
		{Kind: KindReply, RequestID: 1, Payload: json.RawMessage(`{}`)},
	} {
		if err := envelope.Validate(); err != nil {
			t.Fatalf("%q: %v", envelope.Kind, err)
		}
	}
	if err := (Envelope{Kind: "future", Payload: json.RawMessage(`{}`)}).Validate(); err == nil {
		t.Fatal("unknown message kind accepted")
	}
	if err := ValidateNextSequence(9, 10); err != nil {
		t.Fatal(err)
	}
	if err := ValidateNextSequence(9, 11); err == nil {
		t.Fatal("sequence gap accepted")
	}
}

func TestItemAndPixmapBounds(t *testing.T) {
	valid := Item{
		Key: ItemKey{Owner: ":1.2", ObjectPath: "/Item", Generation: 1},
		ID:  "item", Title: "Item", Category: CategoryApplicationStatus, Status: StatusActive,
		Icon: Icon{Pixmaps: []Pixmap{{Width: 1, Height: 1, ARGB: make([]byte, 4)}}},
	}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Item){
		"status":       func(i *Item) { i.Status = Status("future") },
		"category":     func(i *Item) { i.Category = Category("future") },
		"text":         func(i *Item) { i.Title = strings.Repeat("x", MaxTextBytes+1) },
		"tooltip":      func(i *Item) { i.Tooltip.Description = strings.Repeat("x", MaxTooltipBytes+1) },
		"pixmap width": func(i *Item) { i.Icon.Pixmaps[0].Width = MaxPixmapDimension + 1 },
		"pixmap bytes": func(i *Item) { i.Icon.Pixmaps[0].ARGB = make([]byte, MaxPixmapBytesPerItem+1) },
		"bad pixels":   func(i *Item) { i.Icon.Pixmaps[0].Width = ^uint32(0) },
	} {
		t.Run(name, func(t *testing.T) {
			got := valid
			got.Icon.Pixmaps = append([]Pixmap(nil), valid.Icon.Pixmaps...)
			mutate(&got)
			if err := got.Validate(); err == nil {
				t.Fatal("Validate() accepted invalid item")
			}
		})
	}
}

func TestMenuValidationIsBoundedAndRevisionSafe(t *testing.T) {
	valid := Menu{Revision: 7, Root: MenuNode{ID: 0, Visible: true, Children: []MenuNode{{
		ID: 1, Label: "Open", Enabled: true, Visible: true,
	}, {ID: 2, Separator: true, Visible: true}}}}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	duplicate := valid
	duplicate.Root.Children = append(append([]MenuNode(nil), valid.Root.Children...), MenuNode{ID: 1, Label: "Again"})
	if err := duplicate.Validate(); err == nil {
		t.Fatal("duplicate menu ID accepted")
	}
	deep := Menu{Root: MenuNode{ID: 0}}
	node := &deep.Root
	for depth := 0; depth <= MaxMenuDepth; depth++ {
		node.Children = []MenuNode{{ID: int32(depth + 1)}}
		node = &node.Children[0]
	}
	if err := deep.Validate(); err == nil {
		t.Fatal("deep menu accepted")
	}
	wide := Menu{Root: MenuNode{ID: 0, Children: make([]MenuNode, MaxMenuNodes)}}
	for i := range wide.Root.Children {
		wide.Root.Children[i].ID = int32(i + 1)
	}
	if err := wide.Validate(); err == nil {
		t.Fatal("oversized menu accepted")
	}
	icons := Menu{Root: MenuNode{ID: 0, IconData: make([]byte, MaxMenuIconBytes+1)}}
	if err := icons.Validate(); err == nil {
		t.Fatal("oversized menu icon data accepted")
	}
}

func TestCommandsAndRepliesValidateTypedCorrelation(t *testing.T) {
	key := ItemKey{Owner: ":1.2", ObjectPath: "/Item", Generation: 1}
	for _, command := range []Command{
		{Kind: CommandActivate, Item: key, Output: 1, Serial: 2, X: 10, Y: 20},
		{Kind: CommandSecondaryActivate, Item: key, Output: 1, Serial: 2},
		{Kind: CommandScroll, Item: key, Delta: -120, Orientation: ScrollVertical},
		{Kind: CommandMenuOpen, Item: key, MenuRevision: 4, Output: 1, Serial: 2, X: 10, Y: 20},
		{Kind: CommandMenuSelect, Item: key, MenuRevision: 4, MenuID: 7},
		{Kind: CommandAboutToShow, Item: key, MenuRevision: 4, MenuID: 0},
		{Kind: CommandMenuClose, Item: key, MenuRevision: 4},
		{Kind: CommandTerminate, Item: key},
	} {
		if err := command.Validate(); err != nil {
			t.Fatalf("%q: %v", command.Kind, err)
		}
	}
	if err := (Command{Kind: CommandActivate, Item: key, Output: 1, Serial: 2, X: MaxCoordinate + 1}).Validate(); err == nil {
		t.Fatal("out-of-range coordinate accepted")
	}
	if err := (Command{Kind: CommandScroll, Item: key, Delta: 1, Orientation: "diagonal"}).Validate(); err == nil {
		t.Fatal("invalid scroll orientation accepted")
	}
	if err := (Command{Kind: CommandTerminate, Item: key, X: 1}).Validate(); err == nil {
		t.Fatal("terminate with a coordinate accepted")
	}
	if err := (Command{Kind: CommandTerminate, Item: key, MenuID: 3}).Validate(); err == nil {
		t.Fatal("terminate with a menu field accepted")
	}
	if err := (Command{Kind: CommandTerminate}).Validate(); err == nil {
		t.Fatal("terminate without an item accepted")
	}
	if err := (Reply{OK: true, Item: key, Output: 1}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Reply{Item: key, Error: &ProtocolError{Code: ErrorStaleRevision, Message: "changed"}}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Reply{OK: true, Item: key, Error: &ProtocolError{Code: ErrorBusy}}).Validate(); err == nil {
		t.Fatal("reply with two outcomes accepted")
	}
}
