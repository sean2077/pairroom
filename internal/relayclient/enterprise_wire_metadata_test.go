package relayclient

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// nativeMarker is one endpoint-native value class the wire check looks for.
// workspaceClass markers are the path-shaped ones that a validated review anchor may publish.
type nativeMarker struct {
	Label          string
	Value          string
	WorkspaceClass bool
}

// nativeMetadataScan is the outcome of scanning one enterprise request body.
type nativeMetadataScan struct {
	Malformed bool
	Leaked    string
}

// reviewWorkspacePath is the one documented field a validated review anchor may publish.
const reviewWorkspacePath = "intent.review.workspace"

// scanNativeMetadata reports the first endpoint-native marker that appears in a request body.
//
// The body is decoded before matching so the check behaves identically on every platform: a raw
// substring scan never matched a Windows path, because JSON escapes its separators, while the same
// content matched on POSIX. Every decoded string value, array element and object key is scanned.
//
// reviewWorkspace is the workspace of a review anchor that already passed Anchor.Validate().
// Exactly that field is spared the workspace-class markers only, because publishing the review
// checkout is the documented feature; every other marker is still matched against it, and every
// other position keeps every marker. The exemption is by field and class, never by value, so a
// workspace that equals a session identity or credential cannot hide that secret anywhere.
func scanNativeMetadata(body []byte, reviewWorkspace string, markers []nativeMarker) nativeMetadataScan {
	var decoded any
	if json.Unmarshal(body, &decoded) != nil {
		return nativeMetadataScan{Malformed: true}
	}
	var leaked string
	var walk func(value any, path string)
	walk = func(value any, path string) {
		if leaked != "" {
			return
		}
		switch typed := value.(type) {
		case string:
			for _, marker := range markers {
				if marker.Value == "" {
					continue
				}
				if marker.WorkspaceClass && path == reviewWorkspacePath && reviewWorkspace != "" && typed == reviewWorkspace {
					continue
				}
				if strings.Contains(typed, marker.Value) {
					leaked = marker.Label
					return
				}
			}
		case []any:
			for index, element := range typed {
				walk(element, path+"["+strconv.Itoa(index)+"]")
			}
		case map[string]any:
			for key, element := range typed {
				for _, marker := range markers {
					if marker.Value != "" && strings.Contains(key, marker.Value) {
						leaked = marker.Label
						return
					}
				}
				child := key
				if path != "" {
					child = path + "." + key
				}
				walk(element, child)
			}
		}
	}
	walk(decoded, "")
	return nativeMetadataScan{Leaked: leaked}
}

func TestEnterpriseWireMetadataScan(t *testing.T) {
	markers := []nativeMarker{
		{Label: "session", Value: "enterprise-native-session"},
		{Label: "workspace", Value: `C:\private\checkout`, WorkspaceClass: true},
		{Label: "credential", Value: "private-local-relay-capability"},
		{Label: "transcript_path", Value: "transcript_path"},
		{Label: "session_metadata", Value: "session_id"},
	}
	anchor := func(workspace, schema string) string {
		return `{"client_id":"reviewed","intent":{"text":"reviewed response","review":{"schema":` + schema + `,"workspace":"` + workspace + `","base":"b","head":"h","dirty_sha256":"d"}}}`
	}
	cases := []struct {
		name            string
		body            string
		reviewWorkspace string
		wantLeak        string
	}{
		{"validated review root is published", anchor(`C:\\private\\checkout`, "1"), `C:\private\checkout`, ""},
		{"invalid anchor root is not exempt", anchor(`C:\\private\\checkout`, "2"), "", "workspace"},
		{"review root repeated in another field", `{"client_id":"reviewed","intent":{"text":"C:\\private\\checkout","review":{"schema":1,"workspace":"C:\\private\\checkout"}}}`, `C:\private\checkout`, "workspace"},
		{"review workspace carrying the session identity while exempt", anchor(`enterprise-native-session`, "1"), "enterprise-native-session", "session"},
		{"review workspace carrying the capability while exempt", anchor(`private-local-relay-capability`, "1"), "private-local-relay-capability", "credential"},
		{"invalid anchor does not exempt a root that is also a session identity", anchor(`enterprise-native-session`, "2"), "", "session"},
		{"escaped workspace outside a review anchor", `{"client_id":"status","intent":{"text":"C:\\private\\checkout"}}`, "", "workspace"},
		{"native key rejected", `{"session_id":"clean"}`, "", "session_metadata"},
		{"nested native value rejected", `{"client_id":"status","intent":{"text":"enterprise-native-session"}}`, "", "session"},
		{"clean body", `{"client_id":"clean","intent":{"text":"plain"}}`, "", ""},
		{"malformed body", `not json`, "", ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			scan := scanNativeMetadata([]byte(testCase.body), testCase.reviewWorkspace, markers)
			if testCase.body == "not json" {
				if !scan.Malformed || scan.Leaked != "" {
					t.Fatalf("malformed body was not reported as malformed: %+v", scan)
				}
				return
			}
			if scan.Malformed || scan.Leaked != testCase.wantLeak {
				t.Fatalf("scan = %+v, want leak %q", scan, testCase.wantLeak)
			}
		})
	}
	// A validated review workspace must never exempt another marker class in the same field.
	if scan := scanNativeMetadata([]byte(anchor(`C:\\private\\checkout enterpr`+`ise-native-session`, "1")), `C:\private\checkout enterprise-native-session`, markers); scan.Leaked != "session" {
		t.Fatalf("session identity inside the review workspace was not reported: %+v", scan)
	}
}
