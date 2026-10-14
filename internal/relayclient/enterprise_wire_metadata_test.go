package relayclient

import (
	"encoding/json"
	"slices"
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

// reviewWorkspacePath is the one documented field a validated review anchor may publish. The
// exemption compares these segments exactly, so a key that merely contains the same characters,
// such as a root key literally named "intent.review.workspace", cannot take that field's place.
var reviewWorkspacePath = []string{"intent", "review", "workspace"}

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
// and every other field keeps every marker. The exemption is by exact path segments and class,
// never by value, so a workspace that equals a session identity or credential cannot hide that
// secret anywhere, and a key only shaped like the path cannot widen the exemption.
func scanNativeMetadata(body []byte, reviewWorkspace string, markers []nativeMarker) nativeMetadataScan {
	var decoded any
	if json.Unmarshal(body, &decoded) != nil {
		return nativeMetadataScan{Malformed: true}
	}
	leaked := ""
	// The path holds object keys only. An array element appends its index as one more segment, so a
	// value reached through an array can never satisfy the exact three-segment review path.
	child := func(path []string, segment string) []string {
		extended := make([]string, len(path)+1)
		copy(extended, path)
		extended[len(path)] = segment
		return extended
	}
	var walk func(value any, path []string)
	walk = func(value any, path []string) {
		if leaked != "" {
			return
		}
		switch typed := value.(type) {
		case string:
			for _, marker := range markers {
				if marker.Value == "" {
					continue
				}
				if marker.WorkspaceClass && reviewWorkspace != "" && typed == reviewWorkspace && slices.Equal(path, reviewWorkspacePath) {
					continue
				}
				if strings.Contains(typed, marker.Value) {
					leaked = marker.Label
					return
				}
			}
		case []any:
			for index, element := range typed {
				walk(element, child(path, strconv.Itoa(index)))
			}
		case map[string]any:
			for key, element := range typed {
				for _, marker := range markers {
					if marker.Value != "" && strings.Contains(key, marker.Value) {
						leaked = marker.Label
						return
					}
				}
				walk(element, child(path, key))
			}
		}
	}
	walk(decoded, nil)
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
	// insert adds one sibling key to a real body, so a case can place the same review workspace at
	// a key that only looks like the documented path.
	insert := func(body, after, addition string) string {
		return strings.Replace(body, after, after+addition, 1)
	}
	cases := []struct {
		name string
		body string
		want string
	}{
		{"validated review root is published", anchor(`C:\\private\\checkout`, ""), ""},
		{"invalid anchor does not exempt its root", invalidAnchor, "workspace"},
		{"review root repeated in another field", `{"client_id":"reviewed","intent":{"text":"C:\\private\\checkout","review":{"schema":1,"workspace":"C:\\private\\checkout","base":"` + validBase + `","head":"` + validHead + `","dirty_sha256":"` + validDigest + `"}}}`, "workspace"},
		{"dotted root key does not take the review path", insert(anchor(`C:\\private\\checkout`, ""), "{", `"intent.review.workspace":"C:\\private\\checkout",`), "workspace"},
		{"dotted nested key does not take the review path", insert(anchor(`C:\\private\\checkout`, ""), `"intent":{`, `"review.workspace":"C:\\private\\checkout",`), "workspace"},
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
