# sysc-tray

`sysc-tray` is a Go StatusNotifierItem and DBusMenu service for Linux desktops. It owns tray discovery
and D-Bus interaction, then sends renderer-neutral item and menu state to
[`sysc-shell`](https://github.com/Nomadcxx/sysc-shell).

The repository is in the design stage. It contains no production service yet.

## Responsibilities

The first releases will provide:

- StatusNotifierWatcher ownership or attachment and host registration;
- item registration, removal, property updates, activation, context menus, and scrolling;
- bounded validation of icon pixmaps, attention state, overlays, and tooltips;
- a renderer-neutral DBusMenu model and menu-event client;
- a versioned Unix-socket protocol with reconnect snapshots;
- recovery from item, watcher, bus, and shell restarts.

`sysc-shell` owns bar placement, icon-theme lookup, drawing, input, menu surfaces, and styling. `sysc-tray`
does not import Wayland or render UI.

The initial service targets common StatusNotifierItem implementations. Legacy XEmbed tray icons and
cross-platform abstractions are outside scope.

## Development gates

1. Pin watcher, host, item, and DBusMenu behavior with compatibility fixtures.
2. Implement watcher/host lifecycle and complete item update handling.
3. Add bounded shell IPC with current-state recovery after reconnect.
4. Implement the DBusMenu client and shell-owned menu presentation.
5. Qualify representative applications and restart sequences before `v0.1.0`.

See the [design](docs/plans/2026-08-27-sysc-tray-design.md) and [roadmap](docs/roadmap.md).
Package directories will arrive with their first tested behavior.

## Licence

`sysc-tray` uses the [BSD 3-Clause License](LICENSE).
