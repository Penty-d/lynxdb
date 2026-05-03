package pipeline

import "os"

func sumSpillPathBytes(paths []string) int64 {
	var total int64
	for _, path := range paths {
		if info, err := os.Stat(path); err == nil {
			total += info.Size()
		}
	}

	return total
}
