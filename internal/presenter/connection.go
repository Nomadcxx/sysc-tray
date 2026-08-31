package presenter

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/Nomadcxx/sysc-tray/protocol"
)

type outbound struct {
	data     []byte
	sequence uint64
}

// connection is one presenter socket. A single writer owns the frames, so the
// shell sees hello, then the snapshot, then contiguous deltas, in that order.
type connection struct {
	socket     *net.UnixConn
	generation uint64

	mu             sync.Mutex
	queue          chan outbound
	pending        []outbound
	queuedMessages int
	queuedBytes    int
	lastSequence   uint64
	lastRequestID  uint64
	started        bool
	failed         bool

	closed     chan struct{}
	writerDone chan struct{}
	failOnce   sync.Once
}

func newConnection(socket *net.UnixConn, generation uint64) *connection {
	return &connection{
		socket: socket, generation: generation,
		queue:  make(chan outbound, protocol.MaxPresenterQueueMessages),
		closed: make(chan struct{}), writerDone: make(chan struct{}),
	}
}

// Send implements the state sink. It never blocks: a presenter that cannot keep
// up overflows its bound and is dropped, leaving D-Bus unaffected.
func (c *connection) Send(envelope protocol.Envelope) error {
	frame, err := json.Marshal(envelope)
	if err != nil {
		c.fail()
		return err
	}
	message := outbound{data: frame, sequence: envelope.Sequence}
	c.mu.Lock()
	if c.failed {
		c.mu.Unlock()
		return errors.New("presenter: connection failed")
	}
	if !c.reserve(message) {
		c.mu.Unlock()
		c.fail()
		return errors.New("presenter: queue overflow")
	}
	if !c.started {
		c.pending = append(c.pending, message)
		c.mu.Unlock()
		return nil
	}
	if err := c.followsLocked(message.sequence); err != nil {
		c.release(message)
		c.mu.Unlock()
		c.fail()
		return err
	}
	select {
	case c.queue <- message:
		c.lastSequence = message.sequence
		c.mu.Unlock()
		return nil
	default:
		c.release(message)
		c.mu.Unlock()
		c.fail()
		return errors.New("presenter: queue overflow")
	}
}

// start flushes the handshake ahead of anything the state owner queued while
// the snapshot was being written, then hands the queue to the writer.
func (c *connection) start(baseline uint64, prologue ...[]byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failed {
		return errors.New("presenter: connection failed")
	}
	c.lastSequence = baseline
	for _, frame := range prologue {
		message := outbound{data: frame}
		if !c.reserve(message) {
			return errors.New("presenter: queue overflow")
		}
		c.queue <- message
	}
	for _, message := range c.pending {
		if message.sequence <= baseline {
			c.release(message)
			continue
		}
		if err := c.followsLocked(message.sequence); err != nil {
			return err
		}
		c.lastSequence = message.sequence
		select {
		case c.queue <- message:
		default:
			return errors.New("presenter: queue overflow")
		}
	}
	c.pending = nil
	c.started = true
	return nil
}

func (c *connection) followsLocked(sequence uint64) error {
	if sequence == 0 {
		return nil
	}
	return protocol.ValidateNextSequence(c.lastSequence, sequence)
}

// reserve and release keep the queue inside both its message and byte bounds.
func (c *connection) reserve(message outbound) bool {
	if c.queuedMessages+1 > protocol.MaxPresenterQueueMessages {
		return false
	}
	if c.queuedBytes+len(message.data) > protocol.MaxPresenterDecodedBytes {
		return false
	}
	c.queuedMessages++
	c.queuedBytes += len(message.data)
	return true
}

func (c *connection) release(message outbound) {
	c.queuedMessages--
	c.queuedBytes -= len(message.data)
}

func (c *connection) fail() {
	c.failOnce.Do(func() {
		c.mu.Lock()
		c.failed = true
		c.mu.Unlock()
		close(c.closed)
		_ = c.socket.Close()
	})
}

