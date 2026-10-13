package codexwake

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

const fixtureNudge = "PairRoom inbox has messages for you. Run: pairroom relay wait"

func TestMain(m *testing.M) {
	if scenario := os.Getenv("PAIRROOM_CODEX_QUEUE_FIXTURE"); scenario != "" {
		if len(os.Args) != 3 || os.Args[1] != "app-server" || os.Args[2] != "--stdio" {
			os.Exit(30)
		}
		runAppServerFixture(scenario)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runAppServerFixture(scenario string) {
	scanner := bufio.NewScanner(os.Stdin)
	read := func() map[string]json.RawMessage {
		if !scanner.Scan() {
			os.Exit(31)
		}
		var request map[string]json.RawMessage
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			os.Exit(32)
		}
		return request
	}
	request := read()
	var params struct {
		ClientInfo struct {
			Name string `json:"name"`
		} `json:"clientInfo"`
		Capabilities struct {
			ExperimentalAPI bool `json:"experimentalApi"`
		} `json:"capabilities"`
	}
	if string(request["method"]) != `"initialize"` || string(request["id"]) != "1" || json.Unmarshal(request["params"], &params) != nil || !params.Capabilities.ExperimentalAPI || params.ClientInfo.Name != "pairroom_wake" {
		os.Exit(33)
	}
	if scenario == "timeout" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	fmt.Fprintln(os.Stderr, "SECRET-NATIVE-DIAGNOSTIC private-thread-id")
	fmt.Println(`{"id":1,"result":{"userAgent":"fixture"}}`)
	request = read()
	if string(request["method"]) != `"initialized"` || request["id"] != nil {
		os.Exit(34)
	}
	for page := 0; ; page++ {
		request = read()
		var params struct {
			ThreadID string `json:"threadId"`
			Limit    int    `json:"limit"`
			Cursor   string `json:"cursor"`
		}
		if string(request["method"]) != `"thread/queue/list"` || json.Unmarshal(request["params"], &params) != nil || params.ThreadID != "private-thread-id" || params.Limit != 100 {
			os.Exit(35)
		}
		if page > 0 && params.Cursor != fmt.Sprintf("cursor-%d", page) && scenario != "repeated-cursor" {
			os.Exit(36)
		}
		id := request["id"]
		result := `{"data":[],"nextCursor":null}`
		switch scenario {
		case "pending":
			result = queueFixture(`[{"type":"text","text":` + quoted(fixtureNudge) + `}]`)
		case "paginated":
			if page == 0 {
				result = `{"data":[],"nextCursor":"cursor-1"}`
			} else {
				result = queueFixture(`[{"type":"text","text":` + quoted(fixtureNudge) + `}]`)
			}
		case "substring":
			result = queueFixture(`[{"type":"text","text":` + quoted("user quoted: "+fixtureNudge) + `}]`)
		case "multiple-inputs":
			result = queueFixture(`[{"type":"text","text":` + quoted(fixtureNudge) + `},{"type":"image","url":"private-image"}]`)
		case "text-elements":
			result = queueFixture(`[{"type":"text","text":` + quoted(fixtureNudge) + `,"text_elements":[{"byteRange":{"start":0,"end":1}}]}]`)
		case "ordinary-text":
			result = queueFixture(`[{"type":"text","text":"ordinary user input","text_elements":[]}]`)
		case "large-valid-response":
			result = queueFixture(`[{"type":"text","text":` + quoted(strings.Repeat("X", 2<<20)) + `}]`)
		case "other-inputs":
			result = queueFixture(`[{"type":"localImage","path":"private-path"},{"type":"localAudio","path":"private-path"},{"type":"audio","url":"private-url"},{"type":"image","fileId":"private-file"},{"type":"skill","name":"private","path":"private"},{"type":"mention","name":"private","path":"private"}]`)
		case "rpc-error":
			fmt.Printf("{\"id\":%s,\"error\":{\"code\":-32601,\"message\":\"SECRET-NATIVE-DIAGNOSTIC private-thread-id\"}}\n", id)
			return
		case "malformed":
			fmt.Println(`SECRET-QUEUE-CONTENT not-json`)
			return
		case "truncated":
			fmt.Print(`{"id":2,"result":{"data":[`)
			return
		case "missing-data":
			result = `{}`
		case "null-data":
			result = `{"data":null}`
		case "wrong-data":
			result = `{"data":"SECRET-QUEUE-CONTENT"}`
		case "missing-input":
			result = `{"data":[{"id":"a","clientUserMessageId":"b"}],"nextCursor":null}`
		case "empty-input":
			result = queueFixture(`[]`)
		case "missing-cursor":
			result = `{"data":[]}`
		case "wrong-cursor":
			result = `{"data":[],"nextCursor":42}`
		case "missing-text":
			result = queueFixture(`[{"type":"text"}]`)
		case "unknown-type":
			result = queueFixture(`[{"type":"future-input","text":"private"}]`)
		case "empty-cursor":
			result = `{"data":[],"nextCursor":""}`
		case "wrong-id":
			id = json.RawMessage("999")
		case "repeated-cursor":
			result = `{"data":[],"nextCursor":"cursor-1"}`
		case "page-limit":
			result = fmt.Sprintf(`{"data":[],"nextCursor":"cursor-%d"}`, page+1)
		case "line-limit":
			fmt.Println(strings.Repeat("X", maxLineBytes+1))
			return
		case "total-limit":
			for range maxTotalBytes/(512<<10) + 1 {
				fmt.Printf("{\"method\":\"notification\",\"params\":%s}\n", quoted(strings.Repeat("X", 512<<10)))
			}
		case "notifications":
			fmt.Println(`{"method":"thread/status/changed","params":{"private":"SECRET-QUEUE-CONTENT"}}`)
		case "null-id-notification":
			fmt.Println(`{"id":null,"method":"thread/status/changed","params":{"private":"SECRET-QUEUE-CONTENT"}}`)
		case "null-id-error":
			fmt.Println(`{"id":null,"error":{"message":"SECRET-QUEUE-CONTENT"}}`)
		case "notification-error", "null-id-notification-error":
			if scenario == "notification-error" {
				fmt.Println(`{"method":"thread/status/changed","error":{"message":"SECRET-QUEUE-CONTENT"}}`)
			} else {
				fmt.Println(`{"id":null,"method":"thread/status/changed","error":{"message":"SECRET-QUEUE-CONTENT"}}`)
			}
		}
		fmt.Printf("{\"id\":%s,\"result\":%s}\n", id, result)
		if scenario != "paginated" && scenario != "page-limit" && scenario != "repeated-cursor" {
			return
		}
	}
}

func quoted(value string) string { data, _ := json.Marshal(value); return string(data) }

func queueFixture(input string) string {
	return `{"data":[{"id":"queue-id","clientUserMessageId":"message-id","input":` + input + `}],"nextCursor":null}`
}

func TestPendingAppServerBoundary(t *testing.T) {
	for _, scenario := range []string{"empty", "pending", "paginated", "ordinary-text", "large-valid-response", "other-inputs", "notifications", "null-id-notification"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("PAIRROOM_CODEX_QUEUE_FIXTURE", scenario)
			got, err := Pending(context.Background(), os.Args[0], "private-thread-id", fixtureNudge)
			want := scenario == "pending" || scenario == "paginated"
			if err != nil || got != want {
				t.Fatalf("Pending = %v, %v; want %v, nil", got, err, want)
			}
		})
	}
}

