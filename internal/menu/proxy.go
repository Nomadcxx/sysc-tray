package menu

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/godbus/dbus/v5"

	"github.com/Nomadcxx/sysc-tray/protocol"
)

// Update is one published menu tree for an item generation. A non-nil error
// means the menu is unavailable; the item itself stays usable, so activate,
// secondary activate, and scroll keep working.
type Update struct {
	Key  protocol.ItemKey
	Menu protocol.Menu
	Err  error
}

// Proxy tracks one item's DBusMenu. Every command names the item generation and
// the revision the shell drew, and both are rechecked immediately before the
// call so a command never lands on a tree the user never saw.
type Proxy struct {
	conn    *dbus.Conn
	key     protocol.ItemKey
	path    dbus.ObjectPath
	publish func(Update)

	signals chan *dbus.Signal
	wake    chan struct{}
	stop    chan struct{}
	pumped  chan struct{}
	looped  chan struct{}
	once    sync.Once
	started bool

	mu       sync.RWMutex
	revision uint32
	known    bool
}

func NewProxy(conn *dbus.Conn, key protocol.ItemKey, path string, publish func(Update)) *Proxy {
	return &Proxy{
		conn: conn, key: key, path: dbus.ObjectPath(path), publish: publish,
		signals: make(chan *dbus.Signal, 64), wake: make(chan struct{}, 1),
		stop: make(chan struct{}), pumped: make(chan struct{}), looped: make(chan struct{}),
	}
}

func (p *Proxy) Start(ctx context.Context) error {
	if p == nil || p.conn == nil || p.publish == nil {
		return errors.New("menu: missing proxy dependency")
	}
	if !p.path.IsValid() {
		return fmt.Errorf("menu: invalid menu path %q", p.path)
	}
	if p.started {
		return errors.New("menu: proxy already started")
	}
	for _, options := range p.matches() {
		if err := p.conn.AddMatchSignal(options...); err != nil {
			p.removeMatches()
			return fmt.Errorf("menu: watch %s: %w", p.key.Owner, err)
		}
	}
	p.conn.Signal(p.signals)
	p.started = true
	go p.pump()
	go p.loop(ctx)
	p.trigger()
	return nil
}

func (p *Proxy) Close() {
	p.once.Do(func() {
		close(p.stop)
		if !p.started {
			return
		}
		<-p.looped
		<-p.pumped
		p.conn.RemoveSignal(p.signals)
		p.removeMatches()
	})
}

// Revision reports the revision of the published tree.
func (p *Proxy) Revision() uint32 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.revision
}

func (p *Proxy) matches() [][]dbus.MatchOption {
	return [][]dbus.MatchOption{
		{
			dbus.WithMatchSender(p.key.Owner),
			dbus.WithMatchObjectPath(p.path),
			dbus.WithMatchInterface(Interface),
		},
		{
			dbus.WithMatchSender("org.freedesktop.DBus"),
			dbus.WithMatchInterface("org.freedesktop.DBus"),
			dbus.WithMatchMember("NameOwnerChanged"),
			dbus.WithMatchArg(0, p.key.Owner),
		},
	}
}

func (p *Proxy) removeMatches() {
	for _, options := range p.matches() {
		_ = p.conn.RemoveMatchSignal(options...)
	}
}

func (p *Proxy) pump() {
	defer close(p.pumped)
	for {
		select {
		case <-p.stop:
			return
		case signal, ok := <-p.signals:
			if !ok {
				return
			}
			p.handle(signal)
		}
	}
}

func (p *Proxy) handle(signal *dbus.Signal) {
	switch signal.Name {
	case "org.freedesktop.DBus.NameOwnerChanged":
		if len(signal.Body) != 3 {
			return
		}
		name, _ := signal.Body[0].(string)
		newOwner, _ := signal.Body[2].(string)
		if name == p.key.Owner && newOwner == "" {
			p.publish(Update{Key: p.key, Err: fmt.Errorf("menu: owner %s left the bus", p.key.Owner)})
		}
	case Interface + ".LayoutUpdated":
		// A revision older than the published one describes a tree that has
		// already been superseded, so refetching it would move backwards.
		if len(signal.Body) < 1 {
			return
		}
		revision, ok := signal.Body[0].(uint32)
		if !ok {
			return
		}
		p.mu.RLock()
		current, known := p.revision, p.known
		p.mu.RUnlock()
		if known && revision < current {
			return
		}
		p.trigger()
	case Interface + ".ItemsPropertiesUpdated":
		// Property-only updates carry no revision; the refetch republishes the
		// same revision with new properties.
		p.trigger()
	}
}

