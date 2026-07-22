package privacy

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Finding is one leak-scan hit. Message shows field/location and marker label
// only — never the synthetic secret value itself.
type Finding struct {
	// Location is a surface + field path (e.g. "machine_summary.projects[0].path").
	Location string `json:"location"`
	// Label is MarkerLabel for the hit (e.g. "private_credential").
	Label string `json:"label"`
	// Destination is the sink being scanned.
	Destination Destination `json:"destination"`
}

// String renders a finding without secret values.
func (f Finding) String() string {
	return fmt.Sprintf("%s: leak of %s at destination %s", f.Location, f.Label, f.Destination)
}

// ScanText scans a single text blob for synthetic private markers.
func ScanText(dest Destination, location, text string) []Finding {
	if text == "" {
		return nil
	}
	var out []Finding
	for _, m := range AllMarkers() {
		if strings.Contains(text, m) {
			// For global destinations, any marker is a failure.
			// For project destinations, credential markers are always a failure;
			// other markers are also treated as failures when the destination
			// does not allow ClassCodePromptOutput (defensive: project logs may
			// keep bounded content, but raw canary markers still fail the scan
			// so fixtures prove redaction ran).
			if !Allowed(ClassCredentials, dest) || !Allowed(ClassCodePromptOutput, dest) {
				// Always record; project surfaces that allow code still must
				// not retain raw canary credential markers. We always flag
				// every marker in automated scan so acceptance criterion 5
				// is enforceable in unit tests.
				out = append(out, Finding{
					Location:    location,
					Label:       MarkerLabel(m),
					Destination: dest,
				})
			}
		}
	}
	return out
}

// ScanMap walks string values in a flat or nested map (JSON-decoded shape).
func ScanMap(dest Destination, location string, m map[string]any) []Finding {
	var out []Finding
	for k, v := range m {
		loc := location + "." + k
		if location == "" {
			loc = k
		}
		out = append(out, scanValue(dest, loc, v)...)
	}
	return out
}

// ScanJSON unmarshals raw JSON and scans all string leaves.
func ScanJSON(dest Destination, location string, raw []byte) ([]Finding, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		// Non-JSON: scan as opaque text.
		return ScanText(dest, location, string(raw)), nil
	}
	return scanValue(dest, location, v), nil
}

// ScanLines scans each line of a log/JSONL/human output blob.
func ScanLines(dest Destination, location string, text string) []Finding {
	var out []Finding
	for i, line := range strings.Split(text, "\n") {
		loc := fmt.Sprintf("%s:line[%d]", location, i+1)
		out = append(out, ScanText(dest, loc, line)...)
	}
	return out
}

// AssertClean fails the canary when any finding exists. Returns a multi-line
// report of locations/labels only.
func AssertClean(findings []Finding) error {
	if len(findings) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("privacy scan found %d leak(s):\n", len(findings)))
	for _, f := range findings {
		b.WriteString("  - ")
		b.WriteString(f.String())
		b.WriteByte('\n')
	}
	return fmt.Errorf("%s", strings.TrimRight(b.String(), "\n"))
}

func scanValue(dest Destination, location string, v any) []Finding {
	switch t := v.(type) {
	case string:
		return ScanText(dest, location, t)
	case map[string]any:
		return ScanMap(dest, location, t)
	case []any:
		var out []Finding
		for i, el := range t {
			out = append(out, scanValue(dest, fmt.Sprintf("%s[%d]", location, i), el)...)
		}
		return out
	default:
		// numbers/bools/null: no text content
		return nil
	}
}
