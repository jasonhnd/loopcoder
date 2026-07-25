package artifactqual

import (
	"context"
	"strings"
)

// releaseActionsEvidence is the resolved dual-green + RC Actions material for a qualify pass.
// ModeRelease always fetches; ModeUnit may reuse injected caller fields.
type releaseActionsEvidence struct {
	IntegrationReceipt *PreProdActionsReceipt
	RCBinding          *RCActionsBinding
	Reasons            []string
}

// resolveReleaseActions resolves pre-prod dual-green and RC Actions evidence for Input.
// ModeUnit returns injected IntegrationReceipt / RCActionsBinding unchanged.
// ModeRelease never reads those caller fields as authority; it fetches by IDs only.
// Reasons are stable IDs only — never verifier error text.
func resolveReleaseActions(ctx context.Context, in Input) releaseActionsEvidence {
	var out releaseActionsEvidence
	if ctx == nil {
		ctx = context.Background()
	}

	if in.Mode != ModeRelease {
		// ModeUnit (and any non-release): preserve injected caller values.
		out.IntegrationReceipt = in.IntegrationReceipt
		out.RCBinding = in.RCActionsBinding
		return out
	}

	repo := strings.TrimSpace(in.Repository)
	if repo == "" {
		out.Reasons = append(out.Reasons, "release_actions_repository_missing")
	}

	// Pre-prod dual-green: fetch only; never use in.IntegrationReceipt.
	if in.IntegrationVerifier == nil {
		out.Reasons = append(out.Reasons, "preprod_actions_verifier_missing")
	} else if in.IntegrationRunID <= 0 || in.IntegrationRunAttempt < 1 {
		out.Reasons = append(out.Reasons, "preprod_actions_identity_missing")
	} else if repo != "" {
		got, err := in.IntegrationVerifier.FetchRun(ctx, repo, in.IntegrationRunID, in.IntegrationRunAttempt)
		if err != nil {
			out.Reasons = append(out.Reasons, "preprod_actions_fetch_failed")
		} else {
			cp := got
			out.IntegrationReceipt = &cp
		}
	}

	// RC binding: fetch only; never use in.RCActionsBinding.
	if in.RCActionsVerifier == nil {
		out.Reasons = append(out.Reasons, "rc_actions_verifier_missing")
	} else if in.RCRunID <= 0 || in.RCArtifactID <= 0 {
		out.Reasons = append(out.Reasons, "rc_actions_identity_missing")
	} else if repo != "" {
		got, err := in.RCActionsVerifier.FetchRCBinding(ctx, repo, in.RCRunID, in.RCArtifactID)
		if err != nil {
			out.Reasons = append(out.Reasons, "rc_actions_fetch_failed")
		} else {
			cp := got
			out.RCBinding = &cp
		}
	}

	return out
}
