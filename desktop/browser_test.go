package main

import (
	"encoding/json"
	"testing"
)

func TestDesktopBrowserTarget(t *testing.T) {
	for _, tc := range []struct {
		url string
		ok  bool
	}{
		{"https://example.com/path?q=test#section", true},
		{"http://127.0.0.1:8080/rooms/test", true},
		{"javascript:alert(1)", false},
		{"file:///C:/Windows/system32/cmd.exe", false},
		{"ms-settings:defaultapps", false},
		{"https://user:secret@example.com", false},
		{"https:///missing-host", false},
		{"//example.com", false},
		{"https://example.com/\n", false},
	} {
		t.Run(tc.url, func(t *testing.T) {
			message, _ := json.Marshal(map[string]string{"kind": "pairroom.desktop.browser", "url": tc.url})
			target, ok := desktopBrowserTarget(string(message))
			if ok != tc.ok || (ok && target != tc.url) {
				t.Fatalf("target = %q, accepted = %v", target, ok)
			}
		})
	}
	for _, message := range []string{`{`, `{"kind":"pairroom.desktop.startup","url":"https://example.com"}`, `{"kind":"pairroom.desktop.browser","url":42}`} {
		if _, ok := desktopBrowserTarget(message); ok {
			t.Fatalf("accepted invalid request: %s", message)
		}
	}
}
