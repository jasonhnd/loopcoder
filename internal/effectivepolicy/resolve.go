package effectivepolicy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ProvenancedValue is one effective field with its winning source.
type ProvenancedValue struct {
	Value  string `json:"value"`
	Source Source `json:"source"`
}

// Snapshot is an immutable effective-policy freeze. Mutating fields after
// Resolve is undefined; treat the value as read-only. Configuration changes
// require Resolve/Successor, never in-place rewrite.
type Snapshot struct {
	SchemaVersion int                         `json:"schema_version"`
	Digest        string                      `json:"digest"`
	FrozenAt      time.Time                   `json:"frozen_at"`
	Values        map[string]ProvenancedValue `json:"values"`
	Warnings      []string                    `json:"warnings,omitempty"`
}

// Resolve merges inputs by precedence and freezes a digests snapshot.
func Resolve(in Inputs) (Snapshot, error) {
	project := in.ProjectPolicy
	if len(in.ProjectPolicyYAML) > 0 {
		parsed, err := ParsePolicyFile(in.ProjectPolicyYAML)
		if err != nil {
			return Snapshot{}, err
		}
		project = parsed
	}
	user := in.UserLocal
	if len(in.UserLocalYAML) > 0 {
		parsed, err := ParsePolicyFile(in.UserLocalYAML)
		if err != nil {
			return Snapshot{}, err
		}
		user = parsed
	}
	defaults := in.Defaults
	if defaults.SchemaVersion == 0 && defaults.BaseBranch == "" && defaults.Permission == "" {
		defaults = CompiledDefaults()
	}
	if defaults.SchemaVersion != 0 && defaults.SchemaVersion != SchemaVersion {
		return Snapshot{}, fmt.Errorf("effective policy: defaults schema_version %d incompatible", defaults.SchemaVersion)
	}

	// Environment must never become a configuration source for pins.
	warnings := detectIgnoredEnvOverrides(in.Env, in.Explicit, in.RunRequest)

	type candidate struct {
		value  string
		source Source
		set    bool
	}
	pick := func(field string, layers []struct {
		layer  Layer
		source Source
	}) (ProvenancedValue, error) {
		var best candidate
		for _, item := range layers {
			val, ok, err := layerField(item.layer, field)
			if err != nil {
				return ProvenancedValue{}, err
			}
			if !ok {
				continue
			}
			if !best.set || item.source.Rank() > best.source.Rank() {
				best = candidate{value: val, source: item.source, set: true}
			}
		}
		if !best.set {
			return ProvenancedValue{Value: "", Source: SourceAbsent}, nil
		}
		return ProvenancedValue{Value: best.value, Source: best.source}, nil
	}

	layers := []struct {
		layer  Layer
		source Source
	}{
		{defaults, SourceDefault},
		{user, SourceUserLocal},
		{project, SourceProjectPolicy},
		{in.RunRequest, SourceRunRequest},
		{in.Explicit, SourceExplicitCLI},
	}

	values := make(map[string]ProvenancedValue, 12)
	fields := []string{
		FieldProvider, FieldModel, FieldEffort, FieldPermission, FieldReportClient,
		FieldBaseBranch, FieldMaxChildProcesses, FieldMaxRSSMiB, FieldRetentionDays,
		FieldProjectPolicyPath, FieldNativeSubagents,
	}
	for _, field := range fields {
		pv, err := pick(field, layers)
		if err != nil {
			return Snapshot{}, err
		}
		// Mark compatibility-derived defaults when declared on the layer that won.
		if pv.Source == SourceDefault || pv.Source == SourceUserLocal || pv.Source == SourceProjectPolicy {
			for _, item := range layers {
				if item.source != pv.Source {
					continue
				}
				if item.layer.CompatibilityDerived[field] {
					pv.Source = SourceCompatibility
				}
			}
		}
		values[field] = pv
	}

	if err := validateEffective(values); err != nil {
		return Snapshot{}, err
	}

	// Explicit pins must remain the winners when set.
	if err := assertPinsNotOverridden(in.Explicit, values); err != nil {
		return Snapshot{}, err
	}

	frozenAt := time.Unix(0, 0).UTC()
	switch n := any(in.Now).(type) {
	case time.Time:
		if !n.IsZero() {
			frozenAt = n.UTC()
		}
	case *time.Time:
		if n != nil && !n.IsZero() {
			frozenAt = n.UTC()
		}
	}

	digest, err := digestValues(values)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{
		SchemaVersion: SchemaVersion,
		Digest:        digest,
		FrozenAt:      frozenAt,
		Values:        values,
		Warnings:      warnings,
	}, nil
}

// Successor builds a new snapshot from fresh inputs. It never mutates prev.
func Successor(prev Snapshot, in Inputs) (Snapshot, error) {
	next, err := Resolve(in)
	if err != nil {
		return Snapshot{}, err
	}
	if prev.Digest != "" && next.Digest == prev.Digest {
		// Identical policy is allowed; callers may still open a new attempt.
		return next, nil
	}
	return next, nil
}

