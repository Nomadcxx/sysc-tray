// Package presenter serves tray state to exactly one authenticated shell over
// a private Unix socket.
package presenter

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Nomadcxx/sysc-tray/internal/state"
	"github.com/Nomadcxx/sysc-tray/protocol"
)

const (
	socketName         = "presenter.v1.sock"
	runtimeSubdir      = "sysc-tray"
	RequiredCapability = protocol.CapabilityTray
	handshakeTimeout   = 5 * time.Second
)

// Commands routes a presenter command to the package that owns its D-Bus call.
type Commands interface {
	Activate(key protocol.ItemKey, x, y int32) error
	SecondaryActivate(key protocol.ItemKey, x, y int32) error
	Scroll(key protocol.ItemKey, delta int32, orientation protocol.ScrollOrientation) error
	MenuOpen(key protocol.ItemKey, revision uint32, id int32) error
	MenuSelect(key protocol.ItemKey, revision uint32, id int32, timestamp uint32) error
	AboutToShow(key protocol.ItemKey, revision uint32, id int32) (bool, error)
	MenuClose(key protocol.ItemKey, revision uint32, id int32) error
	Terminate(key protocol.ItemKey) error
}

type Server struct {
	runtimeDir string
	uid        uint32
	peerUID    atomic.Uint32

	mu         sync.Mutex
	owner      *state.Owner
	commands   Commands
	listener   *net.UnixListener
	socketPath string
	current    *connection
	nextGen    uint64
	started    bool

	stop      chan struct{}
	done      chan error
	closeOnce sync.Once
	wg        sync.WaitGroup
}

func NewAt(runtimeDir string) *Server {
	uid := uint32(os.Geteuid())
	server := &Server{runtimeDir: runtimeDir, uid: uid, stop: make(chan struct{}), done: make(chan error, 1)}
	server.peerUID.Store(uid)
	return server
}

func New() *Server { return NewAt(os.Getenv("XDG_RUNTIME_DIR")) }

func (s *Server) Serve(owner *state.Owner, commands Commands) error {
	if s == nil || owner == nil {
		return errors.New("presenter: missing server dependency")
	}
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("presenter: server already started")
	}
	s.started = true
	s.owner = owner
	s.commands = commands
	s.mu.Unlock()

	listener, path, err := listenRuntime(s.runtimeDir, s.uid)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.listener, s.socketPath = listener, path
	s.mu.Unlock()
	s.wg.Add(1)
	go s.acceptLoop()
	return nil
}

func (s *Server) SocketPath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.socketPath
}

func (s *Server) Done() <-chan error { return s.done }

func (s *Server) Close() error {
	if s == nil {
		return nil
	}
	var result error
	s.closeOnce.Do(func() {
		close(s.stop)
		s.mu.Lock()
		listener, current, path := s.listener, s.current, s.socketPath
		s.mu.Unlock()
		if listener != nil {
			result = errors.Join(result, listener.Close())
		}
		if current != nil {
			current.fail()
		}
		s.wg.Wait()
		if path != "" {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				result = errors.Join(result, err)
			}
		}
		close(s.done)
	})
	return result
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		socket, err := s.listener.AcceptUnix()
		if err != nil {
			select {
			case <-s.stop:
			default:
				select {
				case s.done <- fmt.Errorf("presenter: accept: %w", err):
				default:
				}
			}
			return
		}
		s.wg.Add(1)
		go s.handle(socket)
	}
}

func (s *Server) handle(socket *net.UnixConn) {
	defer s.wg.Done()
	c := newConnection(socket, 0)
	writerStarted := false
	defer func() {
		c.fail()
		if writerStarted {
			<-c.writerDone
		}
		s.connectionDone(c)
	}()

	// The peer is checked before a single byte of its hello is read.
	uid, err := peerUID(socket)
	if err != nil || uid != s.peerUID.Load() {
		return
	}
	if err := socket.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return
	}
	hello, err := readHello(socket)
	if err != nil || !hasCapability(hello.Capabilities, RequiredCapability) {
		return
	}
	if err := socket.SetDeadline(time.Time{}); err != nil {
		return
	}

	s.mu.Lock()
	s.nextGen++
	c.generation = s.nextGen
	owner, commands := s.owner, s.commands
	s.mu.Unlock()

	// Attaching installs the sink and captures the baseline in one step on the
	// state goroutine, so no delta can slip between the snapshot and the stream.
	snapshot, err := owner.Attach(c)
	if err != nil || snapshot.Validate() != nil {
		return
	}
	serviceHello := protocol.Hello{
		Major: protocol.ProtocolMajor, Minor: protocol.ProtocolMinor, Role: protocol.RolePresenter,
		Capabilities: []string{RequiredCapability, "menus", "commands"},
	}
	helloFrame, err := marshalEnvelope(protocol.Envelope{Kind: protocol.KindHello}, serviceHello)
	if err != nil {
		owner.Detach(c)
		return
	}
	snapshotFrame, err := marshalEnvelope(
		protocol.Envelope{Kind: protocol.KindSnapshot, Sequence: snapshot.Sequence}, snapshot)
	if err != nil {
		owner.Detach(c)
		return
	}
	if err := c.start(snapshot.Sequence, helloFrame, snapshotFrame); err != nil {
		owner.Detach(c)
		return
	}

	// A newer valid presenter replaces the previous generation.
	s.mu.Lock()
	old := s.current
	s.current = c
	s.mu.Unlock()
	if old != nil && old != c {
		old.fail()
	}

	writerStarted = true
	go c.writeLoop()
	c.readLoop(commands)
}

