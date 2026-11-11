package p2p

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Timeouts (peer protocol "Limits").
const (
	DialTimeout   = 5 * time.Second
	FrameTimeout  = 5 * time.Second
	AnswerTimeout = 30 * time.Second
	IdleLimit     = 120 * time.Second
	KeepAlive     = 30 * time.Second
	MaxPipeline   = 64
)

// ErrSelf reports a connection to this node itself.
var ErrSelf = errors.New("connected to itself")

// ErrClosed reports a closed connection.
var ErrClosed = errors.New("connection closed")

// Result is an answer, or the error answer or failure in its place.
type Result struct {
	msg Message
	err error
}

// waiter is a request in flight: where its answer goes, and the most bytes
// the answer's frame may have (0: any frame).
type waiter struct {
	ch   chan Result
	most int
}

// ReadAhead is the most bytes of block slices a connection asks for ahead
// of handling them (the reference node reads 8 MiB of frames ahead): what a
// peer that answers faster than blocks are checked can make the node hold.
const ReadAhead = 8 << 20

// sliceFraming is more than a slice answer's bytes besides its payload.
const sliceFraming = 64

// Conn is a connection this node opened: it asks, the peer answers, in
// order. Many requests may be in flight; answers are matched to them in
// the order they were sent.
type Conn struct {
	nc      net.Conn
	wmu     sync.Mutex
	waiters chan waiter
	// Announcements receives the peer's announcements when the hello asked
	// for them. Slow readers miss some; only the latest matters.
	Announcements chan *Announcement
	// Peer is the state the peer answered the hello with.
	Peer *State

	lastReq atomic.Int64
	done    chan struct{}
	once    sync.Once
	errMu   sync.Mutex
	err     error
}

// Dial connects, says hello and starts reading.
func Dial(ctx context.Context, addr string, hello *Hello) (*Conn, error) {
	d := net.Dialer{Timeout: DialTimeout}
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	c := &Conn{nc: nc, waiters: make(chan waiter, MaxPipeline),
		Announcements: make(chan *Announcement, 16), done: make(chan struct{})}
	// The hello's answer is due within one frame deadline, as the
	// reference node asks of its peers.
	nc.SetDeadline(time.Now().Add(FrameTimeout))
	if err := WriteFrame(nc, Encode(hello)); err != nil {
		nc.Close()
		return nil, err
	}
	for {
		p, err := ReadFrame(nc, MaxFrame)
		if err != nil {
			nc.Close()
			return nil, fmt.Errorf("reading hello answer: %w", err)
		}
		if len(p) > 0 && p[0] == KindAnnouncement {
			continue
		}
		m, err := DecodeAnswer(p)
		if err != nil {
			nc.Close()
			var ea *ErrorAnswer
			if errors.As(err, &ea) && ea.Text == "connected to itself" {
				return nil, ErrSelf
			}
			return nil, err
		}
		st, ok := m.(*State)
		if !ok {
			nc.Close()
			return nil, fmt.Errorf("%w: hello answered with tag %d", ErrNotMessage, m.tag())
		}
		if st.Version < 1 || st.ChainID != hello.ChainID {
			nc.Close()
			return nil, errors.New("peer version or genesis mismatch")
		}
		if st.Nonce == hello.Nonce {
			nc.Close()
			return nil, ErrSelf
		}
		c.Peer = st
		break
	}
	nc.SetDeadline(time.Time{})
	c.lastReq.Store(time.Now().UnixNano())
	go c.readLoop()
	go c.keepAlive()
	return c, nil
}

// Close ends the connection.
func (c *Conn) Close() error {
	c.fail(ErrClosed)
	return nil
}

// Done is closed when the connection ends.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err returns why the connection ended.
func (c *Conn) Err() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return c.err
}

// RemoteAddr returns the peer's address.
func (c *Conn) RemoteAddr() net.Addr { return c.nc.RemoteAddr() }

func (c *Conn) fail(err error) {
	c.once.Do(func() {
		c.errMu.Lock()
		c.err = err
		c.errMu.Unlock()
		close(c.done)
		c.nc.Close()
	})
}

