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
	"github.com/Nomadcxx/sysc-tray/internal/presenter"
	"github.com/Nomadcxx/sysc-tray/internal/state"
	"github.com/Nomadcxx/sysc-tray/internal/watcher"
	"github.com/Nomadcxx/sysc-tray/protocol"
)

// worker holds the D-Bus readers for one item generation. The menu reader is
// created only once the item advertises a menu path, and a menu failure leaves
// the item's own commands working.
type worker struct {
	key      protocol.ItemKey
	item     *item.Proxy
	menu     *menu.Proxy
	menuPath string
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
	a.workers[itemKey] = &worker{key: itemKey, item: proxy}
	a.mu.Unlock()

	if err := proxy.Start(ctx); err != nil {
		a.dropWorker(itemKey)
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
		a.dropWorker(result.Key)
		return
	}
	a.mu.Lock()
	owner := a.owner
	a.mu.Unlock()
	if owner == nil {
		return
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
