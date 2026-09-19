// Package app wires the tray service into one cancellable process: state, the
// session bus, watcher policy, per-item workers, and the presenter socket.
package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/Nomadcxx/sysc-tray/internal/item"
	"github.com/Nomadcxx/sysc-tray/internal/menu"
	"github.com/Nomadcxx/sysc-tray/internal/ownership"
	"github.com/Nomadcxx/sysc-tray/internal/presenter"
	"github.com/Nomadcxx/sysc-tray/internal/state"
	"github.com/Nomadcxx/sysc-tray/internal/watcher"
	"github.com/Nomadcxx/sysc-tray/protocol"
)

// worker holds the D-Bus readers for one item generation. The menu reader is
// created only once the item advertises a menu path, and a menu failure leaves
// the item's own commands working. The ownership record is the process
// identity behind the item's bus owner; a nil record means close is not
// offered for this generation.
type worker struct {
	key      protocol.ItemKey
	item     *item.Proxy
	menu     *menu.Proxy
	menuPath string

	ownMu    sync.Mutex
	own      ownership.Record
	ownKnown bool
}

// closeable reports whether the recorded identity is a same-UID process the
// service may terminate gracefully.
func (w *worker) closeable() bool {
	w.ownMu.Lock()
	defer w.ownMu.Unlock()
	return w.ownKnown && ownership.SameUser(w.own)
}

// reown re-inspects the item owner's identity and verifies it against the
// recorded one. It returns the recorded record for signalling.
func (w *worker) reown(conn *dbus.Conn) (ownership.Record, error) {
	w.ownMu.Lock()
	recorded, known := w.own, w.ownKnown
	w.ownMu.Unlock()
	if !known {
		return ownership.Record{}, ownership.ErrUnavailable
	}
	current, err := ownership.Inspect(conn, w.key.Owner)
	if err != nil {
		return ownership.Record{}, err
	}
	if err := ownership.Verify(recorded, current); err != nil {
		return ownership.Record{}, err
	}
	return recorded, nil
}

type App struct {
	runtimeDir string

	owner     *state.Owner
	conn      *dbus.Conn
	watcher   *watcher.Watcher
	presenter *presenter.Server
	limiter   *item.Limiter

	ctx        context.Context
	cancel     context.CancelFunc
	watcherEnd chan struct{}

	mu      sync.Mutex
	workers map[protocol.ItemKey]*worker
	closing bool
}

func New(runtimeDir string) *App {
	return &App{runtimeDir: runtimeDir, workers: make(map[protocol.ItemKey]*worker)}
}

