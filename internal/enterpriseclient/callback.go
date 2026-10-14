package enterpriseclient

import (
	"context"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"sync"
	"time"
)

type callbackDelivery struct {
	code secret
	ack  chan struct{}
}
type callback struct {
	uri, host, path string
	state           secret
	listener        net.Listener
	server          *http.Server
	mu              sync.Mutex
	consumed        bool
	received        chan callbackDelivery
}

func validCallbackURI(raw string) bool {
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || u.Path != "/pairroom-device-complete" || u.Opaque != "" {
		return false
	}
	ip, e := netip.ParseAddr(u.Hostname())
	if e != nil || !ip.IsLoopback() || ip.Is4In6() || ip.Zone() != "" || u.Hostname() != ip.String() {
		return false
	}
	port, e := strconv.Atoi(u.Port())
	return e == nil && port > 0 && port <= 65535 && strconv.Itoa(port) == u.Port() && u.String() == raw
}
func openCallback(previous string, state secret) (*callback, error) {
	if !validSecret(state) {
		return nil, ErrPrivateState
	}
	addr := "127.0.0.1:0"
	if previous != "" {
		if !validCallbackURI(previous) {
			return nil, ErrPrivateState
		}
		u, _ := url.Parse(previous)
		addr = u.Host
	}
	listener, e := net.Listen("tcp", addr)
	if e != nil {
		return nil, errors.New("enterprise callback listener is unavailable; preserve the pending request")
	}
	c := &callback{host: listener.Addr().String(), path: "/pairroom-device-complete", state: state, listener: listener, received: make(chan callbackDelivery, 1)}
	c.uri = "http://" + c.host + c.path
	if previous != "" && c.uri != previous {
		_ = listener.Close()
		return nil, ErrPrivateState
	}
	c.server = &http.Server{Handler: c, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 16 << 10}
	go func() { _ = c.server.Serve(listener) }()
	return c, nil
}
func (c *callback) Close() { _ = c.server.Close() }
func (c *callback) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'")
	remote, _, e := net.SplitHostPort(r.RemoteAddr)
	ip, ipe := netip.ParseAddr(remote)
	if e != nil || ipe != nil || !ip.IsLoopback() || r.TLS != nil || r.Method != http.MethodGet || r.Host != c.host || r.URL.Path != c.path || r.URL.RawPath != "" || r.URL.ForceQuery || r.ContentLength > 0 {
		http.Error(w, "Invalid callback", http.StatusBadRequest)
		return
	}
	values, e := url.ParseQuery(r.URL.RawQuery)
	if e != nil || len(values) != 2 || len(values["state"]) != 1 || len(values["completion_code"]) != 1 || subtle.ConstantTimeCompare([]byte(values.Get("state")), []byte(c.state)) != 1 || !validSecret(secret(values.Get("completion_code"))) {
		http.Error(w, "Invalid callback", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	if c.consumed {
		c.mu.Unlock()
		http.Error(w, "Callback already consumed", http.StatusConflict)
		return
	}
	c.consumed = true
	c.mu.Unlock()
	delivery := callbackDelivery{code: secret(values.Get("completion_code")), ack: make(chan struct{})}
	select {
	case c.received <- delivery:
	case <-r.Context().Done():
		return
	}
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	select {
	case <-delivery.ack:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("PairRoom authorization received. You may close this page."))
	case <-r.Context().Done():
	case <-timer.C:
		http.Error(w, "Authorization not confirmed; return to the client", http.StatusServiceUnavailable)
	}
}
func (c *callback) wait(ctx context.Context) (callbackDelivery, error) {
	select {
	case d := <-c.received:
		return d, nil
	case <-ctx.Done():
		return callbackDelivery{}, errors.New("enterprise login remains pending; use explicit recovery")
	}
}
func callbackLocation(uri string, state, code secret) string {
	return uri + "?" + url.Values{"state": {string(state)}, "completion_code": {string(code)}}.Encode()
}
