package lanshare

import (
	"encoding/json"
	"io"
	"net/http"
)

const maxErrorResponseBytes = 16 << 10

// ReadResponseError decodes a bounded error response without losing the fact
// that the host returned an HTTP status. Invalid, oversized or interrupted
// bodies retain that status but cannot supply a machine-readable error code.
// The caller closes the body and decides whether remote text may be displayed.
func ReadResponseError(response *http.Response) *Error {
	failure := &Error{Status: response.StatusCode, Message: http.StatusText(response.StatusCode)}
	if response.Body == nil {
		return failure
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxErrorResponseBytes+1))
	if err != nil || len(data) > maxErrorResponseBytes {
		return failure
	}
	var payload struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return failure
	}
	if ValidID(payload.Code) {
		failure.Code = payload.Code
	}
	if payload.Error != "" && len(payload.Error) <= 1024 {
		failure.Message = payload.Error
	}
	return failure
}
