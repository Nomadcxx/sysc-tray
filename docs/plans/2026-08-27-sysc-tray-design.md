# sysc-tray Design

Date: 2026-08-27
Status: Approved foundation boundary; protocol details require the M0 gate

## Purpose

`sysc-tray` owns Linux StatusNotifierItem and DBusMenu behavior for the sysc desktop. Keeping the D-Bus
host outside `sysc-shell` lets tray items remain registered while the shell recreates bars or restarts.

## Service boundary

`sysc-tray` owns watcher acquisition or attachment, host registration, item proxies, item state,
property-change signals, item method calls, menu trees, and menu events.

`sysc-shell` owns Wayland surfaces, bar layout, icon-theme resolution, pixels, pointer and keyboard input,
menu placement, and theme. The tray service sends immutable item and menu state; the shell sends user
intent such as activate, secondary activate, scroll, open menu, and select menu item.

The first implementation has one presentation client. It must tolerate no client and reconnect one shell
from a current snapshot. General multi-frontend synchronization is outside scope.

## D-Bus behavior

Support the StatusNotifierWatcher, StatusNotifierHost, StatusNotifierItem, and DBusMenu contracts used by
current Linux applications. M0 will pin the exact service-name variants, watcher takeover policy, host
identity, item-address forms, and recovery behavior against real applications and focused prior art.

Subscribe to every state-changing item signal required for title, status, normal and attention icons,
overlay icon, tooltip, and menu changes. Property refreshes must coalesce signal bursts and discard stale
replies after an item owner changes.

DBusMenu layout revision numbers order tree updates. The service keeps renderer-neutral properties and
does not encode shell widget nodes in the D-Bus package.

## Trust boundaries

Bound item count, menu depth, menu node count, strings, property maps, pixmap dimensions, decoded byte
count, and update rate. Validate pixmap dimensions with overflow-safe arithmetic. A malformed item or menu
must not remove healthy items.

Track each item by its unique bus owner plus object path, not by a reusable well-known name alone. Drop
in-flight replies and cached menus when the unique owner disappears.

## Shell IPC

Use a private Unix socket with peer-credential checks and a versioned length-bounded protocol. Send a full
item snapshot after handshake, then ordered changes. Menus may be requested on demand to avoid retaining
unused trees for every item.

Disconnect a slow or malformed shell. Reconnection rebuilds presentation from service state. D-Bus item
registration and updates continue while no shell is connected.

## Failure and recovery

The service will either own the watcher name or attach as a host to the current watcher according to the
M0 policy. It must not fight a healthy owner in a name-acquisition loop. Bus reconnection rebuilds the
watcher, host, item, and signal state in order.

Item disappearance removes only that item. A DBusMenu failure leaves the icon usable and reports the menu
as unavailable. Shell loss preserves item state.

## Prior art

Use `nekorg/pawbar` as the BSD-3 source reference for watcher/host behavior and DMS tray recovery as a
behavioral reference. Extract only code that fits this ownership boundary, preserve attribution, complete
missing item signals, and add protocol-level tests before adapting it.

## Proof

Private-bus tests simulate watcher ownership, item registration, owner replacement, signal bursts,
malformed pixmaps, menu revisions, and shell reconnect. Race tests cover concurrent D-Bus replies and
owner loss. A live Niri gate uses shell-owned bar and menu surfaces with representative applications.
