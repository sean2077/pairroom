package lanshare

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestReadResponseErrorBoundsRemoteFieldsAndRejectsIncompleteJSON(t *testing.T) {
	const remoteMessage = "host storage is temporarily unavailable"
	for _, tc := range []struct {
		name    string
		body    string
		code    string
		message string
	}{
		{name: "named", body: `{"error":"` + remoteMessage + `","code":"` + HostUnavailableCode + `"}`, code: HostUnavailableCode, message: remoteMessage},
		{name: "invalid-json", body: `{"error":"partial","code":"` + HostUnavailableCode + `"`},
		{name: "trailing-json", body: `{"code":"` + HostUnavailableCode + `"}{}`},
		{name: "wrong-type", body: `{"error":"ignored","code":42}`},
		{name: "invalid-code", body: `{"code":"host/unavailable"}`},
		{name: "oversized-code", body: `{"code":"` + strings.Repeat("x", 129) + `"}`},
		{name: "oversized-message", body: `{"error":"` + strings.Repeat("x", 1025) + `","code":"` + HostUnavailableCode + `"}`, code: HostUnavailableCode},
		{name: "empty-body"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader(tc.body))}
			defer response.Body.Close()
			failure := ReadResponseError(response)
			wantMessage := tc.message
			if wantMessage == "" {
				wantMessage = http.StatusText(response.StatusCode)
			}
			if failure.Status != response.StatusCode || failure.Code != tc.code || failure.Message != wantMessage {
				t.Fatalf("error boundary lost definite HTTP status or trusted partial JSON: %+v", failure)
			}
		})
	}
}

type interruptedErrorReader struct{}

func (interruptedErrorReader) Read([]byte) (int, error) {
	return 0, errors.New("remote private read diagnostic")
}

func TestReadResponseErrorRetainsHTTPStatusWhenBodyCannotBeRead(t *testing.T) {
	valid := `{"error":"untrusted","code":"` + HostUnavailableCode + `"}`
	oversized := strings.NewReader(`{"code":"` + HostUnavailableCode + `","padding":"` + strings.Repeat("x", maxErrorResponseBytes*2) + `"}`)
	before := oversized.Len()
	for _, tc := range []struct {
		name string
		body io.Reader
	}{
		{name: "oversized", body: oversized},
		{name: "interrupted-after-json", body: io.MultiReader(strings.NewReader(valid), interruptedErrorReader{})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(tc.body)}
			defer response.Body.Close()
			failure := ReadResponseError(response)
			if failure.Status != http.StatusServiceUnavailable || failure.Code != "" || failure.Message != http.StatusText(http.StatusServiceUnavailable) {
				t.Fatalf("unreadable body obscured definite HTTP status or supplied partial code: %+v", failure)
			}
		})
	}
	if consumed := before - oversized.Len(); consumed > maxErrorResponseBytes+1 {
		t.Fatalf("error decoder read beyond its bound: %d", consumed)
	}
	failure := ReadResponseError(&http.Response{StatusCode: http.StatusForbidden})
	if failure.Status != http.StatusForbidden || failure.Code != "" {
		t.Fatalf("missing body erased a definite refusal: %+v", failure)
	}
}
