package watcher

import (
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/prop"
)

const (
	signalItemRegistered   = "StatusNotifierItemRegistered"
	signalItemUnregistered = "StatusNotifierItemUnregistered"
	signalHostRegistered   = "StatusNotifierHostRegistered"

	dbusInvalidArgs = "org.freedesktop.DBus.Error.InvalidArgs"
)

// Server implements the watcher when this process owns the well-known name.
type Server struct {
	conn     *dbus.Conn
	observer Observer

	mu             sync.Mutex
	items          map[Address]Key
	generation     uint64
	hostRegistered bool
	hosts          map[string]struct{}
	served         bool

	props   *prop.Properties
	signals chan *dbus.Signal
	stop    chan struct{}
	done    chan error
	pumped  chan struct{}
	once    sync.Once
}

func NewServer(conn *dbus.Conn, observer Observer) *Server {
	return &Server{
		conn: conn, observer: observer,
		items: make(map[Address]Key), hosts: make(map[string]struct{}),
		signals: make(chan *dbus.Signal, 32), stop: make(chan struct{}),
		done: make(chan error, 1), pumped: make(chan struct{}),
	}
}

// Serve acquires the watcher name and exports its interface. It fails without
// side effects when another process already owns the name.
func (s *Server) Serve() error {
	if s == nil || s.conn == nil || s.observer == nil {
		return errors.New("watcher: missing server dependency")
	}
	s.mu.Lock()
	if s.served {
		s.mu.Unlock()
		return errors.New("watcher: server already started")
	}
	s.mu.Unlock()

	if err := s.conn.Export(endpoint{server: s}, ObjectPath, Interface); err != nil {
		return fmt.Errorf("watcher: export interface: %w", err)
	}
	props, err := prop.Export(s.conn, ObjectPath, map[string]map[string]*prop.Prop{
		Interface: {
			"RegisteredStatusNotifierItems":  {Value: []string{}, Emit: prop.EmitTrue},
			"IsStatusNotifierHostRegistered": {Value: false, Emit: prop.EmitTrue},
			"ProtocolVersion":                {Value: ProtocolVersion, Emit: prop.EmitConst},
		},
	})
	if err != nil {
		_ = s.conn.Export(nil, ObjectPath, Interface)
		return fmt.Errorf("watcher: export properties: %w", err)
	}
	s.props = props
	if err := s.conn.AddMatchSignal(nameOwnerMatch()...); err != nil {
		s.unexport()
		return fmt.Errorf("watcher: watch name ownership: %w", err)
	}
	s.conn.Signal(s.signals)
	reply, err := s.conn.RequestName(BusName, dbus.NameFlagDoNotQueue)
	if err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		s.conn.RemoveSignal(s.signals)
		_ = s.conn.RemoveMatchSignal(nameOwnerMatch()...)
		s.unexport()
		if err != nil {
			return fmt.Errorf("watcher: acquire %s: %w", BusName, err)
		}
		return fmt.Errorf("watcher: acquire %s: %s", BusName, reply)
	}
	s.mu.Lock()
	s.served = true
	s.mu.Unlock()
	go s.pump()
	return nil
}

// Done reports the loss of the watcher name.
func (s *Server) Done() <-chan error { return s.done }

func (s *Server) Close() error {
	if s == nil || s.conn == nil {
		return nil
	}
	var result error
	s.once.Do(func() {
		close(s.stop)
		s.mu.Lock()
		served := s.served
		s.served = false
		s.mu.Unlock()
		if !served {
			return
		}
		<-s.pumped
		s.conn.RemoveSignal(s.signals)
		result = errors.Join(result, s.conn.RemoveMatchSignal(nameOwnerMatch()...))
		if _, err := s.conn.ReleaseName(BusName); err != nil {
			result = errors.Join(result, err)
		}
		s.unexport()
	})
	return result
}

func (s *Server) unexport() {
	_ = s.conn.Export(nil, ObjectPath, Interface)
	_ = s.conn.Export(nil, ObjectPath, "org.freedesktop.DBus.Properties")
}

