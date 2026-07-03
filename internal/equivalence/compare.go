package equivalence

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
)

const (
	DifferenceKindFloat      = "float"
	DifferenceKindNull       = "null"
	DifferenceKindOrdering   = "ordering"
	DifferenceKindStructural = "structural"
	DifferenceKindValue      = "value"
)

type Comparison struct {
	WithinTolerance bool         `json:"within_tolerance"`
	Differences     []Difference `json:"differences"`
}

type Difference struct {
	Path            string  `json:"path"`
	Kind            string  `json:"kind"`
	Detail          string  `json:"detail"`
	Distance        float64 `json:"distance,omitempty"`
	AllowedDistance float64 `json:"allowed_distance,omitempty"`
}

type NoiseFloor map[string]Difference

func Compare(contract Contract, oldValue, newValue any, noise NoiseFloor) Comparison {
	contract.Tolerance = normalizeTolerance(contract.Tolerance)
	var differences []Difference
	compareValues(contract, "$", normalizeValue(oldValue), normalizeValue(newValue), noise, &differences)
	return Comparison{
		WithinTolerance: len(differences) == 0,
		Differences:     differences,
	}
}

func NoiseFloorFrom(comparison Comparison) NoiseFloor {
	floor := NoiseFloor{}
	for _, difference := range comparison.Differences {
		key := differenceKey(difference.Kind, difference.Path)
		existing, ok := floor[key]
		if !ok || difference.Distance > existing.Distance {
			floor[key] = difference
		}
	}
	return floor
}

func compareValues(contract Contract, path string, oldValue, newValue any, noise NoiseFloor, differences *[]Difference) {
	if valuesEqual(oldValue, newValue) {
		return
	}
	if nullMappingAllowed(contract.Tolerance.NullMappings, path, oldValue, newValue) {
		return
	}
	if oldFloat, oldOK := asFloat(oldValue); oldOK {
		if newFloat, newOK := asFloat(newValue); newOK {
			recordFloatDifference(contract, path, oldFloat, newFloat, noise, differences)
			return
		}
	}
	switch oldTyped := oldValue.(type) {
	case map[string]any:
		newTyped, ok := newValue.(map[string]any)
		if !ok {
			recordDifference(Difference{
				Path:     path,
				Kind:     DifferenceKindStructural,
				Detail:   fmt.Sprintf("old is object and new is %T", newValue),
				Distance: 1,
			}, noise, differences)
			return
		}
		compareMaps(contract, path, oldTyped, newTyped, noise, differences)
	case []any:
		newTyped, ok := newValue.([]any)
		if !ok {
			recordDifference(Difference{
				Path:     path,
				Kind:     DifferenceKindStructural,
				Detail:   fmt.Sprintf("old is array and new is %T", newValue),
				Distance: 1,
			}, noise, differences)
			return
		}
		compareSlices(contract, path, oldTyped, newTyped, noise, differences)
	default:
		recordDifference(Difference{
			Path:     path,
			Kind:     differenceKindForValues(oldValue, newValue),
			Detail:   fmt.Sprintf("old=%s new=%s", formatValue(oldValue), formatValue(newValue)),
			Distance: 1,
		}, noise, differences)
	}
}

func compareMaps(contract Contract, path string, oldValue, newValue map[string]any, noise NoiseFloor, differences *[]Difference) {
	keys := map[string]bool{}
	for key := range oldValue {
		keys[key] = true
	}
	for key := range newValue {
		keys[key] = true
	}
	sorted := make([]string, 0, len(keys))
	for key := range keys {
		sorted = append(sorted, key)
	}
	sort.Strings(sorted)
	for _, key := range sorted {
		childPath := path + "." + key
		oldChild, oldOK := oldValue[key]
		newChild, newOK := newValue[key]
		switch {
		case !oldOK:
			recordDifference(Difference{
				Path:     childPath,
				Kind:     DifferenceKindStructural,
				Detail:   "field missing from old output",
				Distance: 1,
			}, noise, differences)
		case !newOK:
			recordDifference(Difference{
				Path:     childPath,
				Kind:     DifferenceKindStructural,
				Detail:   "field missing from new output",
				Distance: 1,
			}, noise, differences)
		default:
			compareValues(contract, childPath, oldChild, newChild, noise, differences)
		}
	}
}

