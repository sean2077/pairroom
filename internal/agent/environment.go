package agent

import (
	"runtime"
	"sort"
	"strings"
)

func mergeRuntimeEnv(base []string, overrides map[string]string) []string {
	return mergeRuntimeEnvForOS(base, overrides, runtime.GOOS)
}

// Windows environment names are case-insensitive. An inherited lower-case
// name must not win over an explicit per-participant credential after os/exec
// deduplicates Env. Keep Unix names case-sensitive and preserve Windows' hidden
// =C: drive-directory entries when reconstructing the environment.
func mergeRuntimeEnvForOS(base []string, overrides map[string]string, goos string) []string {
	if len(overrides) == 0 {
		return base
	}
	normalize := func(key string) string {
		if goos == "windows" {
			return strings.ToUpper(key)
		}
		return key
	}
	values := make(map[string]string, len(base)+len(overrides))
	for _, entry := range base {
		offset := strings.IndexByte(entry, '=')
		if offset == 0 && goos == "windows" {
			if next := strings.IndexByte(entry[1:], '='); next >= 0 {
				offset = next + 1
			}
		}
		if offset > 0 {
			values[normalize(entry[:offset])] = entry
		}
	}
	// Sorting makes even differently cased duplicate override keys deterministic.
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if strings.TrimSpace(key) != "" {
			values[normalize(key)] = key + "=" + overrides[key]
		}
	}
	keys = keys[:0]
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	output := make([]string, 0, len(keys))
	for _, key := range keys {
		output = append(output, values[key])
	}
	return output
}
