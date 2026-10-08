// Package watcher owns StatusNotifierWatcher policy. It either serves the
// watcher name itself or attaches to a healthy external watcher as a host, and
// publishes canonical item identities either way.
package watcher

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/godbus/dbus/v5"
)

const (
	BusName                    = "org.kde.StatusNotifierWatcher"
	Interface                  = "org.kde.StatusNotifierWatcher"
	ObjectPath dbus.ObjectPath = "/StatusNotifierWatcher"

	ItemInterface                   = "org.kde.StatusNotifierItem"
	DefaultItemPath dbus.ObjectPath = "/StatusNotifierItem"

	HostNamePrefix        = "org.kde.StatusNotifierHost-"
	ProtocolVersion int32 = 0
)

// Key names one item generation. A well-known bus name is reusable, so it never
// identifies an item on its own: identity is the unique owner, the object path,
// and a generation that advances when a retired pair is registered again.
type Key struct {
	Owner      string
	ObjectPath dbus.ObjectPath
	Generation uint64
}

// Address is an owner and path pair before a generation is assigned.
type Address struct {
	Owner      string
	ObjectPath dbus.ObjectPath
}

func (a Address) String() string { return a.Owner + string(a.ObjectPath) }

// Observer receives registry transitions in order. A retired generation is
// reported as removed before the address can be registered again.
type Observer interface {
	ItemAdded(Key)
	ItemRemoved(Key)
}

// HostName reports the service name this connection registers a host under.
func HostName(conn *dbus.Conn) string {
	if conn != nil {
		if names := conn.Names(); len(names) > 0 {
			return names[0]
		}
	}
	return fmt.Sprintf("%s%d", HostNamePrefix, os.Getpid())
}

type nameResolver func(string) (string, error)

// resolveRegistration maps a registration argument onto a canonical address.
// The argument is either an object path, in which case the sender owns it, or a
// bus name that must resolve to a live unique owner.
func resolveRegistration(sender, argument string, resolve nameResolver) (Address, error) {
	if argument == "" {
		return Address{}, errors.New("watcher: empty registration argument")
	}
	if strings.HasPrefix(argument, "/") {
		path := dbus.ObjectPath(argument)
		if !path.IsValid() {
			return Address{}, fmt.Errorf("watcher: invalid object path %q", argument)
		}
		if !isUniqueName(sender) {
			return Address{}, fmt.Errorf("watcher: sender %q is not a unique name", sender)
		}
		return Address{Owner: sender, ObjectPath: path}, nil
	}
	if !isBusName(argument) {
		return Address{}, fmt.Errorf("watcher: %q is neither an object path nor a bus name", argument)
	}
	owner, err := resolve(argument)
	if err != nil {
		return Address{}, fmt.Errorf("watcher: resolve %q: %w", argument, err)
	}
	if !isUniqueName(owner) {
		return Address{}, fmt.Errorf("watcher: %q resolved to %q", argument, owner)
	}
	return Address{Owner: owner, ObjectPath: DefaultItemPath}, nil
}

// parseRegisteredItem reads an entry published by an external watcher. KDE
// watchers concatenate the owner and the object path.
func parseRegisteredItem(entry string, resolve nameResolver) (Address, error) {
	if entry == "" {
		return Address{}, errors.New("watcher: empty registered item")
	}
	name, path := entry, string(DefaultItemPath)
	if index := strings.Index(entry, "/"); index >= 0 {
		name, path = entry[:index], entry[index:]
	}
	if !dbus.ObjectPath(path).IsValid() {
		return Address{}, fmt.Errorf("watcher: invalid object path in %q", entry)
	}
	owner := name
	if !isUniqueName(owner) {
		resolved, err := resolve(name)
		if err != nil {
			return Address{}, fmt.Errorf("watcher: resolve %q: %w", name, err)
		}
		owner = resolved
	}
	if !isUniqueName(owner) {
		return Address{}, fmt.Errorf("watcher: %q resolved to %q", name, owner)
	}
	return Address{Owner: owner, ObjectPath: dbus.ObjectPath(path)}, nil
}

func isUniqueName(name string) bool {
	return strings.HasPrefix(name, ":") && len(name) > 1
}

func isBusName(name string) bool {
	if isUniqueName(name) {
		return true
	}
	return strings.Contains(name, ".") && !strings.HasPrefix(name, ".") && !strings.HasSuffix(name, ".")
}

func busNameOwner(conn *dbus.Conn) nameResolver {
	return func(name string) (string, error) {
		var owner string
		err := conn.BusObject().Call("org.freedesktop.DBus.GetNameOwner", 0, name).Store(&owner)
		return owner, err
	}
}