func TestPendingFailsClosedAndRedacts(t *testing.T) {
	for _, scenario := range []string{"rpc-error", "malformed", "truncated", "missing-data", "null-data", "wrong-data", "missing-input", "empty-input", "missing-text", "unknown-type", "missing-cursor", "wrong-cursor", "empty-cursor", "wrong-id", "repeated-cursor", "page-limit", "line-limit", "total-limit", "substring", "multiple-inputs", "text-elements", "null-id-error", "notification-error", "null-id-notification-error"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("PAIRROOM_CODEX_QUEUE_FIXTURE", scenario)
			got, err := Pending(context.Background(), os.Args[0], "private-thread-id", fixtureNudge)
			if got || err == nil {
				t.Fatalf("Pending = %v, %v; want false, error", got, err)
			}
			if (scenario == "line-limit" || scenario == "total-limit") && err.Error() != "codex wake queue: probe limit exceeded" {
				t.Fatalf("resource exhaustion was not distinguished: %v", err)
			}
			for _, secret := range []string{"SECRET", "private-thread-id", fixtureNudge, os.Args[0]} {
				if strings.Contains(err.Error(), secret) {
					t.Fatal("error exposed private transport data")
				}
			}
		})
	}
}

func TestProbeContextDeadlinePolicy(t *testing.T) {
	before := time.Now()
	ctx, cancel := probeContext(context.Background())
	defer cancel()
	after := time.Now()
	deadline, ok := ctx.Deadline()
	if !ok || deadline.Before(before.Add(15*time.Second)) || deadline.After(after.Add(15*time.Second)) {
		t.Fatalf("default deadline = %v, present = %v; want 15 seconds", deadline, ok)
	}
	for _, duration := range []time.Duration{100 * time.Millisecond, 30 * time.Second} {
		t.Run(duration.String(), func(t *testing.T) {
			want := time.Now().Add(duration)
			caller, callerCancel := context.WithDeadline(context.Background(), want)
			defer callerCancel()
			probe, probeCancel := probeContext(caller)
			defer probeCancel()
			if got, ok := probe.Deadline(); !ok || !got.Equal(want) {
				t.Fatalf("caller deadline changed: got %v, present = %v; want %v", got, ok, want)
			}
			callerCancel()
			if probe.Err() != context.Canceled {
				t.Fatal("caller cancellation did not reach probe")
			}
		})
	}
}

type failedReader struct{}

func (failedReader) Read([]byte) (int, error) {
	return 0, errors.New("SECRET-NATIVE-DIAGNOSTIC private-thread-id")
}

func TestInspectReadErrorIsDistinctAndRedacted(t *testing.T) {
	got, err := inspect(context.Background(), io.Discard, failedReader{}, "private-thread-id", fixtureNudge)
	if got || err == nil || err.Error() != "codex wake queue: response read failed" {
		t.Fatalf("inspect read failure = %v, %v", got, err)
	}
}

func TestPendingTimeoutAndUnavailable(t *testing.T) {
	t.Setenv("PAIRROOM_CODEX_QUEUE_FIXTURE", "timeout")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if got, err := Pending(ctx, os.Args[0], "private-thread-id", fixtureNudge); got || err == nil {
		t.Fatalf("Pending timeout = %v, %v", got, err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("probe timeout did not bound transport cleanup")
	}
	if got, err := Pending(context.Background(), "not-a-codex-executable-secret", "private-thread-id", fixtureNudge); got || err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("Pending unavailable = %v, %v", got, err)
	}
	if got, err := Pending(context.Background(), "", "", ""); got || err == nil {
		t.Fatalf("Pending invalid = %v, %v", got, err)
	}
	ctx, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if got, err := Pending(ctx, os.Args[0], "private-thread-id", fixtureNudge); got || err == nil {
		t.Fatalf("Pending canceled = %v, %v", got, err)
	}
}
