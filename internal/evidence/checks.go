// Package evidence implements ordinary-development evidence ownership helpers.
//
// Required PR checks are derived from repository policy (.delivery.yml
// ci.checks). Optional review bots such as Greptile never block merge readiness
// unless an owner deliberately lists them as required checks.
package evidence

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// CheckStatus is a simplified hosted-check state used for merge-readiness
// evaluation. Callers map provider-specific states into these values.
type CheckStatus string

const (
	CheckStatusSuccess CheckStatus = "success"
	CheckStatusPending CheckStatus = "pending"
	CheckStatusFailed  CheckStatus = "failed"
	CheckStatusMissing CheckStatus = "missing"
)

// ObservedCheck is one named check reported for a pull request head.
type ObservedCheck struct {
	Name   string
	Status CheckStatus
}

// MergeReadiness is the policy evaluation of required versus optional checks.
type MergeReadiness struct {
	Required            []string
	OptionalObserved    []string
	MissingRequired     []string
	FailedRequired      []string
	PendingRequired     []string
	Ready               bool
	BlockingReasons     []string
	OptionalAbsentOK    bool
	OptionalFailedNames []string
}

// optionalReviewBot reports whether a check name is a known optional review bot.
// These names never become required solely by appearing in PR check output.
func optionalReviewBot(name string) bool {
	normalized := strings.ToLower(strings.TrimSpace(name))
	switch {
	case normalized == "":
		return false
	case normalized == "greptile review", normalized == "greptile":
		return true
	case strings.HasPrefix(normalized, "greptile"):
		return true
	default:
		return false
	}
}

// LoadRequiredChecks reads .delivery.yml ci.checks as the required PR check set.
func LoadRequiredChecks(deliveryYML []byte) ([]string, error) {
	var delivery struct {
		CI struct {
			Checks []string `yaml:"checks"`
		} `yaml:"ci"`
	}
	if err := yaml.Unmarshal(deliveryYML, &delivery); err != nil {
		return nil, fmt.Errorf("parse delivery config: %w", err)
	}
	out := make([]string, 0, len(delivery.CI.Checks))
	seen := make(map[string]struct{}, len(delivery.CI.Checks))
	for _, name := range delivery.CI.Checks {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("delivery ci.checks is empty")
	}
	sort.Strings(out)
	return out, nil
}

// LoadRequiredChecksFile loads required checks from a delivery config path.
func LoadRequiredChecksFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return LoadRequiredChecks(data)
}

// EvaluateMergeReadiness classifies observed checks against the policy-required
// set. Optional review bots that are not listed as required never block.
func EvaluateMergeReadiness(required []string, observed []ObservedCheck) MergeReadiness {
	requiredSet := make(map[string]struct{}, len(required))
	normalizedRequired := make([]string, 0, len(required))
	for _, name := range required {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := requiredSet[name]; ok {
			continue
		}
		requiredSet[name] = struct{}{}
		normalizedRequired = append(normalizedRequired, name)
	}
	sort.Strings(normalizedRequired)

	byName := make(map[string]CheckStatus, len(observed))
	var optionalObserved []string
	var optionalFailed []string
	for _, check := range observed {
		name := strings.TrimSpace(check.Name)
		if name == "" {
			continue
		}
		byName[name] = check.Status
		if _, isRequired := requiredSet[name]; !isRequired && optionalReviewBot(name) {
			optionalObserved = append(optionalObserved, name)
			if check.Status == CheckStatusFailed {
				optionalFailed = append(optionalFailed, name)
			}
		}
	}
	sort.Strings(optionalObserved)
	sort.Strings(optionalFailed)

	var missing, failed, pending, reasons []string
	for _, name := range normalizedRequired {
		status, ok := byName[name]
		if !ok {
			status = CheckStatusMissing
		}
		switch status {
		case CheckStatusSuccess:
			// ok
		case CheckStatusPending:
			pending = append(pending, name)
			reasons = append(reasons, "required check pending: "+name)
		case CheckStatusFailed:
			failed = append(failed, name)
			reasons = append(reasons, "required check failed: "+name)
		default:
			missing = append(missing, name)
			reasons = append(reasons, "required check missing: "+name)
		}
	}

	return MergeReadiness{
		Required:            normalizedRequired,
		OptionalObserved:    optionalObserved,
		MissingRequired:     missing,
		FailedRequired:      failed,
		PendingRequired:     pending,
		Ready:               len(missing) == 0 && len(failed) == 0 && len(pending) == 0,
		BlockingReasons:     reasons,
		OptionalAbsentOK:    true,
		OptionalFailedNames: optionalFailed,
	}
}