// Start brings the service up in dependency order. A failure at any step
// unwinds the steps already taken, in reverse, before returning.
func (a *App) Start(ctx context.Context) error {
	a.mu.Lock()
	started := a.owner != nil
	a.mu.Unlock()
	if started {
		return errors.New("app: already started")
	}

	owner := state.Start()
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		_ = owner.Close()
		return fmt.Errorf("app: connect session bus: %w", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	relationship := watcher.New(conn, a)
	ended := make(chan struct{})

	a.mu.Lock()
	a.owner, a.conn, a.limiter = owner, conn, item.NewLimiter(time.Now)
	a.ctx, a.cancel = runCtx, cancel
	a.watcher, a.watcherEnd = relationship, ended
	a.closing = false
	a.mu.Unlock()

	go func() {
		defer close(ended)
		_ = relationship.Run(runCtx)
	}()

	server := presenter.NewAt(a.runtimeDir)
	if err := server.Serve(owner, a); err != nil {
		_ = a.unwind()
		return err
	}
	a.mu.Lock()
	a.presenter = server
	a.mu.Unlock()
	return nil
}

// Run starts the service and blocks until the context is cancelled or the
// presenter socket fails, then shuts down.
func (a *App) Run(ctx context.Context) error {
	if err := a.Start(ctx); err != nil {
		return err
	}
	a.mu.Lock()
	failures := a.presenter.Done()
	a.mu.Unlock()

	var failure error
	select {
	case <-ctx.Done():
	case err := <-failures:
		failure = err
	}
	return errors.Join(failure, a.Close())
}

func (a *App) SocketPath() string {
	a.mu.Lock()
	server := a.presenter
	a.mu.Unlock()
	if server == nil {
		return ""
	}
	return server.SocketPath()
}

// Close shuts the service down in reverse order: stop accepting presenters,
// stop the per-item D-Bus readers, release the watcher relationship, close the
// bus, then stop state. The presenter server removes its socket as it closes.
func (a *App) Close() error {
	a.mu.Lock()
	if a.closing || a.owner == nil {
		a.mu.Unlock()
		return nil
	}
	a.closing = true
	a.mu.Unlock()
	return a.unwind()
}

// unwind releases whatever has been built, newest first. It takes ownership of
// each field under the lock and does the blocking work outside it.
func (a *App) unwind() error {
	a.mu.Lock()
	server, conn, owner := a.presenter, a.conn, a.owner
	cancel, ended := a.cancel, a.watcherEnd
	a.presenter, a.conn, a.owner = nil, nil, nil
	a.cancel, a.watcherEnd, a.watcher = nil, nil, nil
	a.closing = true
	a.mu.Unlock()

	var result error
	if server != nil {
		result = errors.Join(result, server.Close())
	}
	a.stopWorkers()
	if cancel != nil {
		cancel()
	}
	if ended != nil {
		<-ended
	}
	if conn != nil {
		result = errors.Join(result, conn.Close())
	}
	if owner != nil {
		result = errors.Join(result, owner.Close())
	}
	return result
}

func (a *App) stopWorkers() {
	a.mu.Lock()
	workers := make([]*worker, 0, len(a.workers))
	for key, w := range a.workers {
		workers = append(workers, w)
		delete(a.workers, key)
	}
	a.mu.Unlock()
	for _, w := range workers {
		closeWorker(w)
	}
}

func closeWorker(w *worker) {
	if w.menu != nil {
		w.menu.Close()
	}
	if w.item != nil {
		w.item.Close()
	}
}

// ItemAdded starts reading a newly registered generation.
func (a *App) ItemAdded(key watcher.Key) {
	itemKey := protocol.ItemKey{
		Owner: key.Owner, ObjectPath: string(key.ObjectPath), Generation: key.Generation,
	}
	a.mu.Lock()
	conn, owner, limiter, ctx, closing := a.conn, a.owner, a.limiter, a.ctx, a.closing
	a.mu.Unlock()
	if closing || conn == nil || owner == nil {
		return
	}
	owner.Add(itemKey)

	proxy := item.NewProxy(conn, itemKey, limiter, a.onItem)
	a.mu.Lock()
	if a.closing {
		a.mu.Unlock()
		return
	}
	w := &worker{key: itemKey, item: proxy}
	a.workers[itemKey] = w
	a.mu.Unlock()

	// Establish the process identity off the registration path: a slow or
	// failed inspection only decides whether Close is offered, never whether
	// the item itself is usable.
	go a.inspectOwnership(conn, w)

	if err := proxy.Start(ctx); err != nil {
		a.dropWorker(itemKey)
	}
}

// inspectOwnership records the item owner's process identity once. The result
// is stored on the worker; a failure leaves the record unknown and Close
// unsupported for this generation.
func (a *App) inspectOwnership(conn *dbus.Conn, w *worker) {
	record, err := ownership.Inspect(conn, w.key.Owner)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.workers[w.key] != w || a.closing {
		return
	}
	w.ownMu.Lock()
	w.own, w.ownKnown = record, err == nil
	w.ownMu.Unlock()
	// The first publish may have raced this inspection; refresh once so the
	// published item carries the CloseSupported capability deterministically.
	if err == nil {
		w.item.Refresh()
	}
}

// ItemRemoved stops reading a generation and retires it from state.
func (a *App) ItemRemoved(key watcher.Key) {
	a.dropWorker(protocol.ItemKey{
		Owner: key.Owner, ObjectPath: string(key.ObjectPath), Generation: key.Generation,
	})
}

func (a *App) dropWorker(key protocol.ItemKey) {
	a.mu.Lock()
	w, ok := a.workers[key]
	delete(a.workers, key)
	owner := a.owner
	a.mu.Unlock()
	if ok {
		closeWorker(w)
	}
	if owner != nil {
		owner.Remove(key)
	}
}

// onItem accepts a refresh. A failed read drops only that item; a menu path
// that appears or changes rebinds just the menu reader.
func (a *App) onItem(result item.Result) {
	if result.Err != nil {
		// The pump delivers this from its own goroutine and then exits.
		// Dropping the worker here would deadlock in Proxy.Close waiting
		// for that same pump to finish (sysc-458).
		go a.dropWorker(result.Key)
		return
	}
	a.mu.Lock()
	owner := a.owner
	w := a.workers[result.Key]
	a.mu.Unlock()
	if owner == nil {
		return
	}
	if w != nil && w.closeable() {
		result.Item.CloseSupported = true
	}
	owner.Update(result.Key, result.Item)
	a.bindMenu(result.Key, result.Item.MenuPath)
}

func (a *App) bindMenu(key protocol.ItemKey, path string) {
	a.mu.Lock()
	w, ok := a.workers[key]
	if !ok || a.closing || w.menuPath == path {
		a.mu.Unlock()
		return
	}
	previous := w.menu
	w.menu, w.menuPath = nil, path
	conn, ctx := a.conn, a.ctx
	a.mu.Unlock()

	// A changed menu path cancels the calls in flight against the old one.
	if previous != nil {
		previous.Close()
	}
	if path == "" || conn == nil {
		return
	}
	proxy := menu.NewProxy(conn, key, path, a.onMenu)
	if err := proxy.Start(ctx); err != nil {
		return
	}
	a.mu.Lock()
	current, ok := a.workers[key]
	if !ok || a.closing || current.menuPath != path {
		a.mu.Unlock()
		proxy.Close()
		return
	}
	current.menu = proxy
	a.mu.Unlock()
}

// onMenu publishes a menu tree. A menu failure is not fatal to the item:
// activate, secondary activate, and scroll keep working without it.
func (a *App) onMenu(update menu.Update) {
	if update.Err != nil {
		return
	}
	a.mu.Lock()
	owner := a.owner
	a.mu.Unlock()
	if owner != nil {
		owner.UpdateMenu(update.Key, update.Menu)
	}
}

func (a *App) itemProxy(key protocol.ItemKey) (*item.Proxy, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	w, ok := a.workers[key]
	if !ok || w.item == nil {
		return nil, &protocol.ProtocolError{Code: protocol.ErrorStaleItem, Message: "no such item generation"}
	}
	return w.item, nil
}

func (a *App) menuProxy(key protocol.ItemKey) (*menu.Proxy, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	w, ok := a.workers[key]
	if !ok {
		return nil, &protocol.ProtocolError{Code: protocol.ErrorStaleItem, Message: "no such item generation"}
	}
	if w.menu == nil {
		return nil, &protocol.ProtocolError{Code: protocol.ErrorUnavailable, Message: "item has no menu"}
	}
	return w.menu, nil
}

func (a *App) Activate(key protocol.ItemKey, x, y int32) error {
	proxy, err := a.itemProxy(key)
	if err != nil {
		return err
	}
	return proxy.Activate(key, x, y)
}

func (a *App) SecondaryActivate(key protocol.ItemKey, x, y int32) error {
	proxy, err := a.itemProxy(key)
	if err != nil {
		return err
	}
	return proxy.SecondaryActivate(key, x, y)
}

func (a *App) Scroll(key protocol.ItemKey, delta int32, orientation protocol.ScrollOrientation) error {
	proxy, err := a.itemProxy(key)
	if err != nil {
		return err
	}
	return proxy.Scroll(key, delta, orientation)
}

func (a *App) MenuOpen(key protocol.ItemKey, revision uint32, id int32) error {
	proxy, err := a.menuProxy(key)
	if err != nil {
		return err
	}
	return proxy.Opened(key, revision, id)
}

func (a *App) MenuSelect(key protocol.ItemKey, revision uint32, id int32, timestamp uint32) error {
	proxy, err := a.menuProxy(key)
	if err != nil {
		return err
	}
	return proxy.Select(key, revision, id, timestamp)
}

func (a *App) AboutToShow(key protocol.ItemKey, revision uint32, id int32) (bool, error) {
	proxy, err := a.menuProxy(key)
	if err != nil {
		return false, err
	}
	return proxy.AboutToShow(key, revision, id)
}

func (a *App) MenuClose(key protocol.ItemKey, revision uint32, id int32) error {
	proxy, err := a.menuProxy(key)
	if err != nil {
		return err
	}
	return proxy.Closed(key, revision, id)
}

// TerminateWait bounds how long Terminate waits for a signalled process to
// leave. ponytail: one flat window; a per-application grace policy would need
// a measured requirement first.
var TerminateWait = 3 * time.Second

// TerminatePoll is the /proc recheck interval inside the bounded wait.
var TerminatePoll = 100 * time.Millisecond

// Terminate gracefully ends the process behind one owned tray item. Every
// identity check runs at operation time: a stale generation, a replaced
// owner, a recycled PID, or a cross-UID process is rejected before any
// signal. The bounded wait returns busy when the process ignores the
// request; the item then stays visible and nothing escalates.
func (a *App) Terminate(key protocol.ItemKey) error {
	a.mu.Lock()
	conn, closing := a.conn, a.closing
	w := a.workers[key]
	a.mu.Unlock()
	if closing || conn == nil {
		return &protocol.ProtocolError{Code: protocol.ErrorUnavailable, Message: "service is closing"}
	}
	if w == nil {
		return &protocol.ProtocolError{Code: protocol.ErrorStaleItem, Message: "no such item generation"}
	}
	record, err := w.reown(conn)
	if err != nil {
		return &protocol.ProtocolError{Code: protocol.ErrorInvalid, Message: err.Error()}
	}
	if !ownership.SameUser(record) {
		return &protocol.ProtocolError{Code: protocol.ErrorInvalid, Message: "item owner is not the service user"}
	}
	if err := ownership.Signal(record); err != nil {
		return &protocol.ProtocolError{Code: protocol.ErrorUnavailable, Message: "termination request failed"}
	}
	// The dispatch runs on the connection goroutine, so this wait delays the
	// shell's next commands by at most TerminateWait. Acceptable for a rare
	// Close action; an async reply would need a reply-queue redesign.
	deadline := time.Now().Add(TerminateWait)
	for {
		if !ownership.Alive(record) {
			return nil
		}
		if time.Now().After(deadline) {
			return &protocol.ProtocolError{Code: protocol.ErrorBusy, Message: "process did not exit in time"}
		}
		time.Sleep(TerminatePoll)
	}
}
