// Package review captures optional, bounded Git evidence observations. An anchor
// is neither an atomic filesystem snapshot nor permission to execute a plan.
package review

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sean2077/pairroom/internal/execx"
)

const maxEvidence = 16 << 20

var digest = regexp.MustCompile(`^[a-f0-9]{64}$`)
var commit = regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`)
var ErrUnavailable = errors.New("review evidence unavailable, changing, or over the 16 MiB / 1000-file limit")

type Anchor struct {
	Schema      int    `json:"schema"`
	Workspace   string `json:"workspace"`
	Base        string `json:"base"`
	Head        string `json:"head"`
	DirtySHA256 string `json:"dirty_sha256"`
}

func (a Anchor) Validate() error {
	if a.Schema != 1 || !commit.MatchString(a.Base) || !commit.MatchString(a.Head) || !digest.MatchString(a.DirtySHA256) || len(a.Workspace) > 2048 || !utf8.ValidString(a.Workspace) || strings.ContainsAny(a.Workspace, "\r\n\x00") || a.Workspace == "" {
		return errors.New("invalid review anchor")
	}
	return nil
}
func (a Anchor) Envelope() string {
	// JSON quoting keeps paths data; never interpolate them into a shell command.
	return fmt.Sprintf("\n\n[Review evidence: workspace=%q; base=%s; head=%s; dirty_sha256=%s. Verify this observation before relying on a prior review; it is not approval.]", a.Workspace, a.Base, a.Head, a.DirtySHA256)
}

type bounded struct{ bytes.Buffer }

func (b *bounded) Write(p []byte) (int, error) {
	if b.Len()+len(p) > maxEvidence {
		return 0, ErrUnavailable
	}
	return b.Buffer.Write(p)
}
func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	execx.NoConsole(cmd)
	cmd.WaitDelay = time.Second
	var out bounded
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return nil, ErrUnavailable
	}
	return out.Bytes(), nil
}
func Capture(parent context.Context, workspace, base string) (Anchor, error) {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	raw, err := git(ctx, workspace, "rev-parse", "--show-toplevel")
	if err != nil {
		return Anchor{}, err
	}
	root, err := filepath.EvalSymlinks(strings.TrimSpace(string(raw)))
	if err != nil {
		return Anchor{}, ErrUnavailable
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return Anchor{}, ErrUnavailable
	}
	resolve := func(ref string) (string, error) {
		out, e := git(ctx, root, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
		if e != nil {
			return "", e
		}
		v := strings.TrimSpace(string(out))
		if !commit.MatchString(v) {
			return "", ErrUnavailable
		}
		return v, nil
	}
	head, err := resolve("HEAD")
	if err != nil {
		return Anchor{}, err
	}
	if base == "" {
		base = head
	}
	base, err = resolve(base)
	if err != nil {
		return Anchor{}, err
	}
	h := sha256.New()
	used := 0
	add := func(label string, data []byte) error {
		used += len(data)
		if used > maxEvidence {
			return ErrUnavailable
		}
		fmt.Fprintf(h, "%d:%s:%d:", len(label), label, len(data))
		_, _ = h.Write(data)
		return nil
	}
	for _, cached := range []bool{false, true} {
		args := []string{"diff", "--no-ext-diff", "--no-textconv", "--no-color", "--full-index", "--binary"}
		if cached {
			args = append(args, "--cached")
		}
		args = append(args, head, "--")
		out, e := git(ctx, root, args...)
		if e != nil {
			return Anchor{}, e
		}
		if e = add(fmt.Sprint(cached), out); e != nil {
			return Anchor{}, e
		}
	}
	raw, err = git(ctx, root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return Anchor{}, err
	}
	names := bytes.Split(bytes.TrimSuffix(raw, []byte{0}), []byte{0})
	if len(names) > 1000 {
		return Anchor{}, ErrUnavailable
	}
	opened, err := os.OpenRoot(root)
	if err != nil {
		return Anchor{}, ErrUnavailable
	}
	defer opened.Close()
	for _, value := range names {
		name := string(value)
		if name == "" {
			continue
		}
		if !filepath.IsLocal(name) {
			return Anchor{}, ErrUnavailable
		}
		info, e := opened.Lstat(name)
		if e != nil {
			return Anchor{}, ErrUnavailable
		}
		var data []byte
		if info.Mode()&os.ModeSymlink != 0 {
			v, e := opened.Readlink(name)
			if e != nil {
				return Anchor{}, ErrUnavailable
			}
			data = []byte(v)
		} else if info.Mode().IsRegular() {
			if info.Size() > int64(maxEvidence-used) {
				return Anchor{}, ErrUnavailable
			}
			f, e := opened.Open(name)
			if e != nil {
				return Anchor{}, ErrUnavailable
			}
			data, e = io.ReadAll(io.LimitReader(f, int64(maxEvidence-used)+1))
			_ = f.Close()
			if e != nil {
				return Anchor{}, ErrUnavailable
			}
		} else {
			return Anchor{}, ErrUnavailable
		}
		if e = add(name+":"+info.Mode().String(), data); e != nil {
			return Anchor{}, e
		}
	}
	after, err := resolve("HEAD")
	if err != nil || after != head {
		return Anchor{}, ErrUnavailable
	}
	a := Anchor{Schema: 1, Workspace: root, Base: base, Head: head, DirtySHA256: hex.EncodeToString(h.Sum(nil))}
	return a, a.Validate()
}

// Check never follows a workspace supplied by an incoming message. The caller
// selects the trusted checkout. A different path/namespace is unverified.
func Check(ctx context.Context, trustedWorkspace string, a Anchor) string {
	if a.Validate() != nil {
		return "unverified"
	}
	current, err := Capture(ctx, trustedWorkspace, a.Base)
	if err != nil {
		return "unverified"
	}
	if current.Workspace != a.Workspace {
		return "different_workspace"
	}
	if current.Head != a.Head || current.DirtySHA256 != a.DirtySHA256 {
		return "stale"
	}
	return "unchanged_observation"
}
