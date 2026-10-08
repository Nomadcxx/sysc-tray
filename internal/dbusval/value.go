// Package dbusval reads values that arrived over D-Bus. The bus decoder gives
// a struct either as a Go struct or as a []interface{}, depending on how the
// caller typed it, and every field is attacker-controlled. These helpers accept
// both shapes and never panic on an unexpected one.
package dbusval

import (
	"reflect"

	"github.com/godbus/dbus/v5"
)

// Indirect unwraps interfaces, pointers, and variants. The bus nests a struct
// inside a variant whenever the containing array is typed `av`, so unwrapping
// has to see through variants for a nested value to be readable at all.
func Indirect(value reflect.Value) reflect.Value {
	for value.IsValid() {
		switch value.Kind() {
		case reflect.Interface, reflect.Pointer:
			if value.IsNil() {
				return reflect.Value{}
			}
			value = value.Elem()
			continue
		}
		if !value.CanInterface() {
			return value
		}
		variant, ok := value.Interface().(dbus.Variant)
		if !ok {
			return value
		}
		value = reflect.ValueOf(variant.Value())
	}
	return value
}

// Tuple reads the first want fields of a D-Bus struct.
func Tuple(value any, want int) ([]reflect.Value, bool) {
	root := Indirect(reflect.ValueOf(value))
	if !root.IsValid() {
		return nil, false
	}
	var length int
	var read func(int) reflect.Value
	switch root.Kind() {
	case reflect.Struct:
		length, read = root.NumField(), root.Field
	case reflect.Slice, reflect.Array:
		length, read = root.Len(), root.Index
	default:
		return nil, false
	}
	if length < want {
		return nil, false
	}
	fields := make([]reflect.Value, want)
	for i := range fields {
		fields[i] = Indirect(read(i))
	}
	return fields, true
}

// Elements reads a D-Bus array as a slice of unwrapped values.
func Elements(value any) ([]reflect.Value, bool) {
	root := Indirect(reflect.ValueOf(value))
	if !root.IsValid() {
		return nil, false
	}
	if kind := root.Kind(); kind != reflect.Slice && kind != reflect.Array {
		return nil, false
	}
	elements := make([]reflect.Value, root.Len())
	for i := range elements {
		elements[i] = Indirect(root.Index(i))
	}
	return elements, true
}

// String reads a string field, reporting "" for any other shape.
func String(value reflect.Value) string {
	if !value.IsValid() || value.Kind() != reflect.String {
		return ""
	}
	return value.String()
}

// Bool reads a boolean field, reporting fallback for any other shape.
func Bool(value reflect.Value, fallback bool) bool {
	if !value.IsValid() || value.Kind() != reflect.Bool {
		return fallback
	}
	return value.Bool()
}

// Int reads any integer field, refusing values outside the int64 range.
func Int(value reflect.Value) (int64, bool) {
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

// Bytes reads a byte array field.
func Bytes(value reflect.Value) ([]byte, bool) {
	if !value.IsValid() || !value.CanInterface() {
		return nil, false
	}
	data, ok := value.Interface().([]byte)
	return data, ok
}

// Any unwraps a decoded field for further decoding, reporting nil when the
// value cannot be read.
func Any(value reflect.Value) any {
	if !value.IsValid() || !value.CanInterface() {
		return nil
	}
	return value.Interface()
}

// StringOf, BoolOf, IntOf, and BytesOf read a loose value of unknown shape.
func StringOf(value any) string { return String(Indirect(reflect.ValueOf(value))) }

func BoolOf(value any, fallback bool) bool { return Bool(Indirect(reflect.ValueOf(value)), fallback) }

func IntOf(value any) (int64, bool) { return Int(Indirect(reflect.ValueOf(value))) }

func BytesOf(value any) ([]byte, bool) { return Bytes(Indirect(reflect.ValueOf(value))) }
