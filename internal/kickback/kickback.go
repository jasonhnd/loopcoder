package kickback

import (
	"fmt"
	"strconv"
	"strings"
)

type Item struct {
	Raw       string
	Canonical string
	PRNumber  int
	SHA       string
}

func ParseItem(item string) Item {
	raw := strings.TrimSpace(item)
	if raw == "" {
		return Item{}
	}
	if number := parsePrefixedPRNumber(raw); number > 0 {
		return Item{
			Raw:       raw,
			Canonical: fmt.Sprintf("#%d", number),
			PRNumber:  number,
		}
	}
	if IsSHAish(raw) {
		sha := strings.ToLower(raw)
		return Item{
			Raw:       raw,
			Canonical: sha,
			SHA:       sha,
		}
	}
	if len(raw) < 7 {
		if number := parsePositiveDecimal(raw); number > 0 {
			return Item{
				Raw:       raw,
				Canonical: fmt.Sprintf("#%d", number),
				PRNumber:  number,
			}
		}
	}
	return Item{Raw: raw, Canonical: raw}
}

func CanonicalizeItem(item string) string {
	return ParseItem(item).Canonical
}

func PRNumber(item string) int {
	return ParseItem(item).PRNumber
}

func IsSHAish(item string) bool {
	item = strings.TrimSpace(item)
	if len(item) < 7 || len(item) > 40 {
		return false
	}
	for _, ch := range item {
		switch {
		case ch >= '0' && ch <= '9':
		case ch >= 'a' && ch <= 'f':
		case ch >= 'A' && ch <= 'F':
		default:
			return false
		}
	}
	return true
}

func parsePrefixedPRNumber(item string) int {
	lower := strings.ToLower(strings.TrimSpace(item))
	for _, prefix := range []string{"pr:", "pr#", "pr-", "#"} {
		if !strings.HasPrefix(lower, prefix) {
			continue
		}
		remainder := strings.TrimSpace(lower[len(prefix):])
		remainder = strings.TrimPrefix(remainder, "#")
		return parsePositiveDecimal(strings.TrimSpace(remainder))
	}
	return 0
}

func parsePositiveDecimal(item string) int {
	item = strings.TrimSpace(item)
	if item == "" {
		return 0
	}
	for _, ch := range item {
		if ch < '0' || ch > '9' {
			return 0
		}
	}
	number, err := strconv.Atoi(item)
	if err != nil || number <= 0 {
		return 0
	}
	return number
}
