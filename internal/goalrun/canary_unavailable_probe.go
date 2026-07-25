package goalrun

import (
	"fmt"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/models"
	"github.com/jasonhnd/loopcoder/internal/routedecision"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

func validateCanaryUnavailableProbeRequest(req Request) error {
	provider := req.CanaryUnavailableProbeProvider
	model := req.CanaryUnavailableProbeModel
	if provider == "" && model == "" {
		return nil
	}
	if provider == "" || model == "" ||
		provider != strings.TrimSpace(provider) ||
		model != strings.TrimSpace(model) {
		return fmt.Errorf("goalrun: canary unavailable probe requires exact provider+model")
	}
	if req.CanaryEmit == nil || strings.TrimSpace(req.CanaryEmit.OutPath) == "" {
		return fmt.Errorf("goalrun: canary unavailable probe requires exact canary evidence emission")
	}
	if strings.TrimSpace(req.Provider) != "" || strings.TrimSpace(req.Model) != "" {
		return fmt.Errorf("goalrun: canary unavailable probe requires normal auto-route")
	}
	if req.DryRun != nil && *req.DryRun {
		return fmt.Errorf("goalrun: canary unavailable probe cannot run in dry-run")
	}
	if _, ok := declaredModel(provider, model); !ok {
		return fmt.Errorf("goalrun: canary unavailable probe model is not adapter-declared")
	}
	return nil
}

func declaredModel(provider, requested string) (models.Model, bool) {
	p, ok := models.LookupProvider(provider)
	if !ok {
		return models.Model{}, false
	}
	for _, model := range p.Models {
		if requested == model.Name {
			return model, true
		}
		for _, alias := range model.Aliases {
			if requested == alias {
				return model, true
			}
		}
	}
	return models.Model{}, false
}

func declaredModelSupports(provider, requested, depth string) bool {
	model, ok := declaredModel(provider, requested)
	if !ok {
		return false
	}
	want := normalizeDepth(depth)
	for _, supported := range model.Depths {
		if normalizeDepth(supported.Token) == want {
			return true
		}
	}
	return false
}

func decisionHasHardEligibleRoute(
	decision *routedecision.Decision,
	provider, model, depth, permission string,
) bool {
	if decision == nil {
		return false
	}
	for _, candidate := range decision.Candidates {
		if candidate.Provider == provider &&
			candidate.Model == model &&
			normalizeDepth(candidate.Effort) == normalizeDepth(depth) &&
			normalizePerm(candidate.Permission) == normalizePerm(permission) &&
			candidate.HardEligible && !candidate.SoftExcluded {
			return true
		}
	}
	return false
}

func prependAlternateUnique(
	alternates []workflowrun.AlternateCandidate,
	preferred workflowrun.AlternateCandidate,
) []workflowrun.AlternateCandidate {
	out := []workflowrun.AlternateCandidate{preferred}
	for _, candidate := range alternates {
		if candidate.Provider == preferred.Provider &&
			candidate.Model == preferred.Model &&
			normalizeDepth(candidate.Effort) == normalizeDepth(preferred.Effort) &&
			normalizePerm(candidate.Permission) == normalizePerm(preferred.Permission) &&
			candidate.AccountRef == preferred.AccountRef &&
			candidate.InstallRef == preferred.InstallRef &&
			candidate.WindowKind == preferred.WindowKind {
			continue
		}
		out = append(out, candidate)
	}
	return out
}
