package version

import (
	"fmt"
	"strconv"
)

const (
	Current     = "2.0.0"
	StoreSchema = 9
)

// Commit, BuildDate, LastTag, and CommitsSinceTag are populated by the make
// and CI build pipelines with -ldflags. Keeping explicit development defaults
// makes locally built binaries honest.
var (
	Commit          = "dev"
	BuildDate       = "unknown"
	LastTag         = "unknown"
	CommitsSinceTag = "unknown"
)

type Info struct {
	Version         string `json:"version"`
	Commit          string `json:"commit"`
	BuildDate       string `json:"build_date"`
	LastTag         string `json:"last_tag"`
	CommitsSinceTag string `json:"commits_since_tag"`
	StoreSchema     int    `json:"store_schema"`
}

func BuildInfo() Info {
	return Info{
		Version: Current, Commit: Commit, BuildDate: BuildDate,
		LastTag: LastTag, CommitsSinceTag: CommitsSinceTag, StoreSchema: StoreSchema,
	}
}

// Describe renders the display version as the most recent tag combined with
// the commit count since that tag and the short commit SHA, e.g.
// "v1.1.0+116.21517d1". Binaries built without git metadata fall back to the
// bare tag. Current remains the canonical semver for protocol/compat use.
func Describe() string {
	tag := LastTag
	if tag == "" || tag == "unknown" {
		tag = "v" + Current
	}
	if Commit == "" || Commit == "dev" {
		return tag
	}
	sha := Commit
	if len(sha) > 7 {
		sha = sha[:7]
	}
	if n, err := strconv.Atoi(CommitsSinceTag); err == nil && n > 0 {
		return fmt.Sprintf("%s+%d.%s", tag, n, sha)
	}
	return tag + "+" + sha
}