func (s *Server) connectionDone(c *connection) {
	s.mu.Lock()
	owner := s.owner
	if s.current == c {
		s.current = nil
	}
	s.mu.Unlock()
	if owner != nil {
		owner.Detach(c)
	}
}

func marshalEnvelope(envelope protocol.Envelope, payload any) ([]byte, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	envelope.Payload = encoded
	return json.Marshal(envelope)
}

func listenRuntime(runtimeDir string, uid uint32) (*net.UnixListener, string, error) {
	if runtimeDir == "" {
		return nil, "", errors.New("presenter: XDG_RUNTIME_DIR is empty")
	}
	abs, err := filepath.Abs(runtimeDir)
	if err != nil {
		return nil, "", err
	}
	if err := rejectSymlinkComponents(abs); err != nil {
		return nil, "", err
	}
	if err := validateDirectory(abs, uid, 0o700); err != nil {
		return nil, "", err
	}
	dir := filepath.Join(abs, runtimeSubdir)
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, "", fmt.Errorf("presenter: create runtime directory: %w", err)
	}
	if err := validateDirectory(dir, uid, 0o700); err != nil {
		return nil, "", err
	}
	path := filepath.Join(dir, socketName)
	if err := removeStaleSocket(path, uid); err != nil {
		return nil, "", err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, "", fmt.Errorf("presenter: listen: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, "", fmt.Errorf("presenter: secure socket: %w", err)
	}
	if err := validateSocket(path, uid); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, "", err
	}
	return listener, path, nil
}

func validateDirectory(path string, uid uint32, mode os.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("presenter: inspect directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != mode || fileUID(info) != uid {
		return fmt.Errorf("presenter: unsafe runtime directory %q", path)
	}
	return nil
}

func validateSocket(path string, uid uint32) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 ||
		info.Mode().Perm() != 0o600 || fileUID(info) != uid {
		return fmt.Errorf("presenter: unsafe socket %q", path)
	}
	return nil
}

func removeStaleSocket(path string, uid uint32) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 ||
		info.Mode().Perm() != 0o600 || fileUID(info) != uid {
		return fmt.Errorf("presenter: unsafe existing socket %q", path)
	}
	if conn, err := net.DialTimeout("unix", path, 50*time.Millisecond); err == nil {
		_ = conn.Close()
		return errors.New("presenter: socket already active")
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("presenter: remove stale socket: %w", err)
	}
	return nil
}

func rejectSymlinkComponents(path string) error {
	current := string(filepath.Separator)
	relative := strings.TrimPrefix(filepath.Clean(path), current)
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("presenter: symlink path component %q", current)
		}
	}
	return nil
}

func fileUID(info os.FileInfo) uint32 { return info.Sys().(*syscall.Stat_t).Uid }

func peerUID(conn *net.UnixConn) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var credential *unix.Ucred
	var controlErr error
	if err := raw.Control(func(fd uintptr) {
		credential, controlErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if controlErr != nil {
		return 0, controlErr
	}
	return credential.Uid, nil
}

func readHello(conn net.Conn) (protocol.Hello, error) {
	frame, err := protocol.ReadFrame(conn)
	if err != nil {
		return protocol.Hello{}, err
	}
	var envelope protocol.Envelope
	if err := protocol.DecodeStrict(frame, &envelope); err != nil {
		return protocol.Hello{}, err
	}
	if err := envelope.Validate(); err != nil || envelope.Kind != protocol.KindHello {
		return protocol.Hello{}, errors.New("presenter: expected hello")
	}
	var hello protocol.Hello
	if err := protocol.DecodeStrict(envelope.Payload, &hello); err != nil {
		return protocol.Hello{}, err
	}
	if err := hello.Validate(protocol.RolePresenter); err != nil {
		return protocol.Hello{}, err
	}
	return hello, nil
}

func hasCapability(capabilities []string, required string) bool {
	for _, capability := range capabilities {
		if capability == required {
			return true
		}
	}
	return false
}
