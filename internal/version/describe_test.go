package version

import "testing"

func TestDescribeBuildMetadata(t *testing.T) {
	oldCommit, oldTag, oldCount := Commit, LastTag, CommitsSinceTag
	t.Cleanup(func() { Commit, LastTag, CommitsSinceTag = oldCommit, oldTag, oldCount })
	for _, tc := range []struct{ name, tag, commit, count, want string }{
		{"development", "unknown", "dev", "unknown", "v" + Current},
		{"empty metadata", "", "", "", "v" + Current},
		{"tag only", "v1.2.3", "dev", "0", "v1.2.3"},
		{"short SHA", "v1.2.3", "abc", "0", "v1.2.3+abc"},
		{"commits since tag", "v1.2.3", "123456789abcdef", "12", "v1.2.3+12.1234567"},
		{"invalid count", "v1.2.3", "123456789", "unknown", "v1.2.3+1234567"},
		{"negative count", "v1.2.3", "abcdefg", "-1", "v1.2.3+abcdefg"},
		{"fallback tag", "unknown", "abcdefg", "1", "v" + Current + "+1.abcdefg"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			LastTag, Commit, CommitsSinceTag = tc.tag, tc.commit, tc.count
			if got := Describe(); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStoreSchemaIsExactNotForwardCompatible(t *testing.T) {
	for _, schema := range []int{0, StoreSchema - 1, StoreSchema, StoreSchema + 1} {
		if got := SupportsStoreSchema(schema); got != (schema == StoreSchema) {
			t.Fatalf("schema %d accepted=%v", schema, got)
		}
	}
}
