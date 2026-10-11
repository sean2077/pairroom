package service

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sean2077/pairroom/internal/attachment"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relayclient"
)

// Exercise the owner upload route with genuinely provisioned local and LAN
// Rooms: a quota intended for sharing must not change local evidence behavior.
func TestNativeEvidenceQuotaAppliesOnlyToSharedRooms(t *testing.T) {
	for _, sharing := range []string{"local", "lan"} {
		t.Run(sharing, func(t *testing.T) {
			relayclient.IsolateNativeCaller(t)
			var native *nativeHostRuntime
			if sharing == "lan" {
				native = newLANHostFixture(t).native
			} else {
				native = nativeHTTP(t).native
			}
			filler, err := os.Create(filepath.Join(native.media.Root(), "retained-evidence.data"))
			if err != nil {
				t.Fatal(err)
			}
			if err := filler.Truncate(attachment.MaxRoomSharedBytes); err != nil {
				_ = filler.Close()
				t.Fatal(err)
			}
			if err := filler.Close(); err != nil {
				t.Fatal(err)
			}
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			part, err := writer.CreateFormFile("file", "repro.sh")
			if err != nil {
				t.Fatal(err)
			}
			const evidence = "#!/bin/sh\nprintf 'repro\\n'\n"
			if _, err := io.WriteString(part, evidence); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/attachments", &body)
			request.Header.Set("Content-Type", writer.FormDataContentType())
			request.Header.Set("X-PairRoom-Attachment-Kind", "file")
			response := httptest.NewRecorder()
			native.upload(response, request)
			if sharing == "lan" {
				if response.Code != http.StatusConflict {
					t.Fatalf("LAN owner bypassed shared quota: HTTP %d %s", response.Code, response.Body.String())
				}
				entries, err := os.ReadDir(native.media.Root())
				if err != nil || len(entries) != 1 || entries[0].Name() != "retained-evidence.data" {
					t.Fatalf("denied upload published or leaked evidence: %+v %v", entries, err)
				}
				return
			}
			var meta model.Attachment
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &meta) != nil {
				t.Fatalf("local owner inherited LAN quota: HTTP %d %s", response.Code, response.Body.String())
			}
			stored, path, err := native.media.Resolve(meta.ID)
			if err != nil || stored.Kind != "file" || filepath.Ext(path) != ".data" {
				t.Fatalf("local evidence lost inert storage: %+v %q %v", stored, path, err)
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != evidence {
				t.Fatalf("local evidence bytes changed: %q %v", data, err)
			}
		})
	}
}
