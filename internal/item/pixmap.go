package item

import (
	"reflect"

	"github.com/godbus/dbus/v5"

	"github.com/Nomadcxx/sysc-tray/internal/dbusval"
	"github.com/Nomadcxx/sysc-tray/protocol"
)

// decodePixmaps converts an SNI `a(iiay)` pixmap array into bounded wire
// pixmaps. A malformed candidate is dropped and its healthy siblings survive,
// so one bad pixmap cannot remove an item. Pixel arithmetic is done in uint64
// so a hostile width and height cannot overflow into a short allocation.
func decodePixmaps(variant dbus.Variant, budget int) []protocol.Pixmap {
	elements, ok := dbusval.Elements(variant.Value())
	if !ok {
		return nil
	}
	var pixmaps []protocol.Pixmap
	var total int
	for i := range elements {
		width, height, data, ok := pixmapFields(elements[i])
		if !ok {
			continue
		}
		if width <= 0 || height <= 0 {
			continue
		}
		if width > protocol.MaxPixmapDimension || height > protocol.MaxPixmapDimension {
			continue
		}
		if uint64(width)*uint64(height)*4 != uint64(len(data)) {
			continue
		}
		if total+len(data) > budget {
			continue
		}
		total += len(data)
		pixmaps = append(pixmaps, protocol.Pixmap{
			Width: uint32(width), Height: uint32(height), ARGB: append([]byte(nil), data...),
		})
	}
	return pixmaps
}

// pixmapFields reads one candidate in either shape the bus decoder produces.
func pixmapFields(value reflect.Value) (int64, int64, []byte, bool) {
	if !value.IsValid() || !value.CanInterface() {
		return 0, 0, nil, false
	}
	fields, ok := dbusval.Tuple(value.Interface(), 3)
	if !ok {
		return 0, 0, nil, false
	}
	width, ok := dbusval.Int(fields[0])
	if !ok {
		return 0, 0, nil, false
	}
	height, ok := dbusval.Int(fields[1])
	if !ok {
		return 0, 0, nil, false
	}
	data, ok := dbusval.Bytes(fields[2])
	if !ok {
		return 0, 0, nil, false
	}
	return width, height, data, true
}
