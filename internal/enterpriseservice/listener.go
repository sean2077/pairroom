package enterpriseservice

import (
	"errors"
	"net"
	"sync"
)

// LimitListener bounds accepted sockets before TLS or HTTP
// parsing. The HTTP handler independently limits admitted business requests.
func LimitListener(inner net.Listener, limit int) (net.Listener, error) {
	if inner == nil || limit < 1 || limit > 256 {
		return nil, errors.New("enterprise connection limit invalid")
	}
	return &enterpriseLimitedListener{Listener: inner, slots: make(chan struct{}, limit), closed: make(chan struct{})}, nil
}

type enterpriseLimitedListener struct {
	net.Listener
	slots     chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
	closeErr  error
}

func (l *enterpriseLimitedListener) Accept() (net.Conn, error) {
	select {
	case <-l.closed:
		return nil, net.ErrClosed
	case l.slots <- struct{}{}:
	}
	select {
	case <-l.closed:
		<-l.slots
		return nil, net.ErrClosed
	default:
	}
	conn, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}
	return &enterpriseLimitedConn{Conn: conn, release: func() { <-l.slots }}, nil
}

func (l *enterpriseLimitedListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed); l.closeErr = l.Listener.Close() })
	return l.closeErr
}

type enterpriseLimitedConn struct {
	net.Conn
	release func()
	once    sync.Once
}

func (c *enterpriseLimitedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
