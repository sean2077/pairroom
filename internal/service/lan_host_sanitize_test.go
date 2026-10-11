package service

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/attachment"
	"github.com/sean2077/pairroom/internal/lanshare"
)

// A LAN member never receives the host's path-bearing storage detail, and a
// refusal the member can act on carries its stable code. Engine validation
// errors stay useful because they carry no private detail.
func TestLANMemberResultSanitizesHostStorageDetail(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code string
	}{
		{name: "store path", err: fmt.Errorf("append event: %w", &os.PathError{Op: "write", Path: string(os.PathSeparator) + "host-data" + string(os.PathSeparator) + "rooms" + string(os.PathSeparator) + "room" + string(os.PathSeparator) + "events.jsonl", Err: errors.New("disk full")}), code: lanshare.HostStorageCode},
		{name: "shared quota", err: attachment.ErrSharedQuota, code: lanshare.SharedQuotaCode},
		{name: "temporary quota", err: attachment.ErrTemporaryQuota, code: lanshare.TemporaryQuotaCode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			lanMemberResult(recorder, nil, tc.err)
			if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), tc.code) {
				t.Fatalf("refusal lost its stable code: %d %s", recorder.Code, recorder.Body)
			}
			if strings.Contains(recorder.Body.String(), "host-data") || strings.Contains(recorder.Body.String(), "disk full") {
				t.Fatalf("host storage detail crossed the LAN boundary: %s", recorder.Body)
			}
		})
	}
	recorder := httptest.NewRecorder()
	lanMemberResult(recorder, nil, errors.New("explicit relay targets only the peer or @user"))
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "explicit relay targets") {
		t.Fatalf("validation guidance was lost: %d %s", recorder.Code, recorder.Body)
	}
}
