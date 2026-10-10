<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/wordmark.png">
    <img src="assets/wordmark-light.png" alt="sysc-tray" height="64">
  </picture>
</p>

`sysc-tray` is a Go StatusNotifierItem and DBusMenu service for Linux desktops. It owns tray discovery
and D-Bus interaction, then sends renderer-neutral item and menu state to
[`sysc-shell`](https://github.com/Nomadcxx/sysc-shell).

## Quick Links

- [Documentation site](https://nomadcxx.github.io/sysc/docs/components/sysc-tray/)
- [Releases](https://github.com/Nomadcxx/sysc-tray/releases)
- [Contributing](https://github.com/Nomadcxx/sysc-tray/issues)
- [`sysc-shell`](https://github.com/Nomadcxx/sysc-shell) — the shell that draws the tray
- [`sysc`](https://github.com/Nomadcxx/sysc) — the installer that wires the session together

## Installation

### Requirements

Linux with a session bus and `XDG_RUNTIME_DIR`. Go 1.26 to build from source.

### From source

```bash
go build -trimpath -o ~/.local/bin/sysc-tray ./cmd/sysc-tray
```

### As a user service

```bash
cp contrib/sysc-tray.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now sysc-tray.service
```

`sysc install` does the same thing and hands the tray over from an existing provider.

## Usage

The daemon serves one presenter at `$XDG_RUNTIME_DIR/sysc-tray/presenter.v1.sock`, owned by the
calling user with mode `0600`. SIGINT and SIGTERM shut it down: it stops accepting presenters, stops
the per-item readers, releases the watcher relationship, closes the bus, and removes its socket.

## Responsibilities

The service provides:

- StatusNotifierWatcher ownership or attachment and host registration;
- item registration, removal, property updates, activation, context menus, and scrolling;
- bounded validation of icon pixmaps, attention state, overlays, and tooltips;
- a renderer-neutral DBusMenu model and menu-event client;
- a versioned Unix-socket protocol with reconnect snapshots;
- recovery from item, watcher, bus, and shell restarts.

Items are identified by unique bus owner, object path, and generation, so a reused well-known name never
addresses a retired item. Menu commands carry the revision the shell drew and are refused rather than
replayed when the tree has moved on.

`sysc-shell` owns bar placement, icon-theme lookup, drawing, input, menu surfaces, and styling. `sysc-tray`
does not import Wayland or render UI.

The initial service targets common StatusNotifierItem implementations. Legacy XEmbed tray icons and
cross-platform abstractions are outside scope.

## Tests

```bash
go test -race ./...
dbus-run-session -- go test -race ./tests/integration/
```

The integration suite runs the daemon against fake applications on a private bus, covering registration
forms, owner replacement, icons and pixmaps, tooltips, pointer commands, menu revisions, submenus, and
stale-revision refusal.

## Ecosystem

- [`sysc`](https://github.com/Nomadcxx/sysc) — the installer for the session
- [`sysc-shell`](https://github.com/Nomadcxx/sysc-shell) — the renderer that presents tray state
- [`sysc-notify`](https://github.com/Nomadcxx/sysc-notify) — the sibling notification service

## Documentation

- [The sysc ecosystem](https://github.com/Nomadcxx/sysc-shell/blob/main/docs/ecosystem.md)

Design plans and roadmaps are kept out of the repository on purpose; they live as local files under
`docs/plans/` and `docs/roadmap.md`.

## License

`sysc-tray` uses the [BSD 3-Clause License](LICENSE).