func layerField(layer Layer, field string) (string, bool, error) {
	switch field {
	case FieldProvider:
		return nonEmpty(layer.Provider)
	case FieldModel:
		return nonEmpty(layer.Model)
	case FieldEffort:
		return nonEmpty(layer.Effort)
	case FieldPermission:
		return nonEmpty(layer.Permission)
	case FieldReportClient:
		return nonEmpty(layer.ReportClient)
	case FieldBaseBranch:
		return nonEmpty(layer.BaseBranch)
	case FieldProjectPolicyPath:
		return nonEmpty(layer.ProjectPolicyPath)
	case FieldMaxChildProcesses:
		if layer.MaxChildProcesses == 0 {
			return "", false, nil
		}
		if layer.MaxChildProcesses < 0 {
			return "", false, fmt.Errorf("effective policy: max_child_processes must be positive")
		}
		return strconv.Itoa(layer.MaxChildProcesses), true, nil
	case FieldMaxRSSMiB:
		if layer.MaxRSSMiB == 0 {
			return "", false, nil
		}
		if layer.MaxRSSMiB < 0 {
			return "", false, fmt.Errorf("effective policy: max_rss_mib must be positive")
		}
		return strconv.Itoa(layer.MaxRSSMiB), true, nil
	case FieldRetentionDays:
		if layer.RetentionDays == 0 {
			return "", false, nil
		}
		if layer.RetentionDays < 0 {
			return "", false, fmt.Errorf("effective policy: retention_days must be positive")
		}
		return strconv.Itoa(layer.RetentionDays), true, nil
	case FieldNativeSubagents:
		if layer.NativeSubagents == nil {
			return "", false, nil
		}
		if *layer.NativeSubagents {
			return "true", true, nil
		}
		return "false", true, nil
	default:
		return "", false, fmt.Errorf("effective policy: unknown field %q", field)
	}
}

func nonEmpty(v string) (string, bool, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", false, nil
	}
	return v, true, nil
}

func validateEffective(values map[string]ProvenancedValue) error {
	if path := values[FieldProjectPolicyPath].Value; path != "" {
		if err := validateLayerPaths(Layer{ProjectPolicyPath: path}); err != nil {
			return err
		}
	}
	if perm := values[FieldPermission].Value; perm != "" {
		switch perm {
		case "read_only", "bounded_write", "orchestrate":
		default:
			return fmt.Errorf("effective policy: invalid permission %q", perm)
		}
	}
	for _, field := range []string{FieldMaxChildProcesses, FieldMaxRSSMiB, FieldRetentionDays} {
		raw := values[field].Value
		if raw == "" {
			continue
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return fmt.Errorf("effective policy: invalid %s %q", field, raw)
		}
	}
	// Provider/model required for a launchable snapshot when permission is write/orchestrate.
	if values[FieldPermission].Value == "bounded_write" || values[FieldPermission].Value == "orchestrate" {
		if values[FieldProvider].Value == "" {
			return fmt.Errorf("effective policy: provider is required for permission %s", values[FieldPermission].Value)
		}
	}
	return nil
}

func assertPinsNotOverridden(explicit Layer, values map[string]ProvenancedValue) error {
	checks := map[string]string{
		FieldProvider:     explicit.Provider,
		FieldModel:        explicit.Model,
		FieldEffort:       explicit.Effort,
		FieldPermission:   explicit.Permission,
		FieldReportClient: explicit.ReportClient,
		FieldBaseBranch:   explicit.BaseBranch,
	}
	for field, want := range checks {
		want = strings.TrimSpace(want)
		if want == "" {
			continue
		}
		got := values[field]
		if got.Source != SourceExplicitCLI || got.Value != want {
			return fmt.Errorf("effective policy: explicit pin %s was overridden (source=%s value=%q)", field, got.Source, got.Value)
		}
	}
	if explicit.NativeSubagents != nil {
		want := "false"
		if *explicit.NativeSubagents {
			want = "true"
		}
		got := values[FieldNativeSubagents]
		if got.Source != SourceExplicitCLI || got.Value != want {
			return fmt.Errorf("effective policy: explicit pin %s was overridden", FieldNativeSubagents)
		}
	}
	return nil
}

func detectIgnoredEnvOverrides(env map[string]string, explicit, run Layer) []string {
	if len(env) == 0 {
		return nil
	}
	// Env keys that must never win over pins.
	mapping := map[string]string{
		"LOOPCODER_PROVIDER":      FieldProvider,
		"LOOPCODER_MODEL":         FieldModel,
		"LOOPCODER_EFFORT":        FieldEffort,
		"LOOPCODER_PERMISSION":    FieldPermission,
		"LOOPCODER_REPORT_CLIENT": FieldReportClient,
		"LOOPCODER_BASE_BRANCH":   FieldBaseBranch,
	}
	var warnings []string
	for envKey, field := range mapping {
		envVal := strings.TrimSpace(env[envKey])
		if envVal == "" {
			continue
		}
		// If a higher pin exists and differs, record that env was ignored.
		pin := strings.TrimSpace(layerString(explicit, field))
		if pin == "" {
			pin = strings.TrimSpace(layerString(run, field))
		}
		if pin != "" && pin != envVal {
			warnings = append(warnings, fmt.Sprintf("ignored environment %s for pin field %s", envKey, field))
		} else if pin != "" && pin == envVal {
			// Same value still does not become the source; note ignore for transparency.
			warnings = append(warnings, fmt.Sprintf("ignored environment %s; pin field %s remains non-environment sourced", envKey, field))
		}
	}
	sort.Strings(warnings)
	return warnings
}

func layerString(layer Layer, field string) string {
	v, ok, _ := layerField(layer, field)
	if !ok {
		return ""
	}
	return v
}

type digestDoc struct {
	SchemaVersion int                         `json:"schema_version"`
	Values        map[string]ProvenancedValue `json:"values"`
}

func digestValues(values map[string]ProvenancedValue) (string, error) {
	// Canonical JSON: sorted keys via encoding/json map sort in Go 1.x is
	// deterministic for string keys.
	doc := digestDoc{SchemaVersion: SchemaVersion, Values: values}
	payload, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
