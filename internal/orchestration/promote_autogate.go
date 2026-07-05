package orchestration

import (
	"context"
	"errors"

	"github.com/jasonhnd/loopcoder/internal/config"
	lcdefaults "github.com/jasonhnd/loopcoder/internal/defaults"
)

type PendingPromotionLedgerLoader func(repoPath, preProdBranch string) []TickPendingPromotion

type AutoGateResolverOptions struct {
	Writer                     PromotionWriter
	RepoPath                   string
	PreProdBranch              string
	RequiredChecks             []string
	ConfiguredEvidence         []config.EvidenceArtifact
	LoadPendingPromotionLedger PendingPromotionLedgerLoader
}

func ResolvePromoteAutoGate(ctx context.Context, opts AutoGateResolverOptions) (*AutoGateInputs, error) {
	if opts.Writer == nil {
		return nil, errors.New("promotion writer is required")
	}
	loadPending := opts.LoadPendingPromotionLedger
	if loadPending == nil {
		loadPending = loadTickPendingPromotionLedger
	}

	inputs := &AutoGateInputs{}

	if files, diff, err := opts.Writer.CompareBranches(ctx, lcdefaults.BaseBranch, opts.PreProdBranch); err == nil {
		inputs.RedLineClean = promoteBoolPtr(PromotionRedLinesClean(files, diff))
	}

	if checks, err := opts.Writer.BranchChecks(ctx, opts.PreProdBranch); err == nil {
		switch status, _ := preProdHealthStatus(normalizeRequiredChecks(opts.RequiredChecks), checks.Checks); status {
		case PreProdHealthStatusGreen:
			inputs.CIGreen = promoteBoolPtr(true)
		case PreProdHealthStatusRed:
			inputs.CIGreen = promoteBoolPtr(false)
		}
	}

	if pending := loadPending(opts.RepoPath, opts.PreProdBranch); len(pending) > 0 {
		inputs.VerdictPass = promoteBoolPtr(true)
	}

	inputs.EvidencePresent = promoteBoolPtr(len(normalizeConfiguredEvidence(opts.ConfiguredEvidence)) > 0)

	return inputs, nil
}

func promoteBoolPtr(value bool) *bool {
	return &value
}
