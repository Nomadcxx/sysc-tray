// Package state owns tray service state. One goroutine holds the item map, the
// service sequence, and the presenter sink, so every published message is
// ordered and no worker can mutate state directly.
package state

import (
	"encoding/json"
	"errors"
	"reflect"
	"sort"

	"github.com/Nomadcxx/sysc-tray/protocol"
)

// Sink receives ordered messages for the connected presenter. Send must not
// block: a presenter that cannot keep up returns an error and is dropped, so a
// slow shell can never stall the D-Bus side of the service.
type Sink interface {
	Send(protocol.Envelope) error
}

// address is the reusable half of an item's identity. The generation makes a
// key unique; the address is what a replacement reuses.
type address struct {
	owner string
	path  string
}

type entry struct {
	key       protocol.ItemKey
	item      protocol.Item
	menu      protocol.Menu
	hasItem   bool
	hasMenu   bool
	published bool
}

type Owner struct {
	commands chan func()
	stopped  chan struct{}
	done     chan struct{}

	items    map[address]*entry
	order    []address
	sequence uint64
	sink     Sink
}

func Start() *Owner {
	owner := &Owner{
		commands: make(chan func()), stopped: make(chan struct{}), done: make(chan struct{}),
		items: make(map[address]*entry),
	}
	go owner.run()
	return owner
}

func (o *Owner) run() {
	defer close(o.done)
	for {
		select {
		case <-o.stopped:
			return
		case command := <-o.commands:
			command()
		}
	}
}

func (o *Owner) Close() error {
	select {
	case <-o.stopped:
		return nil
	default:
	}
	close(o.stopped)
	<-o.done
	return nil
}

// submit runs one command on the owning goroutine and waits for it, so callers
// observe a consistent state without sharing a lock with the run loop.
func (o *Owner) submit(command func()) bool {
	finished := make(chan struct{})
	select {
	case <-o.stopped:
		return false
	case o.commands <- func() { defer close(finished); command() }:
		<-finished
		return true
	}
}

// Add registers a generation. When the item cap is full the registration is
// refused and every existing item is left untouched.
func (o *Owner) Add(key protocol.ItemKey) {
	o.submit(func() {
		spot := address{owner: key.Owner, path: key.ObjectPath}
		existing, known := o.items[spot]
		if known {
			if existing.key.Generation >= key.Generation {
				return
			}
			// A replacement retires the old generation before the new one
			// appears, so a presenter never holds two live generations.
			o.retire(spot, existing)
		} else if len(o.items) >= protocol.MaxItems {
			return
		}
		o.items[spot] = &entry{key: key}
		o.order = append(o.order, spot)
	})
}

// Update accepts a refresh result. A result naming a superseded generation is
// discarded, so a slow reply cannot revive a dead item.
func (o *Owner) Update(key protocol.ItemKey, item protocol.Item) {
	o.submit(func() {
		record, ok := o.current(key)
		if !ok {
			return
		}
		item.Key = key
		if err := item.Validate(); err != nil {
			// One malformed item cannot remove a healthy sibling; it simply
			// never becomes publishable.
			return
		}
		next := cloneItem(item)
		// A read that found nothing new is not an event. The ownership
		// inspection refreshes every registration once, and applications emit
		// property signals far more often than their properties actually
		// change, so publishing unconditionally spends a marshal and a frame
		// per no-op and makes "nothing changed" indistinguishable on the wire
		// from "changed back".
		if record.published && record.hasItem && reflect.DeepEqual(record.item, next) {
			return
		}
		record.item = next
		record.hasItem = true
		if record.published {
			o.publish(protocol.KindItemChanged, record.item)
			return
		}
		record.published = true
		o.publish(protocol.KindItemAdded, record.item)
	})
}

// UpdateMenu accepts a menu tree for a live generation.
func (o *Owner) UpdateMenu(key protocol.ItemKey, tree protocol.Menu) {
	o.submit(func() {
		record, ok := o.current(key)
		if !ok || !record.published {
			return
		}
		if err := tree.Validate(); err != nil {
			return
		}
		record.menu = cloneMenu(tree)
		record.hasMenu = true
		o.publish(protocol.KindMenuUpdated, protocol.MenuUpdate{Key: key, Menu: record.menu})
	})
}

// Remove drops a generation. A stale removal is ignored.
func (o *Owner) Remove(key protocol.ItemKey) {
	o.submit(func() {
		record, ok := o.current(key)
		if !ok {
			return
		}
		o.retire(address{owner: key.Owner, path: key.ObjectPath}, record)
	})
}