func (p *Proxy) trigger() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *Proxy) loop(ctx context.Context) {
	defer close(p.looped)
	for {
		select {
		case <-p.stop:
			return
		case <-ctx.Done():
			return
		case <-p.wake:
		}
		p.refresh()
	}
}

func (p *Proxy) refresh() {
	revision, layout, err := p.getLayout()
	if err != nil {
		p.publish(Update{Key: p.key, Err: err})
		return
	}
	tree, err := convertLayout(revision, layout)
	if err != nil {
		p.publish(Update{Key: p.key, Err: err})
		return
	}
	if err := Validate(tree); err != nil {
		p.publish(Update{Key: p.key, Err: err})
		return
	}
	p.mu.Lock()
	p.revision, p.known = revision, true
	p.mu.Unlock()
	p.publish(Update{Key: p.key, Menu: tree})
}

func (p *Proxy) getLayout() (uint32, any, error) {
	call := p.object().Call(Interface+".GetLayout", 0, int32(0), int32(-1), []string{})
	if call.Err != nil {
		return 0, nil, fmt.Errorf("menu: read layout from %s: %w", p.key.Owner, call.Err)
	}
	if len(call.Body) != 2 {
		return 0, nil, fmt.Errorf("menu: layout reply from %s has %d values", p.key.Owner, len(call.Body))
	}
	revision, ok := call.Body[0].(uint32)
	if !ok {
		return 0, nil, fmt.Errorf("menu: layout revision from %s has type %T", p.key.Owner, call.Body[0])
	}
	return revision, call.Body[1], nil
}

// Select invokes a menu entry. Opened and Closed report menu lifecycle to the
// application. None of them is ever retried: the effect may already have
// happened, and a repeat would invoke the entry twice.
func (p *Proxy) Select(key protocol.ItemKey, revision uint32, id int32, timestamp uint32) error {
	return p.event(key, revision, id, "clicked", timestamp)
}

func (p *Proxy) Opened(key protocol.ItemKey, revision uint32, id int32) error {
	return p.event(key, revision, id, "opened", 0)
}

func (p *Proxy) Closed(key protocol.ItemKey, revision uint32, id int32) error {
	return p.event(key, revision, id, "closed", 0)
}

func (p *Proxy) event(key protocol.ItemKey, revision uint32, id int32, name string, timestamp uint32) error {
	if err := p.check(key, revision); err != nil {
		return err
	}
	call := p.object().Call(Interface+".Event", 0, id, name, dbus.MakeVariant(""), timestamp)
	if call.Err != nil {
		return &protocol.ProtocolError{Code: protocol.ErrorUnavailable, Message: call.Err.Error()}
	}
	return nil
}

// AboutToShow asks the application to refresh before the menu is drawn. A true
// reply schedules a refetch so the shell draws the updated tree.
func (p *Proxy) AboutToShow(key protocol.ItemKey, revision uint32, id int32) (bool, error) {
	if err := p.check(key, revision); err != nil {
		return false, err
	}
	call := p.object().Call(Interface+".AboutToShow", 0, id)
	if call.Err != nil {
		return false, &protocol.ProtocolError{Code: protocol.ErrorUnavailable, Message: call.Err.Error()}
	}
	if len(call.Body) != 1 {
		return false, &protocol.ProtocolError{Code: protocol.ErrorInvalid, Message: "malformed AboutToShow reply"}
	}
	needed, ok := call.Body[0].(bool)
	if !ok {
		return false, &protocol.ProtocolError{Code: protocol.ErrorInvalid, Message: "malformed AboutToShow reply"}
	}
	if needed {
		p.trigger()
	}
	return needed, nil
}

// check rechecks the item generation and menu revision immediately before a
// call, so a tree that changed while the user was reading it refuses the command
// instead of invoking a different entry.
func (p *Proxy) check(key protocol.ItemKey, revision uint32) error {
	if key != p.key {
		return &protocol.ProtocolError{Code: protocol.ErrorStaleItem, Message: "item generation changed"}
	}
	if current := p.Revision(); current != revision {
		return &protocol.ProtocolError{
			Code:    protocol.ErrorStaleRevision,
			Message: fmt.Sprintf("menu revision is %d", current),
		}
	}
	return nil
}

func (p *Proxy) object() dbus.BusObject {
	return p.conn.Object(p.key.Owner, p.path)
}
