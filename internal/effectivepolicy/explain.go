package effectivepolicy

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/sanitize"
)

// Explain returns a human, redacted explanation of every effective value source.
func (s Snapshot) Explain() string {
	var b strings.Builder
	fmt.Fprintf(&b, "effective_policy schema=%d digest=%s\n", s.SchemaVersion, s.Digest)
	keys := make([]string, 0, len(s.Values))
	for k := range s.Values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		pv := s.Values[key]
		value := sanitize.Text(pv.Value)
		if value == "" {
			value = "(absent)"
		}
		fmt.Fprintf(&b, "  %s = %s  source=%s\n", key, value, pv.Source)
	}
	if len(s.Warnings) > 0 {
		b.WriteString("warnings:\n")
		for _, w := range s.Warnings {
			fmt.Fprintf(&b, "  - %s\n", sanitize.Text(w))
		}
	}
	return b.String()
}

// ExplainJSON returns machine-readable redacted provenance.
func (s Snapshot) ExplainJSON() ([]byte, error) {
	type row struct {
		Field  string `json:"field"`
		Value  string `json:"value"`
		Source Source `json:"source"`
	}
	type doc struct {
		SchemaVersion int      `json:"schema_version"`
		Digest        string   `json:"digest"`
		Values        []row    `json:"values"`
		Warnings      []string `json:"warnings,omitempty"`
	}
	keys := make([]string, 0, len(s.Values))
	for k := range s.Values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := doc{
		SchemaVersion: s.SchemaVersion,
		Digest:        s.Digest,
		Warnings:      append([]string(nil), s.Warnings...),
	}
	for _, key := range keys {
		pv := s.Values[key]
		out.Values = append(out.Values, row{
			Field:  key,
			Value:  sanitize.Text(pv.Value),
			Source: pv.Source,
		})
	}
	for i := range out.Warnings {
		out.Warnings[i] = sanitize.Text(out.Warnings[i])
	}
	return json.MarshalIndent(out, "", "  ")
}

// Get returns a field value and source.
func (s Snapshot) Get(field string) (ProvenancedValue, bool) {
	pv, ok := s.Values[field]
	return pv, ok
}

// RequiresCapability is a helper for admission: reading a frozen snapshot is
// cap.config_freeze; this package does not grant other capabilities.
func (s Snapshot) RequiresCapability() string {
	return "cap.config_freeze"
}
