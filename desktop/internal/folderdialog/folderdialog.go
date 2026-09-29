// Package folderdialog carries the Desktop-only native folder picker used by
// the Management Register Project form. The bridge returns only the absolute
// path the user chose in the operating system's dialog: the Service still
// canonicalizes and validates the Git worktree, no directory listing crosses
// the bridge, and an ordinary browser never gains a native dialog.
package folderdialog

import (
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
)

const MessageKind = "pairroom.desktop.folder"

const (
	// maxMessageBytes bounds a whole request before any field is inspected.
	maxMessageBytes = 1024
	// maxIDBytes bounds the opaque correlation ID echoed back to the page.
	maxIDBytes = 100
	// maxPathBytes bounds the chosen path returned to the page.
	maxPathBytes = 4096
	// DefaultTitle labels the native dialog; the host owns no product copy.
	DefaultTitle = "Select Git worktree folder"
)

// Chooser shows the operating system's folder dialog and returns the chosen
// absolute path. An empty path with a nil error means the user cancelled.
type Chooser interface {
	ChooseFolder(title string) (string, error)
}

// Request is the bounded, exactly typed selector message.
type Request struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Response is the page-facing outcome. Path, Cancelled and Error are mutually
// exclusive; ID is always echoed so a late answer cannot settle another lane.
type Response struct {
	ID        string `json:"id"`
	Path      string `json:"path,omitempty"`
	Cancelled bool   `json:"cancelled,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Picker validates bridge requests and normalizes chooser outcomes. At most one
// dialog is open at a time: the page lane can expire while the window-modal
// dialog is still showing, and a second request must not stack another dialog.
type Picker struct {
	chooser Chooser
	open    atomic.Bool
}

func New(chooser Chooser) *Picker { return &Picker{chooser: chooser} }

// Parse accepts only a bounded, exactly typed request for this message kind.
// Anything else reports ok == false so the caller can try its next bridge lane.
func (p *Picker) Parse(message string) (Request, bool) {
	if len(message) > maxMessageBytes {
		return Request{}, false
	}
	var request Request
	decoder := json.NewDecoder(strings.NewReader(message))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return Request{}, false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Request{}, false
	}
	if request.Kind != MessageKind || request.ID == "" || len(request.ID) > maxIDBytes {
		return Request{}, false
	}
	return request, true
}

// Respond shows the dialog off the caller's goroutine and delivers exactly one
// response. The native dialog is window-modal and returns only when the user
// answers, so running it on the WebView message thread would stall every other
// bridge lane until then.
func (p *Picker) Respond(request Request, deliver func(Response)) {
	go func() { deliver(p.Pick(request)) }()
}

// Pick shows the dialog and reports the outcome. A missing chooser, an already
// open dialog, a failed dialog, and a non-absolute answer all fail closed; a
// cancelled dialog is a normal outcome, not an error.
func (p *Picker) Pick(request Request) Response {
	response := Response{ID: request.ID}
	if p.chooser == nil {
		response.Error = "The native folder dialog is unavailable"
		return response
	}
	if !p.open.CompareAndSwap(false, true) {
		response.Error = "A folder dialog is already open"
		return response
	}
	defer p.open.Store(false)
	path, err := p.chooser.ChooseFolder(DefaultTitle)
	if err != nil {
		response.Error = err.Error()
		return response
	}
	// macOS and Linux report a cancelled dialog as an empty path; the Windows
	// adapter normalizes that platform's cancellation error the same way.
	if path == "" {
		response.Cancelled = true
		return response
	}
	if len(path) > maxPathBytes || !filepath.IsAbs(path) {
		response.Error = "The folder dialog did not return an absolute path"
		return response
	}
	response.Path = path
	return response
}
