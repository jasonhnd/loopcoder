package artifactqual

import (
	"strings"
)

// ValidateIndependentVerifierFromChildren requires IndependentVerifier /
// VerifierProvider to derive from exactly one structured wi_verify child and
// one structured wi_implement child with distinct providers.
// ChildID substrings, ArgvDigest, and arbitrary sha256 refs never qualify.
func ValidateIndependentVerifierFromChildren(pr CanaryPR, children []CanaryChild) (ok bool, reasons []string) {
	add := func(s string) { reasons = append(reasons, s) }

	var verifyKids, implementKids []CanaryChild
	for _, c := range children {
		if !c.RealProviderExecuted || !strings.EqualFold(strings.TrimSpace(c.Terminal), "succeeded") {
			continue
		}
		wid := strings.TrimSpace(c.WorkItemID)
		cid := strings.TrimSpace(c.ChildID)
		// Exact work-item identity only; ChildID must equal WorkItemID.
		if wid == "" || cid == "" || cid != wid {
			continue
		}
		switch wid {
		case "wi_verify":
			verifyKids = append(verifyKids, c)
		case "wi_implement":
			implementKids = append(implementKids, c)
		}
	}

	if len(verifyKids) == 0 {
		add("verifier_no_succeeded_wi_verify_child")
		return false, reasons
	}
	if len(verifyKids) != 1 {
		add("verifier_wi_verify_not_unique")
		return false, reasons
	}
	if len(implementKids) == 0 {
		add("verifier_no_succeeded_wi_implement_child")
		return false, reasons
	}
	if len(implementKids) != 1 {
		add("verifier_wi_implement_not_unique")
		return false, reasons
	}

	v := verifyKids[0]
	imp := implementKids[0]

	if strings.TrimSpace(v.TaskClass) != "soul" {
		add("verifier_wi_verify_task_class_not_soul")
	}
	if strings.TrimSpace(imp.TaskClass) != "tera" {
		add("verifier_wi_implement_task_class_not_tera")
	}
	if strings.TrimSpace(v.Provider) == "" {
		add("verifier_provider_missing")
	}
	if strings.TrimSpace(v.AttemptID) == "" {
		add("verifier_attempt_missing")
	}
	if strings.TrimSpace(imp.Provider) == "" {
		add("implement_provider_missing")
	}
	if !isExactSHA256Digest(v.OutputEvidence) {
		add("verifier_output_evidence_invalid")
	}

	// PR binding must match verify child exactly.
	verProv := strings.TrimSpace(pr.VerifierProvider)
	verAtt := strings.TrimSpace(pr.VerifierAttemptID)
	verEv := strings.TrimSpace(pr.VerifierEvidenceRef)
	ind := strings.TrimSpace(pr.IndependentVerifier)
	if verProv == "" {
		add("pr_verifier_provider_missing")
	} else if !strings.EqualFold(verProv, strings.TrimSpace(v.Provider)) {
		add("pr_verifier_provider_mismatch")
	}
	if ind != "" && !strings.EqualFold(ind, strings.TrimSpace(v.Provider)) {
		add("pr_independent_verifier_mismatch")
	}
	if verAtt == "" {
		add("pr_verifier_attempt_missing")
	} else if verAtt != strings.TrimSpace(v.AttemptID) {
		add("pr_verifier_attempt_mismatch")
	}
	head := strings.TrimSpace(pr.HeadOID)
	wantEv := strings.TrimSpace(v.OutputEvidence) + "@head:" + head
	if verEv == "" {
		add("pr_verifier_evidence_missing")
	} else if strings.Contains(strings.ToLower(verEv), "pending") {
		add("pr_verifier_evidence_pending")
	} else if verEv != wantEv {
		add("pr_verifier_evidence_mismatch")
	}

	// Implement and verify providers must differ (unconditional).
	if strings.TrimSpace(v.Provider) != "" && strings.TrimSpace(imp.Provider) != "" &&
		strings.EqualFold(strings.TrimSpace(v.Provider), strings.TrimSpace(imp.Provider)) {
		add("verifier_implement_provider_same")
	}

	ok = len(reasons) == 0
	return ok, reasons
}

// isExactSHA256Digest is true for "sha256:" + exactly 64 hex digits.
func isExactSHA256Digest(s string) bool {
	s = strings.TrimSpace(s)
	const p = "sha256:"
	if !strings.HasPrefix(strings.ToLower(s), p) {
		return false
	}
	hexPart := s[len(p):]
	if len(hexPart) != 64 {
		return false
	}
	for i := 0; i < len(hexPart); i++ {
		c := hexPart[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}
