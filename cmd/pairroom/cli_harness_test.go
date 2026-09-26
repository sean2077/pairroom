package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRuntimeEnvironment turns a child copy of this test binary into a
// stand-in executable for doctor probes, so no vendor CLI is ever launched.
const fakeRuntimeEnvironment = "PAIRROOM_CLI_TEST_FAKE_RUNTIME"

// mainEnvironment makes a child copy of this test binary run the real main
// entry point, so exit codes and stderr are observed as a user sees them.
const mainEnvironment = "PAIRROOM_CLI_TEST_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(mainEnvironment) != "" {
		main()
		os.Exit(0)
	}
	if os.Getenv(fakeRuntimeEnvironment) != "" {
		os.Exit(runFakeRuntime(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func runFakeRuntime(args []string) int {
	name := strings.TrimSuffix(strings.ToLower(filepath.Base(os.Args[0])), ".exe")
	switch strings.Join(args, " ") {
	case "--version":
		if name == "git" {
			fmt.Println("git version 2.99.0.fake")
		} else {
			fmt.Println("fake-runtime 2.1.300 (test)")
		}
		return 0
	case "--help":
		fmt.Println("--resume --add-dir --include-partial-messages --model --permission-mode")
		return 0
	case "app-server --help":
		fmt.Println("Usage: fake app-server")
		return 0
	}
	fmt.Fprintln(os.Stderr, "unexpected fake runtime arguments:", args)
	return 2
}

// fakeExecutable exposes this test binary under name in a fresh directory.
// Child processes behave as runFakeRuntime while the returned cleanup-scoped
// environment variable is set.
func fakeExecutable(t *testing.T, dir, name string) string {
	t.Helper()
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	target := filepath.Join(dir, name)
	if err := os.Link(source, target); err != nil {
		data, readErr := os.ReadFile(source)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if err := os.WriteFile(target, data, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(fakeRuntimeEnvironment, "1")
	return target
}

// isolateUserDirs points every user-directory lookup at a test-owned root and
// returns the resulting os.UserConfigDir value.
func isolateUserDirs(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	t.Setenv("APPDATA", filepath.Join(root, "AppData", "Roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(root, "AppData", "Local"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, ".config"))
	base, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	return base
}

// stdoutCapture replaces os.Stdout until Stop. Start it before, and Stop it
// after, any goroutine that prints so the swap stays race-free.
type stdoutCapture struct {
	mu       sync.Mutex
	buffer   bytes.Buffer
	original *os.File
	writer   *os.File
	done     chan struct{}
	once     sync.Once
}

func startStdoutCapture(t *testing.T) *stdoutCapture {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	capture := &stdoutCapture{original: os.Stdout, writer: writer, done: make(chan struct{})}
	os.Stdout = writer
	go func() {
		defer close(capture.done)
		defer reader.Close()
		chunk := make([]byte, 4096)
		for {
			n, err := reader.Read(chunk)
			capture.mu.Lock()
			capture.buffer.Write(chunk[:n])
			capture.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { capture.Stop() })
	return capture
}

func (c *stdoutCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buffer.String()
}

func (c *stdoutCapture) Stop() string {
	c.once.Do(func() {
		os.Stdout = c.original
		_ = c.writer.Close()
		<-c.done
	})
	return c.String()
}

// waitFor polls an observable condition; it is not a synchronization sleep.
func (c *stdoutCapture) waitFor(t *testing.T, text string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if output := c.String(); strings.Contains(output, text) {
			return output
		}
		if time.Now().After(deadline) {
			t.Fatalf("stdout never contained %q:\n%s", text, c.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func captureRun(t *testing.T, args ...string) (string, error) {
	t.Helper()
	capture := startStdoutCapture(t)
	err := run(args)
	return capture.Stop(), err
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%s exists or is unreadable: %v", path, err)
	}
}

func requireErrorContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want it to contain %q", err, want)
	}
}