// Menu reports the published menu for a live generation.
func (o *Owner) Menu(key protocol.ItemKey) (protocol.Menu, bool) {
	var tree protocol.Menu
	var ok bool
	o.submit(func() {
		record, live := o.current(key)
		if !live || !record.hasMenu {
			return
		}
		tree, ok = cloneMenu(record.menu), true
	})
	return tree, ok
}

// Snapshot reports published items at the current sequence.
func (o *Owner) Snapshot() protocol.Snapshot {
	var snapshot protocol.Snapshot
	o.submit(func() { snapshot = o.snapshotLocked() })
	return snapshot
}

// Attach installs the presenter sink and returns the baseline it must apply
// before any delta. Deltas after this snapshot are contiguous from its sequence.
func (o *Owner) Attach(sink Sink) (protocol.Snapshot, error) {
	if sink == nil {
		return protocol.Snapshot{}, errors.New("state: nil presenter sink")
	}
	var snapshot protocol.Snapshot
	if !o.submit(func() {
		o.sink = sink
		snapshot = o.snapshotLocked()
	}) {
		return protocol.Snapshot{}, errors.New("state: owner is closed")
	}
	return snapshot, nil
}

// Detach removes a sink if it is still the connected one.
func (o *Owner) Detach(sink Sink) {
	o.submit(func() {
		if o.sink == sink {
			o.sink = nil
		}
	})
}

func (o *Owner) current(key protocol.ItemKey) (*entry, bool) {
	record, ok := o.items[address{owner: key.Owner, path: key.ObjectPath}]
	if !ok || record.key != key {
		return nil, false
	}
	return record, true
}

func (o *Owner) retire(spot address, record *entry) {
	delete(o.items, spot)
	for i, candidate := range o.order {
		if candidate == spot {
			o.order = append(o.order[:i], o.order[i+1:]...)
			break
		}
	}
	if record.published {
		o.publish(protocol.KindItemRemoved, protocol.ItemRemoved{Key: record.key})
	}
}

func (o *Owner) snapshotLocked() protocol.Snapshot {
	snapshot := protocol.Snapshot{Sequence: o.sequence}
	for _, spot := range o.order {
		record, ok := o.items[spot]
		if !ok || !record.published {
			continue
		}
		snapshot.Items = append(snapshot.Items, cloneItem(record.item))
	}
	sort.SliceStable(snapshot.Items, func(i, j int) bool {
		return snapshot.Items[i].Key.Generation < snapshot.Items[j].Key.Generation
	})
	return snapshot
}

// publish advances the service sequence and sends one delta. A sink that
// refuses the message is dropped rather than retried.
func (o *Owner) publish(kind string, payload any) {
	o.sequence++
	if o.sink == nil {
		return
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		o.sink = nil
		return
	}
	envelope := protocol.Envelope{Kind: kind, Sequence: o.sequence, Payload: encoded}
	if err := o.sink.Send(envelope); err != nil {
		o.sink = nil
	}
}

func cloneItem(item protocol.Item) protocol.Item {
	item.Icon = cloneIcon(item.Icon)
	item.AttentionIcon = cloneIcon(item.AttentionIcon)
	item.OverlayIcon = cloneIcon(item.OverlayIcon)
	item.Tooltip.Pixmaps = clonePixmaps(item.Tooltip.Pixmaps)
	return item
}

func cloneIcon(icon protocol.Icon) protocol.Icon {
	icon.Pixmaps = clonePixmaps(icon.Pixmaps)
	return icon
}

func clonePixmaps(pixmaps []protocol.Pixmap) []protocol.Pixmap {
	if pixmaps == nil {
		return nil
	}
	copied := make([]protocol.Pixmap, len(pixmaps))
	for i, pixmap := range pixmaps {
		pixmap.ARGB = append([]byte(nil), pixmap.ARGB...)
		copied[i] = pixmap
	}
	return copied
}

func cloneMenu(tree protocol.Menu) protocol.Menu {
	tree.Root = cloneNode(tree.Root)
	return tree
}

func cloneNode(node protocol.MenuNode) protocol.MenuNode {
	node.IconData = append([]byte(nil), node.IconData...)
	if node.Children == nil {
		return node
	}
	children := make([]protocol.MenuNode, len(node.Children))
	for i, child := range node.Children {
		children[i] = cloneNode(child)
	}
	node.Children = children
	return node
}
