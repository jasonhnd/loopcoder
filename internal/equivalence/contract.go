// Package equivalence executes and compares old and new behavior for migration epics.
package equivalence

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	ContractVersion = 1

	SliceModeRead       = "read"
	SliceModeSideEffect = "side_effect"

	RebaselineStatusAllowed = "allowed"
	RebaselineStatusBlocked = "blocked"
)

type Contract struct {
	Version                        int                     `json:"version" yaml:"version"`
	Partition                      Partition               `json:"partition" yaml:"partition"`
	Tolerance                      ToleranceRules          `json:"tolerance" yaml:"tolerance"`
	IntentionalDivergenceAllowlist []IntentionalDivergence `json:"intentional_divergence_allowlist,omitempty" yaml:"intentional_divergence_allowlist,omitempty"`
}

type Partition struct {
	ReadSlices       []string `json:"read_slices" yaml:"read_slices"`
	SideEffectSlices []string `json:"side_effect_slices" yaml:"side_effect_slices"`
}

type ToleranceRules struct {
	FloatPrecision           *FloatPrecision `json:"float_precision,omitempty" yaml:"float_precision,omitempty"`
	NullMappings             []NullMapping   `json:"null_mappings,omitempty" yaml:"null_mappings,omitempty"`
	OrderingInsensitivePaths []string        `json:"ordering_insensitive_paths,omitempty" yaml:"ordering_insensitive_paths,omitempty"`
}

type FloatPrecision struct {
	Absolute float64  `json:"absolute" yaml:"absolute"`
	Relative float64  `json:"relative,omitempty" yaml:"relative,omitempty"`
	Paths    []string `json:"paths,omitempty" yaml:"paths,omitempty"`
}

type NullMapping struct {
	Path     string `json:"path" yaml:"path"`
	OldValue any    `json:"old_value" yaml:"old_value"`
	NewValue any    `json:"new_value" yaml:"new_value"`
}

type IntentionalDivergence struct {
	ID             string   `json:"id" yaml:"id"`
	Approved       bool     `json:"approved" yaml:"approved"`
	PromotionClass bool     `json:"promotion_class" yaml:"promotion_class"`
	Paths          []string `json:"paths,omitempty" yaml:"paths,omitempty"`
	Reason         string   `json:"reason,omitempty" yaml:"reason,omitempty"`
}

type RebaselineRequest struct {
	RequestedByRole string `json:"requested_by_role" yaml:"requested_by_role"`
	PromotionClass  bool   `json:"promotion_class" yaml:"promotion_class"`
	AllowlistID     string `json:"allowlist_id" yaml:"allowlist_id"`
}

type RebaselineDecision struct {
	Status      string `json:"status"`
	Allowed     bool   `json:"allowed"`
	AllowlistID string `json:"allowlist_id,omitempty"`
	Detail      string `json:"detail"`
}

type contractWire struct {
	Version                        int                     `json:"version" yaml:"version"`
	Partition                      Partition               `json:"partition" yaml:"partition"`
	Tolerance                      *ToleranceRules         `json:"tolerance" yaml:"tolerance"`
	IntentionalDivergenceAllowlist []IntentionalDivergence `json:"intentional_divergence_allowlist" yaml:"intentional_divergence_allowlist"`
}

func ParseContract(data []byte) (Contract, error) {
	var wire contractWire
	if err := yaml.Unmarshal(data, &wire); err != nil {
		return Contract{}, fmt.Errorf("parse equivalence contract: %w", err)
	}
	if wire.Tolerance == nil {
		return Contract{}, fmt.Errorf("invalid equivalence contract: tolerance rules are required")
	}
	contract := Contract{
		Version:                        wire.Version,
		Partition:                      normalizePartition(wire.Partition),
		Tolerance:                      normalizeTolerance(*wire.Tolerance),
		IntentionalDivergenceAllowlist: normalizeAllowlist(wire.IntentionalDivergenceAllowlist),
	}
	if err := contract.Validate(); err != nil {
		return Contract{}, err
	}
	return contract, nil
}

func (c Contract) Validate() error {
	if c.Version != ContractVersion {
		return fmt.Errorf("invalid equivalence contract: version=%d is unsupported; supported version is %d", c.Version, ContractVersion)
	}
	if err := validatePartition(c.Partition); err != nil {
		return err
	}
	if !c.Tolerance.hasRule() {
		return fmt.Errorf("invalid equivalence contract: at least one tolerance rule is required")
	}
	if c.Tolerance.FloatPrecision != nil {
		if c.Tolerance.FloatPrecision.Absolute < 0 {
			return fmt.Errorf("invalid equivalence contract: tolerance.float_precision.absolute must be non-negative")
		}
		if c.Tolerance.FloatPrecision.Relative < 0 {
			return fmt.Errorf("invalid equivalence contract: tolerance.float_precision.relative must be non-negative")
		}
	}
	for _, mapping := range c.Tolerance.NullMappings {
		if strings.TrimSpace(mapping.Path) == "" {
			return fmt.Errorf("invalid equivalence contract: tolerance.null_mappings.path is required")
		}
	}
	return nil
}

