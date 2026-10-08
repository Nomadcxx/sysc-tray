package watcher

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/godbus/dbus/v5"
)

// Mode reports how the watcher is currently participating.
type Mode int32

const (
	ModeStopped Mode = iota
	ModeOwner
	ModeAttached
)

func (m Mode) String() string {
	switch m {
	case ModeOwner:
		return "owner"
	case ModeAttached:
		return "attached"
	default:
		return "stopped"
	}
}

const (
	initialBackoff = 50 * time.Millisecond
	maxBackoff     = 5 * time.Second
)

// Watcher owns the run loop that keeps exactly one watcher relationship alive:
// it serves the name when free and attaches as a host when a healthy watcher
// already owns it. A healthy external watcher stays authoritative.
type Watcher struct {
	conn     *dbus.Conn
	observer Observer
	mode     atomic.Int32
}

func New(conn *dbus.Conn, observer Observer) *Watcher {
	return &Watcher{conn: conn, observer: observer}
}

func (w *Watcher) Mode() Mode { return Mode(w.mode.Load()) }

// Run blocks until the context is cancelled, moving between owning and
// attaching as ownership of the watcher name changes.
func (w *Watcher) Run(ctx context.Context) error {
	if w == nil || w.conn == nil || w.observer == nil {
		return errors.New("watcher: missing dependency")
	}
	backoff := initialBackoff
	for {
		if err := ctx.Err(); err != nil {
			w.mode.Store(int32(ModeStopped))
			return err
		}
		progressed, err := w.serveOnce(ctx)
		if err != nil && ctx.Err() != nil {
			w.mode.Store(int32(ModeStopped))
			return ctx.Err()
		}
		if progressed {
			backoff = initialBackoff
			continue
		}
		if err := sleep(ctx, backoff); err != nil {
			w.mode.Store(int32(ModeStopped))
			return err
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// serveOnce holds one relationship until it ends. It reports whether the
// attempt reached a working state, so only a failed attempt pays the backoff.
func (w *Watcher) serveOnce(ctx context.Context) (bool, error) {
	server := NewServer(w.conn, w.observer)
	if err := server.Serve(); err == nil {
		defer func() {
			_ = server.Close()
			w.mode.Store(int32(ModeStopped))
		}()
		// The service is its own host while it owns the watcher.
		_ = server.registerHost(HostName(w.conn))
		w.mode.Store(int32(ModeOwner))
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		case <-server.Done():
			return true, nil
		}
	}

	client := newClient(w.conn, w.observer)
	if err := client.attach(); err != nil {
		client.close()
		return false, err
	}
	defer func() {
		client.close()
		w.mode.Store(int32(ModeStopped))
	}()
	w.mode.Store(int32(ModeAttached))
	select {
	case <-ctx.Done():
		return true, ctx.Err()
	case <-client.lost():
		return true, nil
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// client attaches to an external watcher as a StatusNotifierHost.
type client struct {
	conn     *dbus.Conn
	observer Observer

	signals chan *dbus.Signal
	stop    chan struct{}
	gone    chan struct{}
	pumped  chan struct{}
	once    sync.Once

	mu         sync.Mutex
	items      map[Address]Key
	generation uint64
	attached   bool
}

func newClient(conn *dbus.Conn, observer Observer) *client {
	return &client{
		conn: conn, observer: observer, items: make(map[Address]Key),
		signals: make(chan *dbus.Signal, 32), stop: make(chan struct{}),
		gone: make(chan struct{}), pumped: make(chan struct{}),
	}
}

func (c *client) lost() <-chan struct{} { return c.gone }

func (c *client) attach() error {
	owner, err := busNameOwner(c.conn)(BusName)
	if err != nil {
		return fmt.Errorf("watcher: no external watcher: %w", err)
	}
	matches := watcherSignalMatches()
	if err := c.conn.AddMatchSignal(matches...); err != nil {
		return fmt.Errorf("watcher: watch registrations: %w", err)
	}
	if err := c.conn.AddMatchSignal(nameOwnerMatch()...); err != nil {
		_ = c.conn.RemoveMatchSignal(matches...)
		return fmt.Errorf("watcher: watch watcher ownership: %w", err)
	}
	c.conn.Signal(c.signals)
	c.mu.Lock()
	c.attached = true
	c.mu.Unlock()
	go c.pump()

	object := c.conn.Object(BusName, ObjectPath)
	if call := object.Call(Interface+".RegisterStatusNotifierHost", 0, HostName(c.conn)); call.Err != nil {
		return fmt.Errorf("watcher: register host with %s: %w", owner, call.Err)
	}
	variant, err := object.GetProperty(Interface + ".RegisteredStatusNotifierItems")
	if err != nil {
		return fmt.Errorf("watcher: read registered items: %w", err)
	}
	entries, _ := variant.Value().([]string)
	for _, entry := range entries {
		c.add(entry)
	}
	return nil
}

func (c *client) close() {
	c.once.Do(func() {
		close(c.stop)
		c.mu.Lock()
		attached := c.attached
		c.attached = false
		c.mu.Unlock()
		if !attached {
			return
		}
		<-c.pumped
		c.conn.RemoveSignal(c.signals)
		_ = c.conn.RemoveMatchSignal(watcherSignalMatches()...)
		_ = c.conn.RemoveMatchSignal(nameOwnerMatch()...)
		for _, key := range c.drain() {
			c.observer.ItemRemoved(key)
		}
	})
}

func (c *client) pump() {
	defer close(c.pumped)
	for {
		select {
		case <-c.stop:
			return
		case signal, ok := <-c.signals:
			if !ok {
				return
			}
			c.handle(signal)
		}
	}
}

func (c *client) handle(signal *dbus.Signal) {
	if signal == nil {
		return
	}
	switch signal.Name {
	case "org.freedesktop.DBus.NameOwnerChanged":
		if len(signal.Body) != 3 {
			return
		}
		if name, _ := signal.Body[0].(string); name != BusName {
			return
		}
		if newOwner, _ := signal.Body[2].(string); newOwner == "" {
			c.markGone()
		}
	case Interface + "." + signalItemRegistered:
		if len(signal.Body) == 1 {
			if entry, ok := signal.Body[0].(string); ok {
				c.add(entry)
			}
		}
	case Interface + "." + signalItemUnregistered:
		if len(signal.Body) == 1 {
			if entry, ok := signal.Body[0].(string); ok {
				c.remove(entry)
			}
		}
	}
}

func (c *client) markGone() {
	select {
	case <-c.gone:
	default:
		close(c.gone)
	}
}

func (c *client) add(entry string) {
	address, err := parseRegisteredItem(entry, busNameOwner(c.conn))
	if err != nil {
		return
	}
	c.mu.Lock()
	previous, replaced := c.items[address]
	if replaced {
		delete(c.items, address)
	}
	c.generation++
	key := Key{Owner: address.Owner, ObjectPath: address.ObjectPath, Generation: c.generation}
	c.items[address] = key
	c.mu.Unlock()

	if replaced {
		c.observer.ItemRemoved(previous)
	}
	c.observer.ItemAdded(key)
}

func (c *client) remove(entry string) {
	address, err := parseRegisteredItem(entry, busNameOwner(c.conn))
	if err != nil {
		return
	}
	c.mu.Lock()
	key, known := c.items[address]
	delete(c.items, address)
	c.mu.Unlock()
	if known {
		c.observer.ItemRemoved(key)
	}
}

func (c *client) drain() []Key {
	c.mu.Lock()
	defer c.mu.Unlock()
	keys := make([]Key, 0, len(c.items))
	for address, key := range c.items {
		keys = append(keys, key)
		delete(c.items, address)
	}
	return keys
}

func watcherSignalMatches() []dbus.MatchOption {
	return []dbus.MatchOption{
		dbus.WithMatchSender(BusName),
		dbus.WithMatchInterface(Interface),
	}
}
