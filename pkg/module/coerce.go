package module

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

var sizeUnits = map[string]int64{
	"":    1,
	"b":   1,
	"k":   1024,
	"kb":  1024,
	"kib": 1024,
	"m":   1024 * 1024,
	"mb":  1024 * 1024,
	"mib": 1024 * 1024,
	"g":   1024 * 1024 * 1024,
	"gb":  1024 * 1024 * 1024,
	"gib": 1024 * 1024 * 1024,
	"t":   1024 * 1024 * 1024 * 1024,
	"tb":  1024 * 1024 * 1024 * 1024,
	"tib": 1024 * 1024 * 1024 * 1024,
}

func ParseSize(v any) (int64, error) {
	switch t := v.(type) {
	case int:
		return int64(t), nil
	case int64:
		return t, nil
	case float64:
		return int64(t), nil
	case string:
		return parseSizeString(t)
	}
	return 0, fmt.Errorf("size: unsupported type %T", v)
}

func parseSizeString(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("size: empty value")
	}
	i := 0
	for i < len(s) {
		c := s[i]
		if (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '+' {
			i++
			continue
		}
		break
	}
	numStr := strings.TrimSpace(s[:i])
	unit := strings.ToLower(strings.TrimSpace(s[i:]))
	mult, ok := sizeUnits[unit]
	if !ok {
		return 0, fmt.Errorf("size: unknown unit %q", unit)
	}
	if strings.Contains(numStr, ".") {
		f, err := strconv.ParseFloat(numStr, 64)
		if err != nil {
			return 0, fmt.Errorf("size: %w", err)
		}
		return int64(f * float64(mult)), nil
	}
	n, err := strconv.ParseInt(numStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("size: %w", err)
	}
	return n * mult, nil
}

func ParseDurationValue(v any) (time.Duration, error) {
	switch t := v.(type) {
	case time.Duration:
		return t, nil
	case int:
		return time.Duration(t) * time.Second, nil
	case int64:
		return time.Duration(t) * time.Second, nil
	case float64:
		return time.Duration(t * float64(time.Second)), nil
	case string:
		return time.ParseDuration(strings.TrimSpace(t))
	}
	return 0, fmt.Errorf("duration: unsupported type %T", v)
}
