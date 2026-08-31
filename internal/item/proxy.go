package item

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/Nomadcxx/sysc-tray/protocol"
)

// Result is one completed refresh for a generation. A non-nil error means the
// item is gone; the state owner drops only that item.
type Result struct {
	Key  protocol.ItemKey
	Item protocol.Item
	Err  error
}

// Proxy reads one item over D-Bus and publishes generation-tagged results. It
// never mutates published state: the state owner accepts or discards each
// result, so a slow reply from an old generation cannot revive a dead item.
type Proxy struct {
	conn    *dbus.Conn
	key     protocol.ItemKey
	limiter *Limiter
	publish func(Result)

	signals chan *dbus.Signal
	wake    chan struct{}
	stop    chan struct{}
	pumped  chan struct{}
	looped  chan struct{}
	once    sync.Once
	started bool
}

func NewProxy(conn *dbus.Conn, key protocol.ItemKey, limiter *Limiter, publish func(Result)) *Proxy {
	return &Proxy{
		conn: conn, key: key, limiter: limiter, publish: publish,
		signals: make(chan *dbus.Signal, 64), wake: make(chan struct{}, 1),
		stop: make(chan struct{}), pumped: make(chan struct{}), looped: make(chan struct{}),
	}
}

// Start subscribes to the item's change signals and schedules a first refresh.
func (p *Proxy) Start(ctx context.Context) error {
	if p == nil || p.conn == nil || p.limiter == nil || p.publish == nil {
		return errors.New("item: missing proxy dependency")
	}
	if p.started {
		return errors.New("item: proxy already started")
	}
	for _, options := range p.matches() {
		if err := p.conn.AddMatchSignal(options...); err != nil {
			p.removeMatches()
			return fmt.Errorf("item: watch %s: %w", p.key.Owner, err)
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
		p.limiter.Forget(p.key)
	})
}

func (p *Proxy) matches() [][]dbus.MatchOption {
	return [][]dbus.MatchOption{
		{
			dbus.WithMatchSender(p.key.Owner),
			dbus.WithMatchObjectPath(dbus.ObjectPath(p.key.ObjectPath)),
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
			if p.ownerLeft(signal) {
				p.publish(Result{Key: p.key, Err: fmt.Errorf("item: owner %s left the bus", p.key.Owner)})
				return
			}
			if signal.Path == dbus.ObjectPath(p.key.ObjectPath) {
				p.trigger()
			}
		}
	}
}

func (p *Proxy) ownerLeft(signal *dbus.Signal) bool {
	if signal.Name != "org.freedesktop.DBus.NameOwnerChanged" || len(signal.Body) != 3 {
		return false
	}
	name, _ := signal.Body[0].(string)
	newOwner, _ := signal.Body[2].(string)
	return name == p.key.Owner && newOwner == ""
}

// trigger marks the item dirty. Repeated signals collapse into the single
// pending slot, so a burst costs one refresh rather than one per signal.
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
		if !p.await(ctx) {
			return
		}
		p.refresh()
	}
}

// await holds until the limiter admits this refresh.
func (p *Proxy) await(ctx context.Context) bool {
	for {
		wait := p.limiter.Reserve(p.key)
		if wait <= 0 {
			return true
		}
		timer := time.NewTimer(wait)
		select {
		case <-p.stop:
			timer.Stop()
			return false
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}

func (p *Proxy) refresh() {
	var props map[string]dbus.Variant
	object := p.conn.Object(p.key.Owner, dbus.ObjectPath(p.key.ObjectPath))
	call := object.Call("org.freedesktop.DBus.Properties.GetAll", 0, ItemInterface)
	if call.Err != nil {
		p.publish(Result{Key: p.key, Err: fmt.Errorf("item: read %s: %w", p.key.Owner, call.Err)})
		return
	}
	if err := call.Store(&props); err != nil {
		p.publish(Result{Key: p.key, Err: fmt.Errorf("item: decode %s: %w", p.key.Owner, err)})
		return
	}
	item := normalize(p.key, props)
	if err := item.Validate(); err != nil {
		p.publish(Result{Key: p.key, Err: fmt.Errorf("item: %s produced invalid state: %w", p.key.Owner, err)})
		return
	}
	p.publish(Result{Key: p.key, Item: item})
}

// Activate, SecondaryActivate, and Scroll invoke the item's own methods with
// compositor-logical coordinates. Each rechecks the item generation immediately
// before the call, and none is ever retried: the effect may already have
// happened and a repeat would activate the item twice.
func (p *Proxy) Activate(key protocol.ItemKey, x, y int32) error {
	return p.invoke(key, "Activate", x, y)
}

func (p *Proxy) SecondaryActivate(key protocol.ItemKey, x, y int32) error {
	return p.invoke(key, "SecondaryActivate", x, y)
}

func (p *Proxy) Scroll(key protocol.ItemKey, delta int32, orientation protocol.ScrollOrientation) error {
	switch orientation {
	case protocol.ScrollVertical, protocol.ScrollHorizontal:
	default:
		return &protocol.ProtocolError{Code: protocol.ErrorInvalid, Message: "unknown scroll orientation"}
	}
	return p.invoke(key, "Scroll", delta, string(orientation))
}

func (p *Proxy) invoke(key protocol.ItemKey, member string, args ...any) error {
	if key != p.key {
		return &protocol.ProtocolError{Code: protocol.ErrorStaleItem, Message: "item generation changed"}
	}
	object := p.conn.Object(p.key.Owner, dbus.ObjectPath(p.key.ObjectPath))
	if call := object.Call(ItemInterface+"."+member, 0, args...); call.Err != nil {
		return &protocol.ProtocolError{Code: protocol.ErrorUnavailable, Message: call.Err.Error()}
	}
	return nil
}
