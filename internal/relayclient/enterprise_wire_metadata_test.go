package relayclient

import (
	"encoding/json"
	"strings"
	"testing"
)

// nativeMetadataScan is the outcome of scanning one enterprise request body.
type nativeMetadataScan struct {
	Malformed bool
	Leaked    string
}

// scanNativeMetadata reports the first endpoint-native marker that appears in a request body.
//
// The body is decoded before matching so the check behaves identically on every platform: a raw
// substring scan never matched a Windows path, because JSON escapes its separators, while the same
// content matched on POSIX. Every decoded string value and object key is scanned.
//
// exempt is the one value that a caller has already validated as deliberate shared evidence (the
// review anchor's documented workspace field); exactly one occurrence of it is skipped, so the
// same value in any other position still fails. Labels name the marker class, never the value.
func scanNativeMetadata(body []byte, exempt string, markers map[string]string) nativeMetadataScan {
	var decoded any
	if json.Unmarshal(body, &decoded) != nil {
		return nativeMetadataScan{Malformed: true}
	}
	var values []string
	var collect func(value any)
	collect = func(value any) {
		switch typed := value.(type) {
		case string:
			values = append(values, typed)
		case []any:
			for _, element := range typed {
				collect(element)
			}
		case map[string]any:
			for key, element := range typed {
				values = append(values, key)
				collect(element)
			}
		}
	}
	collect(decoded)
	if exempt != "" {
		for index, value := range values {
			if value == exempt {
				values = append(values[:index], values[index+1:]...)
				break
			}
		}
	}
	for _, value := range values {
		for label, marker := range markers {
			if marker != "" && strings.Contains(value, marker) {
				return nativeMetadataScan{Leaked: label}
			}
		}
	}
	return nativeMetadataScan{}
}

func TestEnterpriseWireMetadataScan(t *testing.T) {
	markers := map[string]string{"session": "enterprise-native-session", "workspace": `C:\private\checkout`, "credential": "private-local-relay-capability", "transcript_path": "transcript_path", "session_metadata": "session_id"}
	// An escaped Windows path in the raw body must still be matched after decoding.
	escaped := []byte(`{"client_id":"reviewed","intent":{"text":"reviewed response","review":{"schema":1,"workspace":"C:\\private\\checkout","base":"b","head":"h","dirty_sha256":"d"}}}`)
	if scan := scanNativeMetadata(escaped, "", markers); scan.Leaked != "workspace" || scan.Malformed {
		t.Fatalf("escaped workspace path escaped the scan: %+v", scan)
	}
	if scan := scanNativeMetadata(escaped, `C:\private\checkout`, markers); scan.Leaked != "" || scan.Malformed {
		t.Fatalf("validated review workspace was not exempt: %+v", scan)
	}
	// The exemption is exactly one value: the same path anywhere else still fails.
	repeated := []byte(`{"client_id":"reviewed","intent":{"text":"C:\\private\\checkout","review":{"schema":1,"workspace":"C:\\private\\checkout"}}}`)
	if scan := scanNativeMetadata(repeated, `C:\private\checkout`, markers); scan.Leaked != "workspace" {
		t.Fatalf("exempt workspace in a second position was not reported: %+v", scan)
	}
	// Object keys and nested values are scanned as well.
	keyed := []byte(`{"client_id":"status","session_id":"enterprise-native-session"}`)
	if scan := scanNativeMetadata(keyed, "", markers); scan.Leaked == "" {
		t.Fatalf("nested native metadata was not reported: %+v", scan)
	}
	if scan := scanNativeMetadata([]byte(`{"client_id":"clean","intent":{"text":"plain"}}`), "", markers); scan.Leaked != "" || scan.Malformed {
		t.Fatalf("clean body reported a leak: %+v", scan)
	}
	if scan := scanNativeMetadata([]byte("not json"), "", markers); !scan.Malformed || scan.Leaked != "" {
		t.Fatalf("malformed body was not reported as malformed: %+v", scan)
	}
	// An absent exemption value must not silently exempt a different one.
	if scan := scanNativeMetadata(escaped, `D:\other`, markers); scan.Leaked != "workspace" {
		t.Fatalf("unrelated exemption suppressed a real match: %+v", scan)
	}
}
