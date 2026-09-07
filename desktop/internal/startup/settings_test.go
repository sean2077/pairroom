package startup

import (
	"errors"
	"strings"
	"testing"
)

type fakeManager struct {
	enabled                  bool
	enables, disables, reads int
	writeErr, readErr        error
}

func (m *fakeManager) Enable() error {
	m.enables++
	if m.writeErr == nil {
		m.enabled = true
	}
	return m.writeErr
}
func (m *fakeManager) Disable() error {
	m.disables++
	if m.writeErr == nil {
		m.enabled = false
	}
	return m.writeErr
}
func (m *fakeManager) IsEnabled() (bool, error) { m.reads++; return m.enabled, m.readErr }

func TestStartupSettingIsOptInAndReadsOSState(t *testing.T) {
	m := &fakeManager{}
	s := New(m)
	if m.enables+m.disables+m.reads != 0 {
		t.Fatal("constructor touched OS settings")
	}
	for _, action := range []string{`"action":"get"`, `"action":"set","enabled":true`, `"action":"get"`, `"action":"set","enabled":false`} {
		response, ok := s.Handle(`{"kind":"` + MessageKind + `","id":"test",` + action + `}`)
		if !ok || response.Error != "" || response.Enabled == nil || *response.Enabled != m.enabled {
			t.Fatalf("response=%+v, ok=%v", response, ok)
		}
	}
	if m.enables != 1 || m.disables != 1 || m.reads != 4 {
		t.Fatalf("operations=%+v", m)
	}
	m.enabled = true // An OS change is visible, including after opening a new window.
	response, _ := New(m).Handle(`{"kind":"` + MessageKind + `","id":"test","action":"get"}`)
	if response.Enabled == nil || !*response.Enabled {
		t.Fatal("ignored OS registration")
	}
}

func TestStartupFailuresReturnActualState(t *testing.T) {
	m := &fakeManager{writeErr: errors.New("access denied")}
	s := New(m)
	response, ok := s.Handle(`{"kind":"` + MessageKind + `","id":"x","action":"set","enabled":true}`)
	if !ok || response.Error != "access denied" || response.Enabled == nil || *response.Enabled {
		t.Fatalf("false success: %+v", response)
	}
	m.readErr = errors.New("cannot inspect")
	response, _ = s.Handle(`{"kind":"` + MessageKind + `","id":"x","action":"get"}`)
	if response.Error == "" || response.Enabled != nil {
		t.Fatalf("uncertain read: %+v", response)
	}
}

func TestInvalidStartupRequestsNeverMutateOS(t *testing.T) {
	m := &fakeManager{}
	s := New(m)
	for _, request := range []string{
		`{}`, `{"kind":"wrong","id":"x","action":"get"}`,
		`{"kind":"pairroom.desktop.startup","id":"x","action":"set"}`,
		`{"kind":"pairroom.desktop.startup","id":"x","action":"get","enabled":false}`,
		`{"kind":"pairroom.desktop.startup","id":"x","action":"set","enabled":"true"}`,
		`{"kind":"pairroom.desktop.startup","id":"x","action":"exec"}`,
		`{"kind":"pairroom.desktop.startup","id":"x","action":"get","extra":1}`,
		`{"kind":"pairroom.desktop.startup","id":"x","action":"get"}{}`,
		strings.Repeat(" ", 1025),
	} {
		if _, ok := s.Handle(request); ok {
			t.Errorf("accepted %s", request)
		}
	}
	if m.enables+m.disables+m.reads != 0 {
		t.Fatalf("invalid request touched OS: %+v", m)
	}
}

func TestTrustedOrigin(t *testing.T) {
	const target = "http://127.0.0.1:9345/?desktop=1#token=secret"
	for _, tt := range []struct {
		origin, top, platform string
		main, want            bool
	}{
		{"http://127.0.0.1:9345", "", "darwin", true, true},
		{"http://127.0.0.1:9345", "", "darwin", false, false},
		{"http://127.0.0.1:9345/", "http://127.0.0.1:9345/", "windows", false, true},
		{"http://127.0.0.1:9345", "http://evil.test", "windows", true, false},
		{"http://127.0.0.1:9345", "", "windows", true, false},
		{"http://127.0.0.1:9345", "", "linux", false, true},
		{"http://127.0.0.1:9346", "", "linux", true, false},
		{"http://localhost:9345", "", "linux", true, false},
		{"http://127.0.0.1:9345@evil.test:9345", "", "linux", true, false},
		{"http://127.0.0.1:9345", "", "server", true, false},
	} {
		if got := TrustedOrigin(target, tt.origin, tt.top, tt.platform, tt.main); got != tt.want {
			t.Errorf("%+v: got %v", tt, got)
		}
	}
	if TrustedOrigin("", "http://127.0.0.1:9345", "", "linux", true) {
		t.Fatal("accepted before authenticated host startup")
	}
}
