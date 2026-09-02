package daemon

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseLogSizeAndBackups(t *testing.T) {
	for input, expected := range map[string]int64{"512": 512, "2K": 2048, "3MB": 3 * 1024 * 1024, "1g": 1024 * 1024 * 1024} {
		value, err := ParseLogSize(input)
		if err != nil || value != expected {
			t.Fatalf("ParseLogSize(%q) = %d, %v; want %d", input, value, err, expected)
		}
	}
	for _, input := range []string{"", "0", "-1", "wat", "999999999999999999TB"} {
		if _, err := ParseLogSize(input); err == nil {
			t.Fatalf("ParseLogSize(%q) succeeded", input)
		}
	}
	if value, err := ParseLogBackups("4"); err != nil || value != 4 {
		t.Fatalf("ParseLogBackups = %d, %v", value, err)
	}
	for _, input := range []string{"0", "-1", "many", "1001"} {
		if _, err := ParseLogBackups(input); err == nil {
			t.Fatalf("ParseLogBackups(%q) succeeded", input)
		}
	}
}

func TestRotatingWriterRetainsConfiguredBackups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.log")
	writer, err := NewRotatingWriter(path, 5, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"first\n", "second\n", "third\n"} {
		if _, err := writer.Write([]byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	assertFileText(t, path, "")
	assertFileText(t, path+".1", "third\n")
	assertFileText(t, path+".2", "second\n")
}

func assertFileText(t *testing.T, path, expected string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != expected {
		t.Fatalf("%s = %q, want %q", path, data, expected)
	}
}

func TestDaemonConsoleDetachRequested(t *testing.T) {
	t.Setenv(ConsoleDetachEnvironment, "")
	if daemonConsoleDetachRequested([]string{"service", "--no-browser"}) {
		t.Fatal("interactive service requested console detach")
	}
	t.Setenv(ConsoleDetachEnvironment, "1")
	if !daemonConsoleDetachRequested(nil) {
		t.Fatal("PAIRROOM_DETACH_CONSOLE=1 should detach")
	}
	t.Setenv(ConsoleDetachEnvironment, "")
	if !daemonConsoleDetachRequested([]string{"service", "--daemon-control-file", `C:\daemon.stop`}) {
		t.Fatal("daemon control file should detach")
	}
	if !daemonConsoleDetachRequested([]string{"service", `--daemon-control-file=C:\daemon.stop`}) {
		t.Fatal("daemon control file assignment should detach")
	}
}

func TestConfigureProcessLoggingFromEnvironment(t *testing.T) {
	if os.Getenv("PAIRROOM_LOG_HELPER") == "1" {
		cleanup, err := ConfigureProcessLoggingFromEnvironment()
		if err != nil {
			t.Fatal(err)
		}
		_, _ = os.Stdout.WriteString("stdout marker\n")
		_, _ = os.Stderr.WriteString("stderr marker\n")
		slog.Info("slog marker")
		if err := cleanup(); err != nil {
			t.Fatal(err)
		}
		return
	}
	path := filepath.Join(t.TempDir(), "service.log")
	command := exec.Command(os.Args[0], "-test.run=^TestConfigureProcessLoggingFromEnvironment$")
	command.Env = append(os.Environ(),
		"PAIRROOM_LOG_HELPER=1",
		LogFileEnvironment+"="+path,
		LogSizeEnvironment+"=1MB",
		LogBackupEnvironment+"=2",
		ConsoleDetachEnvironment+"=1",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("helper failed: %v\n%s", err, output)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"stdout marker", "stderr marker", "slog marker"} {
		if !strings.Contains(string(data), marker) {
			t.Fatalf("daemon log missing %q: %s", marker, data)
		}
	}
}
