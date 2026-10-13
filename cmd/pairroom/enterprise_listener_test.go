package main

import (
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

type enterpriseListenerFixture struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func (l *enterpriseListenerFixture) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}
func (l *enterpriseListenerFixture) Close() error { l.once.Do(func() { close(l.closed) }); return nil }
func (l *enterpriseListenerFixture) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234}
}

func TestEnterpriseListenerBoundsBeforeAcceptAndReleasesEachSocketOnce(t *testing.T) {
	inner := &enterpriseListenerFixture{conns: make(chan net.Conn, 3), closed: make(chan struct{})}
	listener, err := newEnterpriseLimitedListener(inner, 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	for i := 0; i < 3; i++ {
		one, two := net.Pipe()
		inner.conns <- one
		t.Cleanup(func() { _ = two.Close() })
	}
	one, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	two, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	if len(inner.conns) != 1 {
		t.Fatal("listener exceeded accepted socket bound")
	}
	third := make(chan net.Conn, 1)
	go func() { conn, _ := listener.Accept(); third <- conn }()
	select {
	case <-third:
		t.Fatal("third socket accepted before capacity released")
	default:
	}
	_ = one.Close()
	_ = one.Close()
	var three net.Conn
	select {
	case three = <-third:
	case <-time.After(time.Second):
		t.Fatal("close failed to release socket")
	}
	if three == nil {
		t.Fatal("accepted socket absent")
	}
	_ = two.Close()
	_ = three.Close()
	four, peer := net.Pipe()
	defer peer.Close()
	inner.conns <- four
	accepted, err := listener.Accept()
	if err != nil {
		t.Fatal("socket capacity leaked")
	}
	_ = accepted.Close()
}

func TestEnterpriseListenerCloseUnblocksCapacityWaiter(t *testing.T) {
	inner := &enterpriseListenerFixture{conns: make(chan net.Conn, 1), closed: make(chan struct{})}
	one, two := net.Pipe()
	defer two.Close()
	inner.conns <- one
	listener, _ := newEnterpriseLimitedListener(inner, 1)
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	answer := make(chan error, 1)
	go func() { _, err := listener.Accept(); answer <- err }()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-answer:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatal("closed waiter returned wrong error")
		}
	case <-time.After(time.Second):
		t.Fatal("close stranded capacity waiter")
	}
}
