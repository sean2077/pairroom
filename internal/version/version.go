package version

const (
	Current     = "1.1.0"
	StoreSchema = 7
)

// Commit and BuildDate are populated by release builds with -ldflags. Keeping
// explicit development defaults makes locally built binaries honest.
var (
	Commit    = "dev"
	BuildDate = "unknown"
)

type Info struct {
	Version     string `json:"version"`
	Commit      string `json:"commit"`
	BuildDate   string `json:"build_date"`
	StoreSchema int    `json:"store_schema"`
}

func BuildInfo() Info {
	return Info{Version: Current, Commit: Commit, BuildDate: BuildDate, StoreSchema: StoreSchema}
}
