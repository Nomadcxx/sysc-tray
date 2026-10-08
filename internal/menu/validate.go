package menu

import (
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/Nomadcxx/sysc-tray/protocol"
)

// Validate walks a tree with an explicit stack rather than recursion, so a
// hostile layout is bounded by the node and depth caps instead of by the Go
// stack. Every bound is checked while walking, before the tree is published.
func Validate(tree protocol.Menu) error {
	if tree.Root.ID != 0 {
		return fmt.Errorf("menu: root identity is %d, want 0", tree.Root.ID)
	}
	type frame struct {
		node  *protocol.MenuNode
		depth int
	}
	seen := make(map[int32]struct{}, 16)
	stack := []frame{{node: &tree.Root}}
	var nodes int
	var icons int

	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		node := current.node

		if current.depth > protocol.MaxMenuDepth {
			return fmt.Errorf("menu: depth %d exceeds %d", current.depth, protocol.MaxMenuDepth)
		}
		nodes++
		if nodes > protocol.MaxMenuNodes {
			return fmt.Errorf("menu: node count exceeds %d", protocol.MaxMenuNodes)
		}
		if node.ID < 0 {
			return fmt.Errorf("menu: negative identity %d", node.ID)
		}
		if _, exists := seen[node.ID]; exists {
			return fmt.Errorf("menu: repeated identity %d", node.ID)
		}
		seen[node.ID] = struct{}{}

		if err := validateText("label", node.Label); err != nil {
			return err
		}
		if err := validateText("icon name", node.IconName); err != nil {
			return err
		}
		switch node.ToggleType {
		case protocol.ToggleNone, protocol.ToggleCheckmark, protocol.ToggleRadio:
		default:
			return fmt.Errorf("menu: unknown toggle type %q", node.ToggleType)
		}
		switch node.ChildrenDisplay {
		case "", "submenu":
		default:
			return fmt.Errorf("menu: unknown children display %q", node.ChildrenDisplay)
		}
		icons += len(node.IconData)
		if icons > protocol.MaxMenuIconBytes {
			return errors.New("menu: icon data exceeds its aggregate cap")
		}

		for i := range node.Children {
			stack = append(stack, frame{node: &node.Children[i], depth: current.depth + 1})
		}
	}
	return nil
}

func validateText(name, value string) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("menu: %s is not valid UTF-8", name)
	}
	if len(value) > protocol.MaxTextBytes {
		return fmt.Errorf("menu: %s exceeds %d bytes", name, protocol.MaxTextBytes)
	}
	return nil
}
