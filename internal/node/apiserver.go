package node

import (
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"
)

// REST's connection limits (reference node, wiki: node/security). A plain
// HTTP server sets no timer and no bound: connections that send half a
// header, sit idle or never read their answer stay open for good, and about
// a thousand of them use up a typical open-file limit, after which the node
// can open neither peer connections nor block files.
const (
	// RESTConnections is the most REST connections open at once; further
	// ones wait in the listener's queue.
	RESTConnections = 512
	// restPerClient is the most from one client, an IPv4 address or an IPv6
	// /64. Connections from this machine are not counted per client, since a
	// local reverse proxy's all come from there; they count in the total.
	restPerClient = 16
	// restHeaderTime is how long a request's header may take, and how long a
	// connection from elsewhere may sit idle after an answer.
	restHeaderTime = 10 * time.Second
	// restLocalIdle is how long a connection from this machine may sit idle:
	// longer than a reverse proxy keeps idle connections (Caddy two minutes),
	// so the proxy closes them, never the node under a request on its way.
	restLocalIdle = 5 * time.Minute
	// restHeaderBytes bounds a request's header.
	restHeaderBytes = 64 << 10
	// restBackstop ends any request's writing, whatever set its deadline.
	restBackstop = 15 * time.Minute
)

// NewAPIServer returns the HTTP server REST runs on, with its timers set.
// Serve it on a listener from LimitListener.
func NewAPIServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: restHeaderTime,
		IdleTimeout:       restLocalIdle,
		WriteTimeout:      restBackstop,
		MaxHeaderBytes:    restHeaderBytes,
		ConnState: func(c net.Conn, s http.ConnState) {
			if lc, ok := c.(*limitedConn); ok {
				lc.state(s)
			}
		},
	}
}

// LimitListener bounds the connections accepted from ln: total at once, and
// restPerClient from one client. A connection past the total is not
// accepted until another closes; one past its client's share is closed.
func LimitListener(ln net.Listener, total int) net.Listener {
	if total <= 0 {
		total = RESTConnections
	}
	return &limitListener{Listener: ln, slots: make(chan struct{}, total), done: make(chan struct{}),
		clients: map[netip.Addr]int{}}
}

type limitListener struct {
	net.Listener
	slots chan struct{}
	done  chan struct{}
	once  sync.Once

	mu      sync.Mutex
	clients map[netip.Addr]int
}

func (l *limitListener) Accept() (net.Conn, error) {
	for {
		select {
		case l.slots <- struct{}{}:
		case <-l.done:
			return nil, net.ErrClosed
		}
		c, err := l.Listener.Accept()
		if err != nil {
			<-l.slots
			return nil, err
		}
		lc := &limitedConn{Conn: c, l: l, local: true}
		if ap, err := netip.ParseAddrPort(c.RemoteAddr().String()); err == nil && !ap.Addr().Unmap().IsLoopback() {
			lc.local = false
			lc.client = banKey(ap.Addr())
			l.mu.Lock()
			full := l.clients[lc.client] >= restPerClient
			if !full {
				l.clients[lc.client]++
			}
			l.mu.Unlock()
			if full {
				c.Close()
				<-l.slots
				continue
			}
		}
		return lc, nil
	}
}

func (l *limitListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.Listener.Close()
}

// limitedConn gives its place back when it closes, and closes itself when a
// client elsewhere leaves it idle.
type limitedConn struct {
	net.Conn
	l      *limitListener
	local  bool
	client netip.Addr

	mu     sync.Mutex
	idle   *time.Timer
	closed bool
}

func (c *limitedConn) state(s http.ConnState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.idle != nil {
		c.idle.Stop()
		c.idle = nil
	}
	if s == http.StateIdle && !c.local && !c.closed {
		c.idle = time.AfterFunc(restHeaderTime, func() { c.Close() })
	}
}

func (c *limitedConn) Close() error {
	c.mu.Lock()
	first := !c.closed
	c.closed = true
	if c.idle != nil {
		c.idle.Stop()
		c.idle = nil
	}
	c.mu.Unlock()
	err := c.Conn.Close()
	if first {
		if !c.local {
			c.l.mu.Lock()
			if c.l.clients[c.client]--; c.l.clients[c.client] <= 0 {
				delete(c.l.clients, c.client)
			}
			c.l.mu.Unlock()
		}
		<-c.l.slots
	}
	return err
}
