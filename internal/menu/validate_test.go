package menu

import (
	"strings"
	"testing"

	"github.com/Nomadcxx/sysc-tray/protocol"
)

func TestValidateAcceptsABoundedTree(t *testing.T) {
	tree := protocol.Menu{Revision: 3, Root: protocol.MenuNode{ID: 0, Visible: true, Children: []protocol.MenuNode{
		{ID: 1, Label: "Open", Enabled: true, Visible: true},
		{ID: 2, Separator: true, Visible: true},
		{ID: 3, Label: "Quit", Enabled: true, Visible: true, ToggleType: protocol.ToggleCheckmark, ToggleState: 1},
	}}}
	if err := Validate(tree); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejectsDepthBeyondTheBound(t *testing.T) {
	if err := Validate(chain(protocol.MaxMenuDepth)); err != nil {
		t.Fatalf("tree at the depth bound was rejected: %v", err)
	}
	if err := Validate(chain(protocol.MaxMenuDepth + 1)); err == nil {
		t.Fatal("tree past the depth bound was accepted")
	}
}

func TestValidateRejectsNodeCountBeyondTheBound(t *testing.T) {
	if err := Validate(fan(protocol.MaxMenuNodes - 1)); err != nil {
		t.Fatalf("tree at the node bound was rejected: %v", err)
	}
	if err := Validate(fan(protocol.MaxMenuNodes)); err == nil {
		t.Fatal("tree past the node bound was accepted")
	}
}

func TestValidateRejectsRepeatedIdentity(t *testing.T) {
	tree := protocol.Menu{Root: protocol.MenuNode{ID: 0, Children: []protocol.MenuNode{
		{ID: 5, Label: "One"},
		{ID: 5, Label: "Two"},
	}}}
	if err := Validate(tree); err == nil {
		t.Fatal("duplicate menu identity was accepted")
	}
	nested := protocol.Menu{Root: protocol.MenuNode{ID: 0, Children: []protocol.MenuNode{
		{ID: 1, Children: []protocol.MenuNode{{ID: 0}}},
	}}}
	if err := Validate(nested); err == nil {
		t.Fatal("a node repeating the root identity was accepted")
	}
}

func TestValidateRejectsOversizedContent(t *testing.T) {
	label := protocol.Menu{Root: protocol.MenuNode{ID: 0, Children: []protocol.MenuNode{
		{ID: 1, Label: strings.Repeat("x", protocol.MaxTextBytes+1)},
	}}}
	if err := Validate(label); err == nil {
		t.Fatal("oversized label was accepted")
	}
	icons := protocol.Menu{Root: protocol.MenuNode{ID: 0, Children: []protocol.MenuNode{
		{ID: 1, IconData: make([]byte, protocol.MaxMenuIconBytes)},
		{ID: 2, IconData: make([]byte, protocol.MaxMenuIconBytes)},
	}}}
	if err := Validate(icons); err == nil {
		t.Fatal("aggregate icon data past the cap was accepted")
	}
}

func TestValidateRejectsUnknownVocabulary(t *testing.T) {
	toggle := protocol.Menu{Root: protocol.MenuNode{ID: 0, Children: []protocol.MenuNode{
		{ID: 1, ToggleType: protocol.ToggleType("tristate")},
	}}}
	if err := Validate(toggle); err == nil {
		t.Fatal("unknown toggle type was accepted")
	}
	display := protocol.Menu{Root: protocol.MenuNode{ID: 0, Children: []protocol.MenuNode{
		{ID: 1, ChildrenDisplay: "grid"},
	}}}
	if err := Validate(display); err == nil {
		t.Fatal("unknown children display was accepted")
	}
	root := protocol.Menu{Root: protocol.MenuNode{ID: 4}}
	if err := Validate(root); err == nil {
		t.Fatal("a tree whose root is not identity zero was accepted")
	}
	negative := protocol.Menu{Root: protocol.MenuNode{ID: 0, Children: []protocol.MenuNode{{ID: -2}}}}
	if err := Validate(negative); err == nil {
		t.Fatal("negative menu identity was accepted")
	}
}

// chain builds a tree whose deepest node sits at the given depth below the root.
func chain(depth int) protocol.Menu {
	tree := protocol.Menu{Root: protocol.MenuNode{ID: 0}}
	node := &tree.Root
	for i := 1; i <= depth; i++ {
		node.Children = []protocol.MenuNode{{ID: int32(i)}}
		node = &node.Children[0]
	}
	return tree
}

// fan builds a root with the given number of children.
func fan(children int) protocol.Menu {
	tree := protocol.Menu{Root: protocol.MenuNode{ID: 0, Children: make([]protocol.MenuNode, children)}}
	for i := range tree.Root.Children {
		tree.Root.Children[i].ID = int32(i + 1)
	}
	return tree
}
