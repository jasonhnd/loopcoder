package capacitysnapshot

import (
	"sort"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/providerinventory"
)

// associateIdentityEvidence repairs production identity splits without inventing
// capacity or cross-account joins:
//
//  1. Path aliases: only DiscoverySource==path installs that share adapter +
//     exact+fresh ResolvedPathHash may fuse. The canonical install is the actual
//     LookPath hit for the runner command (earliest PATH DiscoveryOrder for that
//     adapter+executable name), and only if that hit is exact/fresh/installed/
//     usable. If the LookPath-first hit fails eligibility, the whole group fails
//     closed — never skip to a later usable PATH alias. Explicit-config rows
//     never fuse, never donate auth/health/models/quota, and stay self-mapped.
//     Distinct ResolvedPathHash values never fuse.
//
//  2. Empty-account reassociation: capacity windows (or other evidence) on
//     provider|""|install are merged into the sole Authenticated account for that
//     same (provider, install) when exactly one Authenticated AccountRef is present.
//     Zero or multiple authenticated accounts ⇒ leave empty-account rows separate
//     (fail closed; never cross-account join).
//
// Sentinel scope tokens such as account:unknown must already be rejected before
// this step (see accountSegmentFromScope); they must not create a fake AccountRef.
//
// Note: RC36 live Discover may only list path-alias A while durable quota
// references path-alias B of the same resolved binary. Rehydrate translates
// durable B→LookPath-primary live only with path provenance.
func associateIdentityEvidence(accounts []AccountObservation, installs []providerinventory.ProviderInstallation) []AccountObservation {
	if len(accounts) == 0 {
		return accounts
	}
	alias := computePathInstallPlan(installs).Alias
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

	// Empty-account reassociation per (provider, install) only.
	//
	// Empty-account quota may bind only to the sole Authenticated (Ready)
	// AccountRef on that same canonical install. Multiple ready accounts or zero
	// stay unmerged (fail closed).
	//
	// Nonempty conflicting AccountRef values must NOT be overwritten or treated as
	// equivalent — leave both rows and fail closed. Codex AuthReadiness now stamps
	// shared codexauth acct-+64hex (same as agent preflight); quota stays empty-account
	// and rebinds only to the sole Ready account on the install.
	type pi struct{ provider, install string }
	authAccountsByPI := map[pi][]string{}
	for _, b := range by {
		if strings.TrimSpace(b.obs.AccountRef) == "" {
			continue
		}
		if !b.obs.Authenticated {
			continue
		}
		p := pi{b.obs.Provider, b.obs.InstallRef}
		authAccountsByPI[p] = appendUniqueSorted(authAccountsByPI[p], b.obs.AccountRef)
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
		accs := authAccountsByPI[p]
		if len(accs) != 1 {
			if len(accs) > 1 {
				b.obs.Provenance = strings.TrimSpace(b.obs.Provenance +
					"; identity_associate=ambiguous_multi_authenticated_account_same_install")
			}
			continue
		}
		targetKey := keyOf(b.obs.Provider, accs[0], b.obs.InstallRef)
		target, ok := by[targetKey]
		if !ok {
			b.obs.AccountRef = accs[0]
			b.obs.Provenance = strings.TrimSpace(b.obs.Provenance +
				"; identity_associate=empty_account_bound_sole_authenticated_install_account")
			delete(by, ek)
			by[targetKey] = b
			continue
		}
		merged := mergeAccountObservations(target.obs, b.obs)
		merged.AccountRef = accs[0]
		merged.Provenance = strings.TrimSpace(merged.Provenance +
			"; identity_associate=empty_account_merged_sole_authenticated_install_account")
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

// installCommandKey binds inventory rows to the runner command LookPath uses
// (adapter + executable basename), not merely resolved-hash groups.
func installCommandKey(inst providerinventory.ProviderInstallation) string {
	adapter := strings.ToLower(strings.TrimSpace(inst.AdapterID))
	exe := strings.ToLower(strings.TrimSpace(inst.ExecutableName))
	if exe == "" {
		exe = strings.ToLower(strings.TrimSpace(inst.ExecutableIdentity.Basename))
	}
	return adapter + "|" + exe
}

func installUsableYes(inst providerinventory.ProviderInstallation) bool {
	u := strings.ToLower(strings.TrimSpace(inst.UsableForInvocation))
	return u == "yes" || u == "true"
}

// lookPathPrimaryEligible is true when the actual LookPath-first PATH hit is
// production-invocable: exact+fresh identity, installed, usable.
func lookPathPrimaryEligible(inst providerinventory.ProviderInstallation) bool {
	if !exactFreshInstallForAlias(inst) {
		return false
	}
	if inst.DiscoverySource != providerinventory.DiscoveryPath {
		return false
	}
	if inst.InstallationState != providerinventory.InstallationInstalled {
		return false
	}
	return installUsableYes(inst)
}

// pathInstallPlan is the authoritative PATH LookPath canonicalization result.
// Alias rewrites only PATH→PATH same (command, resolved). ProductionEligible
// contains only the true LookPath-primary install IDs that may contribute
// Installed/Healthy/Authenticated for unattended routing. Explicit-config and
// later PATH rows when the first hit fails are never production-eligible.
type pathInstallPlan struct {
	Alias               map[string]string // installID → canonical (self if no fuse)
	ProductionEligible  map[string]bool   // LookPath-primary IDs only when eligible
	LookPathPrimaryByCmd map[string]string // commandKey → primary install id (even if ineligible)
}

// computePathInstallPlan builds alias fuse + production eligibility for PATH installs.
func computePathInstallPlan(installs []providerinventory.ProviderInstallation) pathInstallPlan {
	type pathHit struct {
		id       string
		order    int
		resolved string
		cmd      string
		inst     providerinventory.ProviderInstallation
	}
	plan := pathInstallPlan{
		Alias:                map[string]string{},
		ProductionEligible:   map[string]bool{},
		LookPathPrimaryByCmd: map[string]string{},
	}
	pathByCmd := map[string][]pathHit{}
	// exact+fresh PATH only, keyed by command|resolved (never adapter|resolved alone).
	pathExactByCmdResolved := map[string][]string{}
	idToCmd := map[string]string{}

	for _, inst := range installs {
		id := strings.TrimSpace(inst.ProviderInstallationID)
		if id == "" {
			continue
		}
		plan.Alias[id] = id
		if inst.DiscoverySource != providerinventory.DiscoveryPath {
			continue
		}
		ck := installCommandKey(inst)
		idToCmd[id] = ck
		resolved := strings.TrimSpace(inst.ExecutableIdentity.ResolvedPathHash)
		pathByCmd[ck] = append(pathByCmd[ck], pathHit{
			id: id, order: inst.DiscoveryOrder, resolved: resolved, cmd: ck, inst: inst,
		})
		if exactFreshInstallForAlias(inst) && resolved != "" {
			gkey := ck + "|" + resolved
			pathExactByCmdResolved[gkey] = append(pathExactByCmdResolved[gkey], id)
		}
	}

	for ck, hits := range pathByCmd {
		sort.SliceStable(hits, func(i, j int) bool {
			if hits[i].order != hits[j].order {
				return hits[i].order < hits[j].order
			}
			return hits[i].id < hits[j].id
		})
		first := hits[0]
		plan.LookPathPrimaryByCmd[ck] = first.id
		// Eligibility checked AFTER selecting actual LookPath-first hit.
		if !lookPathPrimaryEligible(first.inst) {
			// Fail closed for this command: no fuse, no production eligibility
			// for first or later PATH rows.
			continue
		}
		if first.resolved == "" {
			continue
		}
		plan.ProductionEligible[first.id] = true
		gkey := ck + "|" + first.resolved
		for _, id := range pathExactByCmdResolved[gkey] {
			// Only same command key (enforced by gkey prefix).
			if idToCmd[id] != ck {
				continue
			}
			plan.Alias[id] = first.id
		}
	}
	return plan
}

// canonicalInstallByAlias is the PATH-only identity fuse map from computePathInstallPlan.
func canonicalInstallByAlias(installs []providerinventory.ProviderInstallation) map[string]string {
	return computePathInstallPlan(installs).Alias
}

// productionInstallEligible reports whether installID may complete unattended
// production routing (LookPath primary only).
func productionInstallEligible(plan pathInstallPlan, installID string) bool {
	id := strings.TrimSpace(installID)
	if id == "" {
		return false
	}
	if plan.ProductionEligible[id] {
		return true
	}
	// Path aliases that rewrite to an eligible primary are not independently
	// production-eligible as their own install ID; after associate they share
	// the primary InstallRef. During FromProviderInventoryReport, auth/usable
	// on a path alias of an eligible primary may apply only after rewrite —
	// allow contribution when alias target is production-eligible AND the
	// source will fuse (path alias), not when self-mapped non-primary.
	if canon := plan.Alias[id]; canon != "" && canon != id && plan.ProductionEligible[canon] {
		return true // path alias of eligible primary — identity repair only
	}
	return false
}

// exactFreshInstallForAlias mirrors providerinventory.exactFreshInstallIdentity:
// exact confidence + fresh + nonempty ResolvedPathHash.
func exactFreshInstallForAlias(inst providerinventory.ProviderInstallation) bool {
	if strings.TrimSpace(inst.ProviderInstallationID) == "" {
		return false
	}
	if strings.TrimSpace(inst.AdapterID) == "" {
		return false
	}
	if inst.Confidence != providerinventory.ConfidenceExact {
		return false
	}
	if inst.FreshnessState != providerinventory.FreshnessFresh {
		return false
	}
	if strings.TrimSpace(inst.ExecutableIdentity.ResolvedPathHash) == "" {
		return false
	}
	return true
}

// mergeAccountObservations unions evidence from a and b. Does not invent windows
// or models; ORs install/auth/health flags; concatenates windows/models with
// deterministic model de-dupe by ModelID. Supported depths never broaden on
// disagreement: intersection when both rows declare depths; empty when
// conflict leaves no common depth; a single authoritative exact non-hint row
// may supply depths when the other has none.
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
	// Model merge: prefer non-hint; depths = intersection (fail closed).
	modelIdx := map[string]int{}
	models := make([]ModelEntry, 0, len(out.Models)+len(b.Models))
	addModel := func(m ModelEntry) {
		id := strings.TrimSpace(m.ModelID)
		if id == "" {
			return
		}
		if i, ok := modelIdx[strings.ToLower(id)]; ok {
			cur := models[i]
			// Prefer production-routable (non-hint) truth as authoritative base.
			if cur.CatalogHintOnly && !m.CatalogHintOnly {
				// Promote m as base but re-intersect depths with cur if cur had depths.
				prevDepths := cur.SupportedDepths
				cur = m
				cur.SupportedDepths = mergeDepthsFailClosed(prevDepths, m.SupportedDepths)
			} else if !cur.CatalogHintOnly && m.CatalogHintOnly {
				// Keep cur; ignore hint-only depths that would invent ladder.
			} else {
				// Same authority class: intersect depths (never broaden).
				cur.SupportedDepths = mergeDepthsFailClosed(cur.SupportedDepths, m.SupportedDepths)
				if !cur.PresentInCatalog && m.PresentInCatalog {
					cur.PresentInCatalog = true
				}
				if cur.DefaultDepth == "" {
					cur.DefaultDepth = m.DefaultDepth
				} else if m.DefaultDepth != "" && !strings.EqualFold(cur.DefaultDepth, m.DefaultDepth) {
					// Conflicting defaults: keep only if still in intersected depths.
					if !depthIn(cur.DefaultDepth, cur.SupportedDepths) {
						if depthIn(m.DefaultDepth, cur.SupportedDepths) {
							cur.DefaultDepth = m.DefaultDepth
						} else {
							cur.DefaultDepth = ""
						}
					}
				}
			}
			// Ensure default is a member of supported when present.
			if cur.DefaultDepth != "" && len(cur.SupportedDepths) > 0 && !depthIn(cur.DefaultDepth, cur.SupportedDepths) {
				cur.DefaultDepth = ""
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

// mergeDepthsFailClosed: both empty → empty; one empty → keep the other
// (authoritative sole row); both nonempty → intersection only (never union).
func mergeDepthsFailClosed(a, b []string) []string {
	na := normalizeDepthList(a)
	nb := normalizeDepthList(b)
	if len(na) == 0 {
		return nb
	}
	if len(nb) == 0 {
		return na
	}
	setB := map[string]bool{}
	for _, d := range nb {
		setB[d] = true
	}
	var out []string
	for _, d := range na {
		if setB[d] {
			out = append(out, d)
		}
	}
	return out
}

func normalizeDepthList(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range in {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

func depthIn(d string, list []string) bool {
	d = strings.ToLower(strings.TrimSpace(d))
	for _, x := range list {
		if x == d {
			return true
		}
	}
	return false
}
