// Package startup exposes only the native launch-at-login setting. The OS
// registration is the persistent source of truth; there is no second config
// file to drift, and constructing Settings never enables autostart.
package startup

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
)

const MessageKind = "pairroom.desktop.startup"

type Manager interface {
	Enable() error
	Disable() error
	IsEnabled() (bool, error)
}

type Settings struct {
	mu      sync.Mutex
	manager Manager
}

func New(manager Manager) *Settings { return &Settings{manager: manager} }

type Request struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Action  string `json:"action"`
	Enabled *bool  `json:"enabled,omitempty"`
}

type Response struct {
	ID      string `json:"id"`
	Enabled *bool  `json:"enabled,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Handle accepts only bounded, typed get/set requests. Invalid messages are
// ignored; the caller's bounded timeout reports unavailable native integration.
func (s *Settings) Handle(message string) (Response, bool) {
	if len(message) > 1024 {
		return Response{}, false
	}
	var request Request
	decoder := json.NewDecoder(strings.NewReader(message))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return Response{}, false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Response{}, false
	}
	if request.Kind != MessageKind || request.ID == "" || len(request.ID) > 100 {
		return Response{}, false
	}
	if request.Action != "get" && request.Action != "set" {
		return Response{}, false
	}
	if (request.Action == "set") != (request.Enabled != nil) {
		return Response{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	response := Response{ID: request.ID}
	if s.manager == nil {
		response.Error = "Desktop startup settings are unavailable"
		return response, true
	}
	var mutationErr error
	if request.Action == "set" {
		if *request.Enabled {
			mutationErr = s.manager.Enable()
		} else {
			mutationErr = s.manager.Disable()
		}
	}
	enabled, readErr := s.manager.IsEnabled()
	if readErr == nil {
		response.Enabled = &enabled
	}
	if err := errors.Join(mutationErr, readErr); err != nil {
		response.Error = err.Error()
	}
	return response, true
}

// TrustedOrigin binds IPC to the authenticated Management origin selected by
// the host, not just any loopback server. Wails supplies different frame
// metadata per platform: macOS has IsMainFrame, Windows has TopOrigin, Linux
// only has Origin. Never trust an origin merely because the URL says desktop=1.
func TrustedOrigin(managementURL, origin, topOrigin, platform string, mainFrame bool) bool {
	expected, ok := loopbackOrigin(managementURL)
	if !ok {
		return false
	}
	actual, ok := loopbackOrigin(origin)
	if !ok || actual != expected {
		return false
	}
	switch platform {
	case "darwin":
		return mainFrame
	case "windows":
		top, ok := loopbackOrigin(topOrigin)
		return ok && top == expected
	case "linux":
		return true
	default:
		return false
	}
}

func loopbackOrigin(value string) (string, bool) {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Port() == "" {
		return "", false
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return "", false
	}
	return u.Scheme + "://" + u.Host, true
}