func (c *Conn) readLoop() {
	// A fault in handling one peer's frames ends that connection, not the
	// node.
	defer func() {
		if v := recover(); v != nil {
			c.fail(fmt.Errorf("reading from the peer panicked: %v", v))
		}
	}()
	for {
		p, err := ReadFrame(c.nc, MaxFrame)
		if err != nil {
			c.fail(err)
			return
		}
		if p[0] == KindAnnouncement {
			a, err := DecodeAnnouncement(p)
			if err != nil {
				c.fail(err)
				return
			}
			select {
			case c.Announcements <- a:
			default:
			}
			continue
		}
		var w waiter
		select {
		case w = <-c.waiters:
		default:
			c.fail(fmt.Errorf("%w: answer without a request", ErrNotMessage))
			return
		}
		if w.most > 0 && len(p) > w.most {
			// More than was asked for: what bounds the answers held ahead
			// of handling them would not hold.
			err := fmt.Errorf("%w: answer of %d bytes to a request for at most %d", ErrNotMessage, len(p), w.most)
			w.ch <- Result{err: err}
			c.fail(err)
			return
		}
		m, err := DecodeAnswer(p)
		var ea *ErrorAnswer
		if err != nil && !errors.As(err, &ea) {
			w.ch <- Result{err: err}
			c.fail(err)
			return
		}
		w.ch <- Result{msg: m, err: err}
	}
}

func (c *Conn) keepAlive() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
			if time.Since(time.Unix(0, c.lastReq.Load())) >= KeepAlive {
				go func() {
					defer func() { recover() }() //nolint:errcheck // a keep-alive has nothing to lose
					c.Do(context.Background(), &StateRequest{})
				}()
			}
		}
	}
}

// Send writes a request and returns a channel for its answer, so callers
// can pipeline.
func (c *Conn) Send(req Message) (<-chan Result, error) { return c.send(req, maxAnswer(req)) }

// maxAnswer is the most bytes an honest answer to a request has. An answer
// waits in memory until the task that asked takes it, and several tasks ask
// at once, so a peer must not be able to answer each with a frame of 16
// MiB. An error answer fits every bound.
func maxAnswer(req Message) int {
	const framing = 4 << 10 // tags, counts, an agent, an error's text
	switch r := req.(type) {
	case *RawRequest:
		return int(r.Length) + sliceFraming
	case *TransactionRawRequest:
		return int(r.Length) + sliceFraming
	case *StateRequest:
		return framing + 32*(MaxLocator+2*MaxPoolIDs)
	case *HeadersRequest:
		return framing + MaxHeaders*294
	case *InventoryRequest:
		return framing + 32*MaxInventory
	case *AddressesRequest:
		return framing + 32*MaxAddresses
	case *Import, *Submit:
		return framing
	case *PendingRequest, *DeferredRequest:
		// A launch-era node carries up to 8 MiB of transactions whole.
		return MaxFrame/2 + 64<<10
	}
	return 0
}

func (c *Conn) send(req Message, most int) (<-chan Result, error) {
	w := make(chan Result, 1)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	select {
	case <-c.done:
		return nil, c.Err()
	case c.waiters <- waiter{ch: w, most: most}:
	}
	c.lastReq.Store(time.Now().UnixNano())
	c.nc.SetWriteDeadline(time.Now().Add(AnswerTimeout))
	if err := WriteFrame(c.nc, Encode(req)); err != nil {
		c.fail(err)
		return nil, err
	}
	return w, nil
}

// Wait waits for an answer sent with Send.
func (c *Conn) Wait(ctx context.Context, w <-chan Result) (Message, error) {
	t := time.NewTimer(AnswerTimeout)
	defer t.Stop()
	select {
	case r := <-w:
		return r.msg, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.C:
		c.fail(errors.New("answer timed out"))
		return nil, errors.New("answer timed out")
	case <-c.done:
		select {
		case r := <-w:
			return r.msg, r.err
		default:
		}
		return nil, c.Err()
	}
}

// Do sends a request and waits for its answer. An error answer is returned
// as an *ErrorAnswer error.
func (c *Conn) Do(ctx context.Context, req Message) (Message, error) {
	w, err := c.Send(req)
	if err != nil {
		return nil, err
	}
	return c.Wait(ctx, w)
}

// State asks for the peer's current state.
func (c *Conn) State(ctx context.Context) (*State, error) {
	m, err := c.Do(ctx, &StateRequest{})
	if err != nil {
		return nil, err
	}
	s, ok := m.(*State)
	if !ok {
		return nil, fmt.Errorf("%w: unexpected answer", ErrNotMessage)
	}
	return s, nil
}