func (c Contract) SliceMode(sliceID string) (string, error) {
	sliceID = strings.TrimSpace(sliceID)
	if sliceID == "" {
		return "", fmt.Errorf("slice id is required")
	}
	for _, candidate := range c.Partition.ReadSlices {
		if candidate == sliceID {
			return SliceModeRead, nil
		}
	}
	for _, candidate := range c.Partition.SideEffectSlices {
		if candidate == sliceID {
			return SliceModeSideEffect, nil
		}
	}
	return "", fmt.Errorf("slice %q is not declared in the equivalence contract partition", sliceID)
}

func AuthorizeRebaseline(contract Contract, request RebaselineRequest) RebaselineDecision {
	role := strings.ToLower(strings.TrimSpace(request.RequestedByRole))
	allowlistID := strings.TrimSpace(request.AllowlistID)
	if role != "human" {
		return RebaselineDecision{
			Status:  RebaselineStatusBlocked,
			Allowed: false,
			Detail:  "re-baseline is a promotion-class human decision; workers may not re-baseline golden masters",
		}
	}
	if !request.PromotionClass {
		return RebaselineDecision{
			Status:  RebaselineStatusBlocked,
			Allowed: false,
			Detail:  "re-baseline requires a promotion-class decision",
		}
	}
	if allowlistID == "" {
		return RebaselineDecision{
			Status:  RebaselineStatusBlocked,
			Allowed: false,
			Detail:  "re-baseline requires an approved intentional-divergence allowlist entry",
		}
	}
	for _, entry := range contract.IntentionalDivergenceAllowlist {
		if entry.ID == allowlistID && entry.Approved && entry.PromotionClass {
			return RebaselineDecision{
				Status:      RebaselineStatusAllowed,
				Allowed:     true,
				AllowlistID: allowlistID,
				Detail:      "re-baseline authorized by approved promotion-class intentional-divergence allowlist entry",
			}
		}
	}
	return RebaselineDecision{
		Status:      RebaselineStatusBlocked,
		Allowed:     false,
		AllowlistID: allowlistID,
		Detail:      "re-baseline allowlist entry is missing, unapproved, or not promotion-class",
	}
}

func normalizePartition(partition Partition) Partition {
	return Partition{
		ReadSlices:       cleanStrings(partition.ReadSlices),
		SideEffectSlices: cleanStrings(partition.SideEffectSlices),
	}
}

func normalizeTolerance(rules ToleranceRules) ToleranceRules {
	rules.OrderingInsensitivePaths = cleanStrings(rules.OrderingInsensitivePaths)
	if rules.FloatPrecision != nil {
		rules.FloatPrecision.Paths = cleanStrings(rules.FloatPrecision.Paths)
	}
	for i := range rules.NullMappings {
		rules.NullMappings[i].Path = strings.TrimSpace(rules.NullMappings[i].Path)
		rules.NullMappings[i].OldValue = normalizeValue(rules.NullMappings[i].OldValue)
		rules.NullMappings[i].NewValue = normalizeValue(rules.NullMappings[i].NewValue)
	}
	return rules
}

func normalizeAllowlist(entries []IntentionalDivergence) []IntentionalDivergence {
	out := make([]IntentionalDivergence, 0, len(entries))
	for _, entry := range entries {
		entry.ID = strings.TrimSpace(entry.ID)
		entry.Paths = cleanStrings(entry.Paths)
		entry.Reason = strings.TrimSpace(entry.Reason)
		if entry.ID != "" {
			out = append(out, entry)
		}
	}
	return out
}

func validatePartition(partition Partition) error {
	if len(partition.ReadSlices) == 0 && len(partition.SideEffectSlices) == 0 {
		return fmt.Errorf("invalid equivalence contract: partition must declare read_slices or side_effect_slices")
	}
	seen := map[string]string{}
	for _, slice := range partition.ReadSlices {
		if prior := seen[slice]; prior != "" {
			return fmt.Errorf("invalid equivalence contract: slice %q appears in both %s and %s partition lists", slice, prior, SliceModeRead)
		}
		seen[slice] = SliceModeRead
	}
	for _, slice := range partition.SideEffectSlices {
		if prior := seen[slice]; prior != "" {
			return fmt.Errorf("invalid equivalence contract: slice %q appears in both %s and %s partition lists", slice, prior, SliceModeSideEffect)
		}
		seen[slice] = SliceModeSideEffect
	}
	return nil
}

func (t ToleranceRules) hasRule() bool {
	return t.FloatPrecision != nil || len(t.NullMappings) > 0 || len(t.OrderingInsensitivePaths) > 0
}

func cleanStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func normalizeValue(value any) any {
	switch v := value.(type) {
	case nil:
		return nil
	case json.RawMessage:
		return decodeJSONValue([]byte(v))
	case []byte:
		return decodeJSONValue(v)
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return v
		}
		return decodeJSONValue(data)
	}
}

func decodeJSONValue(data []byte) any {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var out any
	if err := decoder.Decode(&out); err != nil {
		return string(data)
	}
	return out
}
