package aistudio

import (
	"fmt"
	"math"
	"strings"
)

// FormatBytes 将字节数自适应格式化为紧凑易读的单位（B / KB / MB / GB / TB，以 1024 为基数）
func FormatBytes(bytes int64) string {
	if bytes < 0 {
		if bytes == math.MinInt64 {
			return "-8388608TB"
		}
		return "-" + FormatBytes(-bytes)
	}
	if bytes < 1024 {
		return fmt.Sprintf("%dB", bytes)
	}

	val := float64(bytes)
	units := []string{"KB", "MB", "GB", "TB"}
	unitIdx := 0
	val /= 1024
	for val >= 1024 && unitIdx < len(units)-1 {
		val /= 1024
		unitIdx++
	}

	s := fmt.Sprintf("%.2f", val)
	s = strings.TrimSuffix(s, ".00")
	return s + units[unitIdx]
}
