//go:build !windows

package enterpriseservice

import (
	"os"
	"strconv"
	"strings"
)

func capacityRSS() uint64 {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "VmRSS:" {
			n, _ := strconv.ParseUint(fields[1], 10, 64)
			return n * 1024
		}
	}
	return 0
}
