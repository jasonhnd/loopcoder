package audit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type BaselineFile struct {
	Version int              `yaml:"version"`
	Waivers []BaselineWaiver `yaml:"waivers"`
}

type BaselineWaiver struct {
	ID                 string `yaml:"id"`
	Rule               string `yaml:"rule"`
	MatchingRule       string `yaml:"matching_rule"`
	Path               string `yaml:"path"`
	PathGlob           string `yaml:"path_glob"`
	Fingerprint        string `yaml:"fingerprint"`
	NormalizedEvidence string `yaml:"normalized_evidence"`
	OriginalSeverity   string `yaml:"original_severity"`
	Justification      string `yaml:"justification"`
	Date               string `yaml:"date"`
	DateAdded          string `yaml:"date_added"`
	ReviewBy           string `yaml:"review_by"`
	Expires            string `yaml:"expires"`
	Expiry             string `yaml:"expiry"`
}

type BaselineValidation struct {
	Errors  []string
	Expired []BaselineWaiver
	Broad   []BaselineWaiver
}

func LoadBaseline(repoPath, rawPath string, now time.Time) (BaselineFile, BaselineValidation, error) {
	clean, err := safeRepoRelativePath(rawPath)
	if err != nil {
		return BaselineFile{}, BaselineValidation{}, fmt.Errorf("audit.baseline.path %q is invalid: %w", rawPath, err)
	}
	data, err := os.ReadFile(filepath.Join(repoPath, filepath.FromSlash(clean)))
	if err != nil {
		return BaselineFile{}, BaselineValidation{}, fmt.Errorf("read audit.baseline.path %s: %w", clean, err)
	}
	var baseline BaselineFile
	if err := yaml.Unmarshal(data, &baseline); err != nil {
		return BaselineFile{}, BaselineValidation{}, fmt.Errorf("parse audit baseline %s: %w", clean, err)
	}
	return baseline, ValidateBaseline(baseline, now), nil
}

func ValidateBaseline(baseline BaselineFile, now time.Time) BaselineValidation {
	validation := BaselineValidation{}
	seen := map[string]bool{}
	for index, waiver := range baseline.Waivers {
		label := fmt.Sprintf("waivers[%d]", index)
		id := strings.TrimSpace(waiver.ID)
		if id == "" {
			validation.Errors = append(validation.Errors, label+".id is required")
		} else {
			label = fmt.Sprintf("waiver %q", id)
			if seen[id] {
				validation.Errors = append(validation.Errors, label+" duplicates another waiver id")
			}
			seen[id] = true
		}
		if waiverRule(waiver) == "" {
			validation.Errors = append(validation.Errors, label+".rule or .matching_rule is required")
		}
		if waiverPath(waiver) == "" && waiverPathGlob(waiver) == "" {
			validation.Errors = append(validation.Errors, label+".path or .path_glob is required")
		}
		if strings.TrimSpace(waiver.Fingerprint) == "" && strings.TrimSpace(waiver.NormalizedEvidence) == "" {
			validation.Broad = append(validation.Broad, waiver)
			validation.Errors = append(validation.Errors, label+".fingerprint or .normalized_evidence is required")
		}
		if severity := strings.TrimSpace(waiver.OriginalSeverity); severity == "" {
			validation.Errors = append(validation.Errors, label+".original_severity is required")
		} else if !ValidSeverity(severity) {
			validation.Errors = append(validation.Errors, label+".original_severity is invalid: "+severity)
		}
		if strings.TrimSpace(waiver.Justification) == "" {
			validation.Errors = append(validation.Errors, label+".justification is required")
		}
		if _, ok := parseBaselineDate(firstNonEmpty(waiver.DateAdded, waiver.Date)); !ok {
			validation.Errors = append(validation.Errors, label+".date_added or .date is required in YYYY-MM-DD form")
		}
		expiry, ok := parseBaselineDate(waiverExpiry(waiver))
		if !ok {
			validation.Errors = append(validation.Errors, label+".review_by, .expires, or .expiry is required in YYYY-MM-DD form")
		} else if baselineDateExpired(expiry, now) {
			validation.Expired = append(validation.Expired, waiver)
		}
	}
	return validation
}

func applyBaseline(repoPath string, cfg BaselineConfig, result *Result, now time.Time) {
	if strings.TrimSpace(cfg.Path) == "" {
		return
	}
	baseline, validation, err := LoadBaseline(repoPath, cfg.Path, now)
	if err != nil {
		addNeedsHuman(result, LayerSAST, err.Error())
		return
	}
	if len(validation.Errors) > 0 {
		for _, validationErr := range validation.Errors {
			addNeedsHuman(result, LayerSAST, "invalid audit baseline: "+validationErr)
		}
		return
	}

	expired := map[string]bool{}
	for _, waiver := range validation.Expired {
		expired[strings.TrimSpace(waiver.ID)] = true
		addNeedsHuman(result, LayerSAST, fmt.Sprintf("audit baseline waiver %q expired on %s", waiver.ID, waiverExpiry(waiver)))
	}

	matched := map[string]bool{}
	for findingIndex := range result.Findings {
		for _, waiver := range baseline.Waivers {
			if expired[strings.TrimSpace(waiver.ID)] {
				continue
			}
			if !waiverMatchesFinding(waiver, result.Findings[findingIndex]) {
				continue
			}
			result.Findings[findingIndex].Waived = true
			result.Findings[findingIndex].WaiverID = strings.TrimSpace(waiver.ID)
			matched[strings.TrimSpace(waiver.ID)] = true
			break
		}
	}

	for _, waiver := range baseline.Waivers {
		id := strings.TrimSpace(waiver.ID)
		if expired[id] || matched[id] {
			continue
		}
		result.BaselineNotices = append(result.BaselineNotices, BaselineNotice{
			ID:     id,
			Status: "stale",
			Reason: "waiver did not match any current finding",
		})
	}
}

func waiverMatchesFinding(waiver BaselineWaiver, finding Finding) bool {
	if waiverRule(waiver) != strings.TrimSpace(finding.Rule) {
		return false
	}
	path := waiverPath(waiver)
	pathGlob := waiverPathGlob(waiver)
	findingPath := normalizeRepoPath(finding.File)
	if path != "" && path != findingPath {
		return false
	}
	if pathGlob != "" && !matchRepoGlob(pathGlob, findingPath) {
		return false
	}
	if fingerprint := strings.TrimSpace(waiver.Fingerprint); fingerprint != "" && fingerprint == finding.Fingerprint {
		return true
	}
	if evidence := normalizeEvidence(waiver.NormalizedEvidence); evidence != "" && evidence == normalizeEvidence(finding.Evidence) {
		return true
	}
	return false
}

func waiverRule(waiver BaselineWaiver) string {
	return firstNonEmpty(waiver.Rule, waiver.MatchingRule)
}

func waiverPath(waiver BaselineWaiver) string {
	return normalizeRepoPath(waiver.Path)
}

func waiverPathGlob(waiver BaselineWaiver) string {
	return normalizeRepoPath(waiver.PathGlob)
}

func waiverExpiry(waiver BaselineWaiver) string {
	return firstNonEmpty(waiver.ReviewBy, waiver.Expires, waiver.Expiry)
}

func parseBaselineDate(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse("2006-01-02", raw)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}

func baselineDateExpired(expiry time.Time, now time.Time) bool {
	if now.IsZero() {
		now = time.Now()
	}
	today := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	return expiry.Before(today)
}
