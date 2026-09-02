package service

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ProxyTarget returns the verified loopback Room HTTP origin and the runtime
// bearer used by the Management surface gateway. The token is never copied
// into RuntimeStatus.
func (m *RuntimeManager) ProxyTarget(roomID string) (baseURL string, token string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return "", "", ErrRuntimeManagerClosed
	}
	entry := m.entries[roomID]
	if entry == nil || entry.phase != RuntimeActive || entry.runtime == nil {
		return "", "", ErrRuntimeNotReady
	}
	access, ok := entry.runtime.(RuntimeProxyAccess)
	if !ok {
		return "", "", fmt.Errorf("%w: room runtime does not expose a proxy endpoint", ErrRuntimeNotReady)
	}
	baseURL = strings.TrimSpace(access.ProxyBaseURL())
	token = strings.TrimSpace(access.ProxyToken())
	if baseURL == "" || token == "" {
		return "", "", fmt.Errorf("%w: room runtime proxy endpoint is incomplete", ErrRuntimeNotReady)
	}
	if _, err := parseLoopbackHTTPBase(baseURL); err != nil {
		return "", "", err
	}
	return baseURL, token, nil
}

func parseLoopbackHTTPBase(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse runtime endpoint: %w", err)
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("runtime endpoint must not include userinfo")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("runtime endpoint must be http")
	}
	host := parsed.Hostname()
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("runtime endpoint is not numeric loopback")
	}
	if parsed.Port() == "" {
		return nil, fmt.Errorf("runtime endpoint must include a port")
	}
	return &url.URL{Scheme: parsed.Scheme, Host: parsed.Host, Path: "/"}, nil
}
