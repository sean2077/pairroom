package enterpriseclient

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestNumericCallbackRejectsWrongStateHostAndDuplicateConsumption(t *testing.T) {
	state, _ := newSecret()
	code, _ := newSecret()
	c, e := openCallback("", state)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	for _, bad := range []struct {
		state secret
		host  string
		extra bool
	}{{"wrong", "", false}, {state, "attacker.example", false}, {state, "", true}} {
		target := callbackLocation(c.uri, bad.state, code)
		if bad.extra {
			target += "&state=" + url.QueryEscape(string(state))
		}
		r, _ := http.NewRequest(http.MethodGet, target, nil)
		if bad.host != "" {
			r.Host = bad.host
		}
		response, e := http.DefaultClient.Do(r)
		if e != nil {
			t.Fatal(e)
		}
		_ = response.Body.Close()
		if response.StatusCode != 400 {
			t.Fatalf("invalid callback status: %d", response.StatusCode)
		}
	}
	result := make(chan int, 1)
	go func() {
		r, e := http.Get(callbackLocation(c.uri, state, code))
		if e != nil {
			result <- 0
			return
		}
		defer r.Body.Close()
		_, _ = io.Copy(io.Discard, r.Body)
		result <- r.StatusCode
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	d, e := c.wait(ctx)
	if e != nil || d.code != code {
		t.Fatal("valid code not delivered")
	}
	close(d.ack)
	if <-result != 200 {
		t.Fatal("valid callback not acknowledged")
	}
	response, e := http.Get(callbackLocation(c.uri, state, code))
	if e != nil {
		t.Fatal(e)
	}
	_ = response.Body.Close()
	if response.StatusCode != 409 {
		t.Fatal("completion callback consumed twice")
	}
}
func TestCallbackRecoveryCannotRetargetFrozenAddress(t *testing.T) {
	s, _ := newSecret()
	for _, uri := range []string{"http://localhost:1234/pairroom-device-complete", "https://127.0.0.1:1234/pairroom-device-complete", "http://192.168.1.2:1234/pairroom-device-complete", "http://127.0.0.1:0/pairroom-device-complete", "http://[::ffff:127.0.0.1]:1234/pairroom-device-complete", "http://127.0.0.1:1234/another"} {
		if c, e := openCallback(uri, s); e == nil {
			c.Close()
			t.Fatalf("unsafe callback accepted: %s", uri)
		}
	}
	first, e := openCallback("", s)
	if e != nil {
		t.Fatal(e)
	}
	defer first.Close()
	if second, e := openCallback(first.uri, s); e == nil {
		second.Close()
		t.Fatal("recovery silently selected another port")
	}
}
