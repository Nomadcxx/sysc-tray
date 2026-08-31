package item

import (
	"testing"

	"github.com/godbus/dbus/v5"

	"github.com/Nomadcxx/sysc-tray/protocol"
)

func TestPixmapsDecodeBoundedCandidates(t *testing.T) {
	value := []struct {
		Width  int32
		Height int32
		Data   []byte
	}{
		{Width: 2, Height: 1, Data: make([]byte, 8)},
		{Width: 1, Height: 1, Data: make([]byte, 4)},
	}
	got := decodePixmaps(dbus.MakeVariant(value), protocol.MaxPixmapBytesPerItem)
	if len(got) != 2 {
		t.Fatalf("decodePixmaps() returned %d pixmaps, want 2", len(got))
	}
	for i := range got {
		if err := got[i].Validate(); err != nil {
			t.Fatalf("pixmap[%d]: %v", i, err)
		}
	}
}

func TestPixmapsDropOnlyMalformedCandidates(t *testing.T) {
	value := []struct {
		Width  int32
		Height int32
		Data   []byte
	}{
		{Width: 0, Height: 4, Data: make([]byte, 0)},
		{Width: 2, Height: 2, Data: make([]byte, 4)},
		{Width: protocol.MaxPixmapDimension + 1, Height: 1, Data: make([]byte, 4)},
		{Width: -1, Height: -1, Data: make([]byte, 4)},
		{Width: 1, Height: 1, Data: make([]byte, 4)},
	}
	got := decodePixmaps(dbus.MakeVariant(value), protocol.MaxPixmapBytesPerItem)
	if len(got) != 1 {
		t.Fatalf("decodePixmaps() returned %d pixmaps, want only the healthy one", len(got))
	}
	if got[0].Width != 1 || got[0].Height != 1 {
		t.Fatalf("surviving pixmap = %dx%d, want 1x1", got[0].Width, got[0].Height)
	}
}

func TestPixmapsStopAtTheItemBudget(t *testing.T) {
	const side = protocol.MaxPixmapDimension
	const bytesEach = side * side * 4
	value := make([]struct {
		Width  int32
		Height int32
		Data   []byte
	}, 5)
	for i := range value {
		value[i].Width, value[i].Height, value[i].Data = side, side, make([]byte, bytesEach)
	}
	got := decodePixmaps(dbus.MakeVariant(value), protocol.MaxPixmapBytesPerItem)
	var total int
	for _, pixmap := range got {
		total += len(pixmap.ARGB)
	}
	if total > protocol.MaxPixmapBytesPerItem {
		t.Fatalf("decoded %d bytes, want at most %d", total, protocol.MaxPixmapBytesPerItem)
	}
	if len(got) != 2 {
		t.Fatalf("budget admitted %d pixmaps, want the 2 that fit", len(got))
	}
}

func TestPixmapsIgnoreForeignPayloads(t *testing.T) {
	for _, variant := range []dbus.Variant{
		dbus.MakeVariant("not a pixmap array"),
		dbus.MakeVariant(int32(7)),
		dbus.MakeVariant([]string{"a"}),
	} {
		if got := decodePixmaps(variant, protocol.MaxPixmapBytesPerItem); len(got) != 0 {
			t.Fatalf("decodePixmaps(%v) = %v, want none", variant, got)
		}
	}
}
