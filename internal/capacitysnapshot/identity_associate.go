package capacitysnapshot

import (
	"sort"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/providerinventory"
)

// associateIdentityEvidence repairs production identity splits without inventing
// capacity or cross-account joins:
//
//  1. Path aliases: installations that share adapter + exact ResolvedPathHash are
//     rewritten to one canonical ProviderInstallationID so install/auth/models and
//     exact/fresh quota that landed on different path aliases can recombine.
//     Never fuses solely by adapter, map iteration order, or across distinct
//     resolved binaries. Empty ResolvedPathHash ⇒ no alias fuse for that install.
//
//  2. Empty-account reassociation: capacity windows (or other evidence) on
//     provider|""|install are merged into the sole authenticated account for that
//     same (provider, install) when exactly one non-empty AccountRef is present.
//     Two or more distinct accounts for the same install ⇒ leave empty-account
//     rows separate (fail closed; never cross-account join).
//
// Sentinel scope tokens such as account:unknown must already be rejected before
// this step (see accountSegmentFromScope); they must not create a fake AccountRef.
func associateIdentityEvidence(accounts []AccountObservation, installs []providerinventory.ProviderInstallation) []AccountObservation {
	if len(accounts) == 0 {
		return accounts
	}
	alias := canonicalInstallByAlias(installs)
	// Rewrite install refs through alias map, then re-bucket and merge collisions.
	type bucket struct {
		obs AccountObservation
	}
	by := map[string]*bucket{}
	keyOf := func(provider, account, install string) string {
		return strings.ToLower(strings.TrimSpace(provider)) + "|" +
			strings.TrimSpace(account) + "|" + strings.TrimSpace(install)
	}
	for _, a := range accounts {
		a.Provider = strings.ToLower(strings.TrimSpace(a.Provider))
		a.AccountRef = strings.TrimSpace(a.AccountRef)
		a.InstallRef = strings.TrimSpace(a.InstallRef)
		if canon, ok := alias[a.InstallRef]; ok && canon != "" {
			if a.InstallRef != canon {
				a.Provenance = strings.TrimSpace(a.Provenance +
					"; install_alias_canonical=" + canon + " from=" + a.InstallRef)
				a.InstallRef = canon
			}
		}
		k := keyOf(a.Provider, a.AccountRef, a.InstallRef)
		if existing, ok := by[k]; ok {
			existing.obs = mergeAccountObservations(existing.obs, a)
			continue
		}
		cp := a
		by[k] = &bucket{obs: cp}
	}

	// Empty-account reassociation per (provider, install).
	type pi struct{ provider, install string }
	accountsByPI := map[pi][]string{} // non-empty account refs
	for k, b := range by {
		_ = k
		if strings.TrimSpace(b.obs.AccountRef) == "" {
			continue
		}
		p := pi{b.obs.Provider, b.obs.InstallRef}
		accountsByPI[p] = appendUniqueSorted(accountsByPI[p], b.obs.AccountRef)
	}
	var emptyKeys []string
	for k, b := range by {
		if strings.TrimSpace(b.obs.AccountRef) == "" {
			emptyKeys = append(emptyKeys, k)
		}
	}
	sort.Strings(emptyKeys)
	for _, ek := range emptyKeys {
		b := by[ek]
		if b == nil {
			continue
		}
		p := pi{b.obs.Provider, b.obs.InstallRef}
		accs := accountsByPI[p]
		if len(accs) != 1 {
			// 0: no account to bind; 2+: ambiguous — leave empty group unmerged.
			if len(accs) > 1 {
				b.obs.Provenance = strings.TrimSpace(b.obs.Provenance +
					"; identity_associate=ambiguous_multi_account_same_install")
			}
			continue
		}
		targetKey := keyOf(b.obs.Provider, accs[0], b.obs.InstallRef)
		target, ok := by[targetKey]
		if !ok {
			// Promote empty row onto the sole account (deterministic).
			b.obs.AccountRef = accs[0]
			b.obs.Provenance = strings.TrimSpace(b.obs.Provenance +
				"; identity_associate=empty_account_bound_sole_install_account")
			delete(by, ek)
			by[targetKey] = b
			continue
		}
		merged := mergeAccountObservations(target.obs, b.obs)
		merged.AccountRef = accs[0]
		merged.Provenance = strings.TrimSpace(merged.Provenance +
			"; identity_associate=empty_account_merged_sole_install_account")
		target.obs = merged
		delete(by, ek)
	}

	keys := make([]string, 0, len(by))
	for k := range by {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]AccountObservation, 0, len(keys))
	for _, k := range keys {
		out = append(out, by[k].obs)
	}
	return out
}