func compareSlices(contract Contract, path string, oldValue, newValue []any, noise NoiseFloor, differences *[]Difference) {
	if pathMatchesAny(contract.Tolerance.OrderingInsensitivePaths, path) {
		oldCanonical := canonicalElements(oldValue)
		newCanonical := canonicalElements(newValue)
		if reflect.DeepEqual(oldCanonical, newCanonical) {
			return
		}
		recordDifference(Difference{
			Path:     path,
			Kind:     DifferenceKindOrdering,
			Detail:   "arrays differ after order-insensitive comparison",
			Distance: 1,
		}, noise, differences)
		return
	}
	if len(oldValue) != len(newValue) {
		recordDifference(Difference{
			Path:     path,
			Kind:     DifferenceKindStructural,
			Detail:   fmt.Sprintf("array length old=%d new=%d", len(oldValue), len(newValue)),
			Distance: 1,
		}, noise, differences)
	}
	limit := len(oldValue)
	if len(newValue) < limit {
		limit = len(newValue)
	}
	for i := 0; i < limit; i++ {
		compareValues(contract, fmt.Sprintf("%s[%d]", path, i), oldValue[i], newValue[i], noise, differences)
	}
}

func recordFloatDifference(contract Contract, path string, oldValue, newValue float64, noise NoiseFloor, differences *[]Difference) {
	distance := math.Abs(oldValue - newValue)
	allowed := floatTolerance(contract.Tolerance.FloatPrecision, path, oldValue)
	if noise != nil {
		if baseline, ok := noise[differenceKey(DifferenceKindFloat, path)]; ok && baseline.Distance > allowed {
			allowed = baseline.Distance
		}
	}
	if distance <= allowed {
		return
	}
	recordDifference(Difference{
		Path:            path,
		Kind:            DifferenceKindFloat,
		Detail:          fmt.Sprintf("float delta %.12g exceeds tolerance %.12g", distance, allowed),
		Distance:        distance,
		AllowedDistance: allowed,
	}, nil, differences)
}

func recordDifference(difference Difference, noise NoiseFloor, differences *[]Difference) {
	if difference.Distance == 0 {
		difference.Distance = 1
	}
	if noise != nil {
		if baseline, ok := noise[differenceKey(difference.Kind, difference.Path)]; ok && difference.Distance <= baseline.Distance {
			return
		}
	}
	*differences = append(*differences, difference)
}

func floatTolerance(rule *FloatPrecision, path string, oldValue float64) float64 {
	if rule == nil {
		return 0
	}
	if len(rule.Paths) > 0 && !pathMatchesAny(rule.Paths, path) {
		return 0
	}
	return rule.Absolute + rule.Relative*math.Abs(oldValue)
}

func nullMappingAllowed(mappings []NullMapping, path string, oldValue, newValue any) bool {
	for _, mapping := range mappings {
		if !pathMatches(mapping.Path, path) {
			continue
		}
		if valuesEqual(normalizeValue(mapping.OldValue), oldValue) && valuesEqual(normalizeValue(mapping.NewValue), newValue) {
			return true
		}
	}
	return false
}

func valuesEqual(left, right any) bool {
	if leftFloat, leftOK := asFloat(left); leftOK {
		if rightFloat, rightOK := asFloat(right); rightOK {
			return leftFloat == rightFloat
		}
	}
	return reflect.DeepEqual(left, right)
}

func asFloat(value any) (float64, bool) {
	switch v := value.(type) {
	case json.Number:
		out, err := v.Float64()
		return out, err == nil
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int8:
		return float64(v), true
	case int16:
		return float64(v), true
	case int32:
		return float64(v), true
	case int64:
		return float64(v), true
	case uint:
		return float64(v), true
	case uint8:
		return float64(v), true
	case uint16:
		return float64(v), true
	case uint32:
		return float64(v), true
	case uint64:
		return float64(v), true
	default:
		return 0, false
	}
}

func differenceKindForValues(oldValue, newValue any) string {
	if oldValue == nil || newValue == nil {
		return DifferenceKindNull
	}
	return DifferenceKindValue
}

func canonicalElements(values []any) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, canonicalValue(value))
	}
	sort.Strings(out)
	return out
}

func canonicalValue(value any) string {
	data, err := json.Marshal(normalizeValue(value))
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(data)
}

func formatValue(value any) string {
	text := canonicalValue(value)
	if len(text) > 160 {
		return text[:160] + "..."
	}
	return text
}

func pathMatchesAny(patterns []string, path string) bool {
	for _, pattern := range patterns {
		if pathMatches(pattern, path) {
			return true
		}
	}
	return false
}

func pathMatches(pattern, path string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return false
	}
	if pattern == "*" || pattern == "$" {
		return true
	}
	if pattern == path {
		return true
	}
	if strings.HasSuffix(pattern, ".*") {
		return strings.HasPrefix(path, strings.TrimSuffix(pattern, "*"))
	}
	return false
}

func differenceKey(kind, path string) string {
	return kind + "\x00" + path
}
