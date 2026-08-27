# sysc-tray Roadmap

Date: 2026-08-27

## M0: Compatibility contract

- pin service-name variants, watcher takeover, host registration, and item addressing;
- define bounds for pixmaps, properties, menus, and update rates;
- exercise Pawbar and DMS recovery behavior against current applications;
- define the first shell IPC snapshot and commands.

Gate: private-bus fixtures reproduce the required watcher and item lifecycles.

## M1: Watcher, host, and items

- acquire or attach to the watcher without fighting a healthy owner;
- register the host and track items by unique owner plus object path;
- handle every required item update signal;
- implement activate, secondary activate, context menu, and scroll calls.

Gate: item registration, owner replacement, malformed properties, and service restart pass race tests.

## M2: Shell transport

- add the private Unix socket, peer validation, bounds, and version handshake;
- send a full current snapshot and ordered item changes;
- retain D-Bus state while the shell is absent;
- recover a restarted shell without asking applications to restart.

Gate: repeated shell reconnect preserves the correct set and state of tray items.

## M3: DBusMenu

- fetch and update revisioned menu trees on demand;
- normalize renderer-neutral menu properties;
- send menu events and handle about-to-show refresh;
- bound tree depth, node count, strings, and update frequency.

Gate: menu failure cannot remove or block an otherwise healthy tray item.

## M4: sysc-shell presentation and release

- render icons and attention state in each configured bar;
- render keyboard-accessible menu surfaces inside output bounds;
- qualify representative applications, watcher takeover, and restart order;
- tag `v0.1.0` with the matching shell protocol version.

Legacy XEmbed icons remain outside scope.
