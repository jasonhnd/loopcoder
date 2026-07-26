package proctelemetry

import (
	"fmt"
	"strconv"
	"strings"
)

// parsePSTime parses [[dd-]hh:]mm:ss cumulative CPU from ps(1).
func parsePSTime(s string) (float64, error) {
	s = strings.TrimSpace(s)
	var days int
	if i := strings.IndexByte(s, '-'); i >= 0 {
		d, err := strconv.Atoi(s[:i])
		if err != nil {
			return 0, err
		}
		days = d
		s = s[i+1:]
	}
	parts := strings.Split(s, ":")
	var h, m int
	var sec float64
	var err error
	switch len(parts) {
	case 3:
		h, err = strconv.Atoi(parts[0])
		if err != nil {
			return 0, err
		}
		m, err = strconv.Atoi(parts[1])
		if err != nil {
			return 0, err
		}
		sec, err = strconv.ParseFloat(parts[2], 64)
		if err != nil {
			return 0, err
		}
	case 2:
		m, err = strconv.Atoi(parts[0])
		if err != nil {
			return 0, err
		}
		sec, err = strconv.ParseFloat(parts[1], 64)
		if err != nil {
			return 0, err
		}
	default:
		return 0, fmt.Errorf("bad time %q", s)
	}
	return float64(days*24*3600+h*3600+m*60) + sec, nil
}
