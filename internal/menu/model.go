// Package menu reads revisioned DBusMenu layouts and invokes menu commands.
// It keeps properties renderer-neutral: the shell owns surfaces, fonts, icon
// lookup, and placement.
package menu

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/godbus/dbus/v5"

	"github.com/Nomadcxx/sysc-tray/internal/dbusval"
	"github.com/Nomadcxx/sysc-tray/protocol"
)

const Interface = "com.canonical.dbusmenu"

// convertLayout turns one DBusMenu layout tuple into a bounded tree. It walks
// with an explicit worklist and enforces the node and depth caps while walking,
// so a hostile layout is refused before the whole tree is materialized.
func convertLayout(revision uint32, layout any) (protocol.Menu, error) {
	root, err := convertNode(layout)
	if err != nil {
		return protocol.Menu{}, err
	}
	tree := protocol.Menu{Revision: revision, Root: root}

	type frame struct {
		source any
		target *protocol.MenuNode
		depth  int
	}
	sources, ok := dbusval.Tuple(layout, 3)
	if !ok {
		return protocol.Menu{}, errors.New("menu: layout is not a DBusMenu node")
	}
	children, _ := dbusval.Elements(dbusval.Any(sources[2]))
	stack := make([]frame, 0, len(children))
	for i := len(children) - 1; i >= 0; i-- {
		stack = append(stack, frame{source: dbusval.Any(children[i]), target: &tree.Root, depth: 1})
	}

	nodes := 1
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if current.depth > protocol.MaxMenuDepth {
			return protocol.Menu{}, fmt.Errorf("menu: depth %d exceeds %d", current.depth, protocol.MaxMenuDepth)
		}
		nodes++
		if nodes > protocol.MaxMenuNodes {
			return protocol.Menu{}, fmt.Errorf("menu: node count exceeds %d", protocol.MaxMenuNodes)
		}
		node, err := convertNode(current.source)
		if err != nil {
			return protocol.Menu{}, err
		}
		current.target.Children = append(current.target.Children, node)
		added := &current.target.Children[len(current.target.Children)-1]

		fields, ok := dbusval.Tuple(current.source, 3)
		if !ok {
			continue
		}
		grandchildren, _ := dbusval.Elements(dbusval.Any(fields[2]))
		for i := len(grandchildren) - 1; i >= 0; i-- {
			stack = append(stack, frame{source: dbusval.Any(grandchildren[i]), target: added, depth: current.depth + 1})
		}
	}
	return tree, nil
}

// convertNode reads one node's identity and properties, applying the DBusMenu
// defaults for anything absent or malformed.
func convertNode(source any) (protocol.MenuNode, error) {
	fields, ok := dbusval.Tuple(source, 3)
	if !ok {
		return protocol.MenuNode{}, errors.New("menu: node is not a DBusMenu node")
	}
	id, ok := dbusval.Int(fields[0])
	if !ok {
		return protocol.MenuNode{}, errors.New("menu: node identity is not an integer")
	}
	if id < 0 || id > int64(^uint32(0)>>1) {
		return protocol.MenuNode{}, fmt.Errorf("menu: node identity %d is out of range", id)
	}
	node := protocol.MenuNode{ID: int32(id), Enabled: true, Visible: true}
	properties := propertyMap(dbusval.Any(fields[1]))
	if value, ok := properties["label"]; ok {
		node.Label = clamp(stringValue(value), protocol.MaxTextBytes)
	}
	if value, ok := properties["enabled"]; ok {
		node.Enabled = boolValue(value, true)
	}
	if value, ok := properties["visible"]; ok {
		node.Visible = boolValue(value, true)
	}
	if value, ok := properties["type"]; ok {
		node.Separator = stringValue(value) == "separator"
	}
	if value, ok := properties["icon-name"]; ok {
		node.IconName = clamp(stringValue(value), protocol.MaxTextBytes)
	}
	if value, ok := properties["icon-data"]; ok {
		if data, ok := dbusval.BytesOf(value.Value()); ok && len(data) <= protocol.MaxMenuIconBytes {
			node.IconData = append([]byte(nil), data...)
		}
	}
	if value, ok := properties["toggle-type"]; ok {
		switch candidate := protocol.ToggleType(stringValue(value)); candidate {
		case protocol.ToggleCheckmark, protocol.ToggleRadio:
			node.ToggleType = candidate
		}
	}
	if value, ok := properties["toggle-state"]; ok {
		if state, ok := dbusval.IntOf(value.Value()); ok {
			node.ToggleState = int32(state)
		}
	}
	if value, ok := properties["children-display"]; ok {
		if stringValue(value) == "submenu" {
			node.ChildrenDisplay = "submenu"
		}
	}
	return node, nil
}

func propertyMap(value any) map[string]dbus.Variant {
	switch typed := value.(type) {
	case map[string]dbus.Variant:
		return typed
	case map[string]any:
		converted := make(map[string]dbus.Variant, len(typed))
		for name, item := range typed {
			converted[name] = dbus.MakeVariant(item)
		}
		return converted
	default:
		return nil
	}
}

func stringValue(value dbus.Variant) string { return dbusval.StringOf(value.Value()) }

func boolValue(value dbus.Variant, fallback bool) bool {
	return dbusval.BoolOf(value.Value(), fallback)
}

// clamp keeps text valid UTF-8 and within its bound on a rune boundary.
func clamp(value string, limit int) string {
	if !utf8.ValidString(value) {
		value = strings.ToValidUTF8(value, "")
	}
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}