// canonicalInstallByAlias maps each ProviderInstallationID to a canonical id
// among installs that share (adapter, exact ResolvedPathHash). Installs without
// ResolvedPathHash map to themselves only (no alias fuse).
func canonicalInstallByAlias(installs []providerinventory.ProviderInstallation) map[string]string {
	type meta struct {
		id       string
		adapter  string
		resolved string
		rank     int // lower is better
	}
	// Group by adapter|resolvedHash.
	groups := map[string][]meta{}
	out := map[string]string{}
	for _, inst := range installs {
		id := strings.TrimSpace(inst.ProviderInstallationID)
		if id == "" {
			continue
		}
		adapter := strings.ToLower(strings.TrimSpace(inst.AdapterID))
		resolved := strings.TrimSpace(inst.ExecutableIdentity.ResolvedPathHash)
		if resolved == "" {
			// No exact resolved identity — refuse alias fusion for this install.
			out[id] = id
			continue
		}
		rank := 100
		if inst.InstallationState == providerinventory.InstallationInstalled {
			rank -= 40
		}
		if inst.UsableForInvocation == "yes" || inst.UsableForInvocation == "true" {
			rank -= 20
		}
		if inst.Confidence == providerinventory.ConfidenceExact {
			rank -= 5
		}
		gkey := adapter + "|" + resolved
		groups[gkey] = append(groups[gkey], meta{id: id, adapter: adapter, resolved: resolved, rank: rank})
	}
	for gkey, members := range groups {
		_ = gkey
		if len(members) == 0 {
			continue
		}
		// Deterministic: best rank, then lexicographic install id.
		sort.SliceStable(members, func(i, j int) bool {
			if members[i].rank != members[j].rank {
				return members[i].rank < members[j].rank
			}
			return members[i].id < members[j].id
		})
		canon := members[0].id
		for _, m := range members {
			out[m.id] = canon
		}
	}
	return out
}

// mergeAccountObservations unions evidence from a and b. Does not invent windows
// or models; ORs install/auth/health flags; concatenates windows/models with
// deterministic model de-dupe by ModelID.
func mergeAccountObservations(a, b AccountObservation) AccountObservation {
	out := a
	if out.Provider == "" {
		out.Provider = b.Provider
	}
	if out.AccountRef == "" {
		out.AccountRef = b.AccountRef
	}
	if out.InstallRef == "" {
		out.InstallRef = b.InstallRef
	}
	out.Installed = out.Installed || b.Installed
	out.Authenticated = out.Authenticated || b.Authenticated
	out.Healthy = out.Healthy || b.Healthy
	out.RateLimited = out.RateLimited || b.RateLimited
	out.CooldownActive = out.CooldownActive || b.CooldownActive
	// Prefer fresher/higher confidence health when a is unknown.
	if out.HealthFreshness == FreshnessUnknown && b.HealthFreshness != FreshnessUnknown {
		out.HealthFreshness = b.HealthFreshness
	}
	if out.HealthFreshness == FreshnessStale && b.HealthFreshness == FreshnessFresh {
		out.HealthFreshness = FreshnessFresh
	}
	if out.HealthConfidence == ConfidenceUnknown && b.HealthConfidence != ConfidenceUnknown {
		out.HealthConfidence = b.HealthConfidence
	}
	if out.HealthConfidence == ConfidenceEstimated && b.HealthConfidence == ConfidenceExact {
		out.HealthConfidence = ConfidenceExact
	}
	out.Windows = append(append([]Window(nil), out.Windows...), b.Windows...)
	// Model merge: prefer non-hint, union depths.
	modelIdx := map[string]int{}
	models := make([]ModelEntry, 0, len(out.Models)+len(b.Models))
	addModel := func(m ModelEntry) {
		id := strings.TrimSpace(m.ModelID)
		if id == "" {
			return
		}
		if i, ok := modelIdx[strings.ToLower(id)]; ok {
			cur := models[i]
			if cur.CatalogHintOnly && !m.CatalogHintOnly {
				cur.CatalogHintOnly = false
				cur.PresentInCatalog = m.PresentInCatalog
			}
			if !cur.PresentInCatalog && m.PresentInCatalog {
				cur.PresentInCatalog = true
			}
			// Union depths
			seen := map[string]bool{}
			var depths []string
			for _, d := range append(append([]string(nil), cur.SupportedDepths...), m.SupportedDepths...) {
				d = strings.ToLower(strings.TrimSpace(d))
				if d == "" || seen[d] {
					continue
				}
				seen[d] = true
				depths = append(depths, d)
			}
			sort.Strings(depths)
			cur.SupportedDepths = depths
			if cur.DefaultDepth == "" {
				cur.DefaultDepth = m.DefaultDepth
			}
			models[i] = cur
			return
		}
		modelIdx[strings.ToLower(id)] = len(models)
		models = append(models, m)
	}
	for _, m := range out.Models {
		addModel(m)
	}
	for _, m := range b.Models {
		addModel(m)
	}
	out.Models = models
	if out.Source == "" {
		out.Source = b.Source
	}
	if out.Provenance == "" {
		out.Provenance = b.Provenance
	} else if b.Provenance != "" && !strings.Contains(out.Provenance, b.Provenance) {
		out.Provenance = strings.TrimSpace(out.Provenance + "; " + b.Provenance)
	}
	if out.CapturedAt.IsZero() {
		out.CapturedAt = b.CapturedAt
	}
	return out
}
