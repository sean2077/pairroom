// Package claudewake posts a fixed wake nudge to an already-running Claude Code
// inbox. It never launches/resumes Claude, claims relay input, or overrides the
// receiving session's inbound policy.
package claudewake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const FileName = "claude-inbox.json"

var (
	ErrUnavailable = errors.New("Claude inbox capability unavailable; use relay wait or rebind in the intended session")
	ErrSend        = errors.New("Claude inbox submission failed; queued input remains available through relay wait")
)

// Identity binds the private capability to one confirmed PairRoom generation.
// It is not a vendor lookup key: the address must come from that session's env.
type Identity struct {
	BindID     string `json:"bind_id"`
	Generation uint64 `json:"generation"`
	SessionID  string `json:"session_id"`
}

type capability struct {
	Identity
	Address string `json:"address"`
	Token   string `json:"token"`
}

// Send reports socket submission only, NOT vendor acceptance or a model turn.
// The returned closure keeps credentials out of printable/exportable structs.
type Send func(context.Context, string) error

// SlotDir resolves only the bound workspace's private slot directory. Neither
// a peer message nor the browser can supply an arbitrary credential-file path.
func SlotDir(root, room, slot string) (string, error) {
	if !filepath.IsAbs(root) || room == "" || room == "." || room == ".." || strings.ContainsAny(room, "/\\\x00\r\n") || (slot != "slot1" && slot != "slot2") {
		return "", ErrUnavailable
	}
	path := root
	for _, part := range []string{".pairroom", "rooms", room, "slots", slot} {
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !privateDirectory(info) {
			return "", ErrUnavailable
		}
	}
	return path, nil
}

// Capture is called only for a Service-confirmed bind, or a Stop whose official
// session matches that confirmed local binding; Prepare rechecks identity.
// Missing/invalid environment clears an older capability rather than retaining
// a stale endpoint. Errors are fixed text, never paths, tokens, or vendor output.
func Capture(dir string, identity Identity, address, token string) error {
	path := filepath.Join(dir, FileName)
	c := capability{Identity: identity, Address: address, Token: token}
	if !validCapability(c) {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return ErrUnavailable
		}
		if address == "" && token == "" {
			return nil
		}
		return ErrUnavailable
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return ErrUnavailable
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return ErrUnavailable
	}
	data, err := json.Marshal(c)
	if err != nil || len(data) > 16384 {
		return ErrUnavailable
	}
	data = append(data, '\n')
	if unchanged(path, data) {
		return nil // no per-Stop token rewrite or fsync for an identical capability
	}
	// Create with owner-only access before writing any token, including on
	// Windows where chmod(0600) alone does not establish a private DACL.
	f, err := privateTemp(dir)
	if err != nil {
		return ErrUnavailable
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return ErrUnavailable
	}
	if os.Rename(f.Name(), path) != nil {
		return ErrUnavailable
	}
	return nil
}

// unchanged accepts only the same private regular file that Prepare would
// trust, so skipping a rewrite never preserves a replaced or exposed file.
func unchanged(path string, data []byte) bool {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Size() != int64(len(data)) {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) || !privateFile(f, after) {
		return false
	}
	current, err := io.ReadAll(io.LimitReader(f, int64(len(data))+1))
	return err == nil && bytes.Equal(current, data)
}

// Prepare reads a bounded owner-only sidecar, never the vendor registry or
// transcript. Re-reading on each attempt handles Service restart and rebind.
func Prepare(dir string, identity Identity) (Send, error) {
	path := filepath.Join(dir, FileName)
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Size() > 16384 {
		return nil, ErrUnavailable
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) || !privateFile(f, after) {
		return nil, ErrUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(f, 16385))
	if err != nil || len(data) > 16384 {
		return nil, ErrUnavailable
	}
	var c capability
	if json.Unmarshal(data, &c) != nil || c.Identity != identity || !validCapability(c) {
		return nil, ErrUnavailable
	}
	return func(ctx context.Context, nudge string) error {
		// No reply/ack framing is documented for this socket. A complete write
		// must be recorded as submitted, not delivered/accepted by Claude.
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		auth, _ := json.Marshal(struct {
			Type  string `json:"type"`
			Token string `json:"token"`
		}{"auth", c.Token})
		message, _ := json.Marshal(struct {
			Type    string `json:"type"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
		}{Type: "user", Message: struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{"user", nudge}})
		frame := append(append(append(auth, '\n'), message...), '\n')
		if writeInbox(ctx, c.Address, frame) != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return ErrSend
		}
		return nil
	}, nil
}

func validCapability(c capability) bool {
	return c.BindID != "" && c.Generation > 0 && c.SessionID != "" && len(c.BindID) <= 256 && len(c.SessionID) <= 4096 && len(c.Address) <= 4096 && len(c.Token) > 0 && len(c.Token) <= 4096 && !strings.ContainsAny(c.Address+c.Token, "\x00\r\n") && validAddress(c.Address)
}