func (s *Server) pump() {
	defer close(s.pumped)
	for {
		select {
		case <-s.stop:
			return
		case signal, ok := <-s.signals:
			if !ok {
				return
			}
			s.handleNameOwnerChanged(signal)
		}
	}
}

func (s *Server) handleNameOwnerChanged(signal *dbus.Signal) {
	if signal == nil || signal.Name != "org.freedesktop.DBus.NameOwnerChanged" || len(signal.Body) != 3 {
		return
	}
	name, _ := signal.Body[0].(string)
	newOwner, _ := signal.Body[2].(string)
	if newOwner != "" {
		return
	}
	if name == BusName {
		select {
		case s.done <- fmt.Errorf("watcher: lost %s", BusName):
		default:
		}
		return
	}
	if !isUniqueName(name) {
		return
	}
	for _, key := range s.removeOwner(name) {
		s.emitUnregistered(key)
	}
}

// removeOwner drops every item belonging to a departed owner.
func (s *Server) removeOwner(owner string) []Key {
	s.mu.Lock()
	defer s.mu.Unlock()
	var removed []Key
	for address, key := range s.items {
		if address.Owner == owner {
			delete(s.items, address)
			removed = append(removed, key)
		}
	}
	if len(removed) > 0 {
		s.publishItemsLocked()
	}
	return removed
}

func (s *Server) register(sender, argument string) *dbus.Error {
	address, err := resolveRegistration(sender, argument, busNameOwner(s.conn))
	if err != nil {
		return dbus.NewError(dbusInvalidArgs, []any{err.Error()})
	}
	s.mu.Lock()
	if _, registered := s.items[address]; registered {
		s.mu.Unlock()
		return nil
	}
	s.generation++
	key := Key{Owner: address.Owner, ObjectPath: address.ObjectPath, Generation: s.generation}
	s.items[address] = key
	s.publishItemsLocked()
	s.mu.Unlock()

	s.observer.ItemAdded(key)
	_ = s.conn.Emit(ObjectPath, Interface+"."+signalItemRegistered, address.String())
	return nil
}

func (s *Server) emitUnregistered(key Key) {
	s.observer.ItemRemoved(key)
	address := Address{Owner: key.Owner, ObjectPath: key.ObjectPath}
	_ = s.conn.Emit(ObjectPath, Interface+"."+signalItemUnregistered, address.String())
}

func (s *Server) registerHost(service string) *dbus.Error {
	if service == "" {
		return dbus.NewError(dbusInvalidArgs, []any{"watcher: empty host name"})
	}
	s.mu.Lock()
	_, known := s.hosts[service]
	s.hosts[service] = struct{}{}
	first := !s.hostRegistered
	s.hostRegistered = true
	if first {
		s.setPropLocked("IsStatusNotifierHostRegistered", true)
	}
	s.mu.Unlock()
	if !known {
		_ = s.conn.Emit(ObjectPath, Interface+"."+signalHostRegistered)
	}
	return nil
}

// Items reports the canonical registry in a stable order.
func (s *Server) Items() []Key {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]Key, 0, len(s.items))
	for _, key := range s.items {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Generation < keys[j].Generation })
	return keys
}

func (s *Server) publishItemsLocked() {
	addresses := make([]string, 0, len(s.items))
	for address := range s.items {
		addresses = append(addresses, address.String())
	}
	sort.Strings(addresses)
	s.setPropLocked("RegisteredStatusNotifierItems", addresses)
}

func (s *Server) setPropLocked(name string, value any) {
	if s.props == nil {
		return
	}
	s.props.SetMust(Interface, name, value)
}

type endpoint struct {
	server *Server
}

func (e endpoint) RegisterStatusNotifierItem(sender dbus.Sender, service string) *dbus.Error {
	return e.server.register(string(sender), service)
}

func (e endpoint) RegisterStatusNotifierHost(service string) *dbus.Error {
	return e.server.registerHost(service)
}

func nameOwnerMatch() []dbus.MatchOption {
	return []dbus.MatchOption{
		dbus.WithMatchInterface("org.freedesktop.DBus"),
		dbus.WithMatchMember("NameOwnerChanged"),
		dbus.WithMatchSender("org.freedesktop.DBus"),
	}
}
