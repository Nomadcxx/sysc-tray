package item

import (
	"reflect"

	"github.com/godbus/dbus/v5"

	"github.com/Nomadcxx/sysc-tray/protocol"
)

// decodePixmaps converts an SNI `a(iiay)` pixmap array into bounded wire
// pixmaps. A malformed candidate is dropped and its healthy siblings survive,
// so one bad pixmap cannot remove an item. Pixel arithmetic is done in uint64
// so a hostile width and height cannot overflow into a short allocation.
func decodePixmaps(variant dbus.Variant, budget int) []protocol.Pixmap {
	value := reflect.ValueOf(variant.Value())
	if !value.IsValid() {
		return nil
	}
	if kind := value.Kind(); kind != reflect.Slice && kind != reflect.Array {
		return nil
	}
	var pixmaps []protocol.Pixmap
	var total int
	for i := range value.Len() {
		width, height, data, ok := pixmapFields(value.Index(i))
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

// pixmapFields reads one candidate, accepting both the struct form a typed
// caller produces and the []interface{} form the bus decoder produces.
func pixmapFields(value reflect.Value) (int64, int64, []byte, bool) {
	value = indirect(value)
	if !value.IsValid() {
		return 0, 0, nil, false
	}
	var fields [3]reflect.Value
	switch value.Kind() {
	case reflect.Struct:
		if value.NumField() < 3 {
			return 0, 0, nil, false
		}
		for i := range fields {
			fields[i] = value.Field(i)
		}
	case reflect.Slice, reflect.Array:
		if value.Len() < 3 {
			return 0, 0, nil, false
		}
		for i := range fields {
			fields[i] = value.Index(i)
		}
	default:
		return 0, 0, nil, false
	}
	width, ok := integer(fields[0])
	if !ok {
		return 0, 0, nil, false
	}
	height, ok := integer(fields[1])
	if !ok {
		return 0, 0, nil, false
	}
	data, ok := indirect(fields[2]).Interface().([]byte)
	if !ok {
		return 0, 0, nil, false
	}
	return width, height, data, true
}

func integer(value reflect.Value) (int64, bool) {
	value = indirect(value)
	if !value.IsValid() {
		return 0, false
	}
	switch value.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return value.Int(), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		converted := value.Uint()
		if converted > uint64(^uint32(0)) {
			return 0, false
		}
		return int64(converted), true
	default:
		return 0, false
	}
}

func indirect(value reflect.Value) reflect.Value {
	for value.IsValid() && (value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer) {
		if value.IsNil() {
			return reflect.Value{}
		}
		value = value.Elem()
	}
	return value
}
