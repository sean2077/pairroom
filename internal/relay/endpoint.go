package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const EndpointFile = "relay-endpoint.json"

// Endpoint is owner-only local CLI discovery, never a Room or browser payload.
type Endpoint struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

func (e Endpoint) Validate() error {
	u, err := url.Parse(e.URL)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("relay Service URL must be a numeric-loopback HTTP origin")
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() || u.Port() == "" {
		return errors.New("relay Service URL must be numeric loopback with a port")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return errors.New("invalid relay Service port")
	}
	return nil
}
func ReadEndpoint(path string) (Endpoint, error) {
	var e Endpoint
	info, err := os.Lstat(path)
	if err != nil {
		return e, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return e, errors.New("Service endpoint must be a regular owner-only file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return e, errors.New("Service endpoint permissions must be 0600")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return e, err
	}
	if len(data) > 16384 {
		return e, errors.New("Service endpoint file is too large")
	}
	if err = json.Unmarshal(data, &e); err != nil {
		return e, errors.New("invalid Service endpoint file")
	}
	if strings.TrimSpace(e.Token) == "" {
		return e, errors.New("Service endpoint token missing")
	}
	return e, e.Validate()
}
func WriteEndpoint(root string, e Endpoint) error {
	if err := e.Validate(); err != nil {
		return err
	}
	return AtomicJSON(filepath.Join(root, EndpointFile), e)
}

// AtomicJSON consumes no sequence until the caller's entire next state is
// atomically replaced. Publication callers must not perform network I/O first.
func AtomicJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if info, err := os.Lstat(path); err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return errors.New("refusing to replace a non-regular state file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("atomic state replacement: %w", err)
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
