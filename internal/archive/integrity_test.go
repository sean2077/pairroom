package archive

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestVerifyRejectsMissingInitialFactAndEmptyRoomIdentity(t *testing.T) {
	for _, test := range []string{"initial-sequence", "empty-room-id"} {
		t.Run(test, func(t *testing.T) {
			dir, _ := makeValidDataDir(t)
			path := filepath.Join(dir, "events.jsonl")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			var output strings.Builder
			for _, line := range lines {
				var event model.Event
				if err := json.Unmarshal([]byte(line), &event); err != nil {
					t.Fatal(err)
				}
				if test == "initial-sequence" {
					event.Seq++
				} else {
					event.RoomID = ""
				}
				value, err := json.Marshal(event)
				if err != nil {
					t.Fatal(err)
				}
				output.Write(value)
				output.WriteByte('\n')
			}
			if err := os.WriteFile(path, []byte(output.String()), 0600); err != nil {
				t.Fatal(err)
			}
			if report := Verify(dir); report.OK {
				t.Fatalf("corrupt history accepted: %#v", report)
			}
		})
	}
}
