package relayclient

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/relay"
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

// validatedReviewWorkspace returns the workspace of a review anchor carried by the body when that
// anchor passed Anchor.Validate(). The fixture and its regression test both go through this path,
// so an anchor that fails validation never produces an exemption.
func validatedReviewWorkspace(body []byte) string {
	var scope struct {
		Intent relay.EnterpriseOperationIntent `json:"intent"`
	}
	if json.Unmarshal(body, &scope) != nil || scope.Intent.Review.Validate() != nil {
		return ""
	}
	return scope.Intent.Review.Workspace
}

// scanNativeMetadata reports the first endpoint-native marker that appears in a request body.
//
// The body is decoded before matching so the check behaves identically on every platform: a raw
// substring scan never matched a Windows path, because JSON escapes its separators, while the same
// content matched on POSIX. Every decoded string value, array element and object key is scanned.
//
// reviewWorkspace is the workspace of a review anchor that already passed Anchor.Validate().
// Exactly that field is spared the workspace-class markers only, because publishing the review
// checkout is the documented feature; every other marker class is still matched inside that field,
// and every other field keeps every marker. The exemption is by field and class, never by value, so
// a workspace that equals a session identity or credential cannot hide that secret anywhere.
func scanNativeMetadata(body []byte, reviewWorkspace string, markers []nativeMarker) nativeMetadataScan {
	var decoded any
	if json.Unmarshal(body, &decoded) != nil {
		return nativeMetadataScan{Malformed: true}
	}
	leaked := ""
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
	validBase, validHead, validDigest := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 64)
	// anchor builds a real body; an empty digest makes the anchor fail Anchor.Validate().
	anchor := func(workspace, dirty string) string {
		if dirty == "" {
			dirty = validDigest
		}
		return `{"client_id":"reviewed","intent":{"text":"reviewed response","review":{"schema":1,"workspace":"` + workspace + `","base":"` + validBase + `","head":"` + validHead + `","dirty_sha256":"` + dirty + `"}}}`
	}
	invalidAnchor := `{"client_id":"reviewed","intent":{"text":"reviewed response","review":{"schema":2,"workspace":"C:\\private\\checkout","base":"` + validBase + `","head":"` + validHead + `","dirty_sha256":"` + validDigest + `"}}}`
	cases := []struct {
		name string
		body string
		want string
	}{
		{"validated review root is published", anchor(`C:\\private\\checkout`, ""), ""},
		{"invalid anchor does not exempt its root", invalidAnchor, "workspace"},
		{"review root repeated in another field", `{"client_id":"reviewed","intent":{"text":"C:\\private\\checkout","review":{"schema":1,"workspace":"C:\\private\\checkout","base":"` + validBase + `","head":"` + validHead + `","dirty_sha256":"` + validDigest + `"}}}`, "workspace"},
		{"validated review workspace equal to the session identity", anchor("enterprise-native-session", ""), "session"},
		{"validated review workspace equal to the capability", anchor("private-local-relay-capability", ""), "credential"},
		{"escaped workspace outside a review anchor", `{"client_id":"status","intent":{"text":"C:\\private\\checkout"}}`, "workspace"},
		{"native key rejected", `{"session_id":"clean"}`, "session_metadata"},
		{"nested native value rejected", `{"client_id":"status","intent":{"text":"enterprise-native-session"}}`, "session"},
		{"clean body", `{"client_id":"clean","intent":{"text":"plain"}}`, ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			body := []byte(testCase.body)
			scan := scanNativeMetadata(body, validatedReviewWorkspace(body), markers)
			if scan.Malformed || scan.Leaked != testCase.want {
				t.Fatalf("scan = %+v, want leak %q", scan, testCase.want)
			}
		})
	}
	// A validation failure must not exempt anything, even when the shape looks like an anchor.
	if exempt := validatedReviewWorkspace([]byte(invalidAnchor)); exempt != "" {
		t.Fatalf("invalid anchor produced exemption %q", exempt)
	}
	if exempt := validatedReviewWorkspace([]byte(anchor(`C:\\private\\checkout`, strings.Repeat("f", 63)))); exempt != "" {
		t.Fatalf("short digest produced exemption %q", exempt)
	}
	if exempt := validatedReviewWorkspace([]byte(anchor(`C:\\private\\checkout`, ""))); exempt != `C:\private\checkout` {
		t.Fatalf("valid anchor did not produce its workspace, got %q", exempt)
	}
	// A marker class that is not workspace-shaped is still matched inside the exempt field.
	mixed := []byte(anchor(`C:\\private\\checkout enterprise-native-session`, ""))
	if scan := scanNativeMetadata(mixed, validatedReviewWorkspace(mixed), markers); scan.Leaked != "session" {
		t.Fatalf("session identity inside the review workspace was not reported: %+v", scan)
	}
	if scan := scanNativeMetadata([]byte("not json"), "", markers); !scan.Malformed || scan.Leaked != "" {
		t.Fatalf("malformed body was not reported as malformed: %+v", scan)
	}
}
