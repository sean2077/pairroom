package version

const (
	Current     = "1.1.0"
	StoreSchema = 8
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
