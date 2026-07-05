package audit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type baselineDocument struct {
	Waivers []baselineWaiver `yaml:"waivers"`
}

type baselineWaiver struct {
	ID                 string `yaml:"id"`
	Rule               string `yaml:"rule"`
	File               string `yaml:"file"`
	Path               string `yaml:"path"`
	PathGlob           string `yaml:"path_glob"`
	Fingerprint        string `yaml:"fingerprint"`
	NormalizedEvidence string `yaml:"normalized_evidence"`
	OriginalSeverity   string `yaml:"original_severity"`
	Justification      string `yaml:"justification"`
	DateAdded          string `yaml:"date_added"`
	ReviewBy           string `yaml:"review_by"`
	ExpiresAt          string `yaml:"expires_at"`
}

func applyBaseline(repoPath, rawPath string, result *Result) {
	rawPath = strings.TrimSpace(rawPath)
	if rawPath == "" || result == nil {
		return
	}
	path, err := safeRepoRelativePath(rawPath)
	if err != nil {
		addNeedsHuman(result, LayerSAST, fmt.Sprintf("audit.baseline.path %q is invalid: %v", rawPath, err))
		return
	}
	data, err := os.ReadFile(filepath.Join(repoPath, filepath.FromSlash(path)))
	if err != nil {
		addNeedsHuman(result, LayerSAST, fmt.Sprintf("read audit.baseline.path %s: %v", path, err))
		return
	}
	var baseline baselineDocument
	if err := yaml.Unmarshal(data, &baseline); err != nil {
		addNeedsHuman(result, LayerSAST, fmt.Sprintf("parse audit.baseline.path %s: %v", path, err))
		return
	}
	now := time.Now().UTC()
	matched := make([]bool, len(baseline.Waivers))
	for index, waiver := range baseline.Waivers {
		if err := validateBaselineWaiver(waiver, now); err != nil {
			addNeedsHuman(result, LayerSAST, fmt.Sprintf("audit baseline waiver %s is invalid: %v", baselineWaiverLabel(index, waiver), err))
			continue
		}
		for findingIndex := range result.Findings {
			if result.Findings[findingIndex].Waived || !baselineWaiverMatches(waiver, result.Findings[findingIndex]) {
				continue
			}
			if NormalizeSeverity(result.Findings[findingIndex].Severity) == SeverityCritical {
				addNeedsHuman(result, LayerSAST, fmt.Sprintf("critical finding %s requires human review; waiver %s did not suppress it", result.Findings[findingIndex].ID, strings.TrimSpace(waiver.ID)))
				matched[index] = true
				continue
			}
			result.Findings[findingIndex].Waived = true
			result.Findings[findingIndex].WaiverID = strings.TrimSpace(waiver.ID)
			matched[index] = true
		}
	}
	for index, waiver := range baseline.Waivers {
		if !matched[index] {
			addNeedsHuman(result, LayerSAST, fmt.Sprintf("audit baseline waiver %s is stale; no current finding matched", baselineWaiverLabel(index, waiver)))
		}
	}
}

func validateBaselineWaiver(waiver baselineWaiver, now time.Time) error {
	if strings.TrimSpace(waiver.ID) == "" {
		return fmt.Errorf("id is required")
	}
	if strings.TrimSpace(waiver.Rule) == "" {
		return fmt.Errorf("rule is required")
	}
	if strings.TrimSpace(waiver.File) == "" && strings.TrimSpace(waiver.Path) == "" && strings.TrimSpace(waiver.PathGlob) == "" {
		return fmt.Errorf("file, path, or path_glob is required")
	}
	if strings.TrimSpace(waiver.Fingerprint) == "" && strings.TrimSpace(waiver.NormalizedEvidence) == "" {
		return fmt.Errorf("fingerprint or normalized_evidence is required")
	}
	if !ValidSeverity(waiver.OriginalSeverity) {
		return fmt.Errorf("original_severity must be critical, high, medium, low, or info")
	}
	if strings.TrimSpace(waiver.Justification) == "" {
		return fmt.Errorf("justification is required")
	}
	if _, ok := parseBaselineDate(waiver.DateAdded); !ok {
		return fmt.Errorf("date_added must use YYYY-MM-DD")
	}
	deadlineRaw := firstNonEmpty(waiver.ReviewBy, waiver.ExpiresAt)
	deadline, ok := parseBaselineDate(deadlineRaw)
	if !ok {
		return fmt.Errorf("review_by or expires_at must use YYYY-MM-DD")
	}
	if !deadline.After(now) {
		return fmt.Errorf("waiver expired on %s", deadline.Format("2006-01-02"))
	}
	return nil
}

func baselineWaiverMatches(waiver baselineWaiver, finding Finding) bool {
	if strings.TrimSpace(waiver.Rule) != strings.TrimSpace(finding.Rule) {
		return false
	}
	if fingerprint := strings.TrimSpace(waiver.Fingerprint); fingerprint != "" && fingerprint != strings.TrimSpace(finding.Fingerprint) {
		return false
	}
	if evidence := strings.TrimSpace(waiver.NormalizedEvidence); evidence != "" && evidence != normalizeEvidence(finding.Evidence) {
		return false
	}
	return baselinePathMatches(waiver, finding.File)
}

func baselinePathMatches(waiver baselineWaiver, file string) bool {
	file = normalizeRepoPath(file)
	for _, exact := range []string{waiver.File, waiver.Path} {
		if strings.TrimSpace(exact) != "" && normalizeRepoPath(exact) == file {
			return true
		}
	}
	if glob := strings.TrimSpace(waiver.PathGlob); glob != "" {
		return matchesAnyRepoGlob(file, []string{glob})
	}
	return false
}

func baselineWaiverLabel(index int, waiver baselineWaiver) string {
	if id := strings.TrimSpace(waiver.ID); id != "" {
		return id
	}
	return fmt.Sprintf("waivers[%d]", index)
}

func parseBaselineDate(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse("2006-01-02", value)
	return parsed, err == nil
}