func (c *connection) writeLoop() {
	defer close(c.writerDone)
	for {
		select {
		case <-c.closed:
			return
		case message := <-c.queue:
			if err := protocol.WriteFrame(c.socket, message.data); err != nil {
				c.fail()
				return
			}
			c.mu.Lock()
			c.release(message)
			c.mu.Unlock()
		}
	}
}

// readLoop validates and dispatches commands. Request IDs must increase within
// a connection generation: a duplicate or stale ID means the shell is replaying,
// and a replayed activation could fire twice, so the connection is closed.
func (c *connection) readLoop(commands Commands) {
	defer c.fail()
	for {
		frame, err := protocol.ReadFrame(c.socket)
		if err != nil {
			return
		}
		var envelope protocol.Envelope
		if err := protocol.DecodeStrict(frame, &envelope); err != nil {
			return
		}
		if err := envelope.Validate(); err != nil || envelope.Kind != protocol.KindCommand {
			return
		}
		c.mu.Lock()
		if envelope.RequestID <= c.lastRequestID {
			c.mu.Unlock()
			return
		}
		c.lastRequestID = envelope.RequestID
		c.mu.Unlock()

		var command protocol.Command
		if err := protocol.DecodeStrict(envelope.Payload, &command); err != nil {
			return
		}
		reply := dispatch(commands, command)
		if err := c.reply(envelope.RequestID, reply); err != nil {
			return
		}
	}
}

func (c *connection) reply(requestID uint64, reply protocol.Reply) error {
	payload, err := json.Marshal(reply)
	if err != nil {
		return err
	}
	frame, err := json.Marshal(protocol.Envelope{
		Kind: protocol.KindReply, RequestID: requestID, Payload: payload,
	})
	if err != nil {
		return err
	}
	message := outbound{data: frame}
	c.mu.Lock()
	if c.failed || !c.reserve(message) {
		c.mu.Unlock()
		return errors.New("presenter: cannot queue reply")
	}
	select {
	case c.queue <- message:
		c.mu.Unlock()
		return nil
	default:
		c.release(message)
		c.mu.Unlock()
		return errors.New("presenter: queue overflow")
	}
}

// dispatch validates the command, then routes it to the package that owns the
// D-Bus call. That package rechecks the item generation and menu revision
// immediately before invoking, so this layer never assumes freshness.
func dispatch(commands Commands, command protocol.Command) protocol.Reply {
	if err := command.Validate(); err != nil {
		return failure(command, protocol.ErrorInvalid, err.Error())
	}
	if commands == nil {
		return failure(command, protocol.ErrorUnavailable, "no command handler")
	}
	var err error
	switch command.Kind {
	case protocol.CommandActivate:
		err = commands.Activate(command.Item, command.X, command.Y)
	case protocol.CommandSecondaryActivate:
		err = commands.SecondaryActivate(command.Item, command.X, command.Y)
	case protocol.CommandScroll:
		err = commands.Scroll(command.Item, command.Delta, command.Orientation)
	case protocol.CommandMenuOpen:
		err = commands.MenuOpen(command.Item, command.MenuRevision, command.MenuID)
	case protocol.CommandMenuSelect:
		err = commands.MenuSelect(command.Item, command.MenuRevision, command.MenuID, command.Serial)
	case protocol.CommandAboutToShow:
		_, err = commands.AboutToShow(command.Item, command.MenuRevision, command.MenuID)
	case protocol.CommandMenuClose:
		err = commands.MenuClose(command.Item, command.MenuRevision, command.MenuID)
	default:
		return failure(command, protocol.ErrorInvalid, fmt.Sprintf("unknown command %q", command.Kind))
	}
	if err != nil {
		var typed *protocol.ProtocolError
		if errors.As(err, &typed) {
			return protocol.Reply{Item: command.Item, Output: command.Output, Error: typed}
		}
		return failure(command, protocol.ErrorUnavailable, err.Error())
	}
	return protocol.Reply{OK: true, Item: command.Item, Output: command.Output}
}

func failure(command protocol.Command, code protocol.ErrorCode, message string) protocol.Reply {
	return protocol.Reply{
		Item: command.Item, Output: command.Output,
		Error: &protocol.ProtocolError{Code: code, Message: message},
	}
}
