package capacitysnapshot

import (
	"testing"

	"github.com/jasonhnd/loopcoder/internal/providerinventory"
)

func pathInstall(adapter, exe, id, resolved, pathHash string, order int) providerinventory.ProviderInstallation {
	return providerinventory.ProviderInstallation{
		AdapterID: adapter, ProviderInstallationID: id, ExecutableName: exe,
		DiscoverySource: providerinventory.DiscoveryPath, DiscoveryOrder: order,
		InstallationState: providerinventory.InstallationInstalled, UsableForInvocation: "yes",
		FreshnessState: providerinventory.FreshnessFresh, Confidence: providerinventory.ConfidenceExact,
		ExecutableIdentity: providerinventory.ExecutableIdentity{
			Basename: exe, ResolvedPathHash: resolved, PathHash: pathHash,
		},
	}
}

func TestCanonicalInstallByAlias_LookPathPrimaryNotLaterEligible(t *testing.T) {
	const (
		// Lexico-first secondary id must not win over PATH order 0.
		secondary = "pinst_3an5v55kgyq352a2bbgkfljbmikrndoq"
		primary   = "pinst_wrpmecvyfayff7nnqvaztkqhfs7ua2hd"
		rhash     = "sha256:same-resolved-binary"
	)
	installs := []providerinventory.ProviderInstallation{
		pathInstall("grok", "grok", secondary, rhash, "sha256:path-secondary", 1),
		pathInstall("grok", "grok", primary, rhash, "sha256:path-primary", 0),
	}
	alias := canonicalInstallByAlias(installs)
	if alias[primary] != primary {
		t.Fatalf("primary must map to self, got %q", alias[primary])
	}
	if alias[secondary] != primary {
		t.Fatalf("secondary PATH alias must map to LookPath primary %s, got %q", primary, alias[secondary])
	}
}

// PATH hit 0 unusable + hit 1 usable → no routable fallback (fail closed).
func TestCanonicalInstallByAlias_UnusableLookPathFirstNoFallback(t *testing.T) {
	const (
		first  = "pinst_path_hit0_unusable"
		second = "pinst_path_hit1_usable"
		rhash  = "sha256:same"
	)
	p0 := pathInstall("grok", "grok", first, rhash, "sha256:p0", 0)
	p0.UsableForInvocation = "unknown"
	p1 := pathInstall("grok", "grok", second, rhash, "sha256:p1", 1)
	alias := canonicalInstallByAlias([]providerinventory.ProviderInstallation{p0, p1})
	if alias[first] != first || alias[second] != second {
		t.Fatalf("unusable LookPath-first must fail closed (no later fallback): %v", alias)
	}
}

func TestCanonicalInstallByAlias_ExplicitNeverFusesOrBecomesPrimary(t *testing.T) {
	const (
		pathID     = "pinst_path_lookpath"
		explicitID = "pinst_explicit_first_order"
		rhash      = "sha256:same"
	)
	explicit := pathInstall("codex", "codex", explicitID, rhash, "sha256:e", 0)
	explicit.DiscoverySource = providerinventory.DiscoveryExplicitConfig
	path := pathInstall("codex", "codex", pathID, rhash, "sha256:p", 5)
	alias := canonicalInstallByAlias([]providerinventory.ProviderInstallation{explicit, path})
	if alias[pathID] != pathID {
		t.Fatalf("PATH install must stay primary self, got %q", alias[pathID])
	}
	// Explicit must remain self — never fuse into PATH AccountObservation.
	if alias[explicitID] != explicitID {
		t.Fatalf("explicit must not fuse into PATH primary, got %q", alias[explicitID])
	}
}

func TestCanonicalInstallByAlias_SoleExplicitNoFuse(t *testing.T) {
	const (
		a     = "pinst_explicit_a"
		b     = "pinst_explicit_b"
		rhash = "sha256:same"
	)
	ea := pathInstall("grok", "grok", a, rhash, "sha256:a", 0)
	ea.DiscoverySource = providerinventory.DiscoveryExplicitConfig
	eb := pathInstall("grok", "grok", b, rhash, "sha256:b", 1)
	eb.DiscoverySource = providerinventory.DiscoveryExplicitConfig
	alias := canonicalInstallByAlias([]providerinventory.ProviderInstallation{ea, eb})
	if alias[a] != a || alias[b] != b {
		t.Fatalf("explicit-only must not fuse: %v", alias)
	}
}

func TestCanonicalInstallByAlias_PATHOrderFlipConsistent(t *testing.T) {
	const (
		a     = "pinst_a_first_on_path"
		b     = "pinst_b_second_on_path"
		rhash = "sha256:same"
	)
	m1 := canonicalInstallByAlias([]providerinventory.ProviderInstallation{
		pathInstall("codex", "codex", a, rhash, "sha256:a", 0),
		pathInstall("codex", "codex", b, rhash, "sha256:b", 1),
	})
	if m1[a] != a || m1[b] != a {
		t.Fatalf("PATH a,b: want primary a; map=%v", m1)
	}
	m2 := canonicalInstallByAlias([]providerinventory.ProviderInstallation{
		pathInstall("codex", "codex", b, rhash, "sha256:b", 0),
		pathInstall("codex", "codex", a, rhash, "sha256:a", 1),
	})
	if m2[b] != b || m2[a] != b {
		t.Fatalf("PATH b,a: want primary b; map=%v", m2)
	}
}

func TestCanonicalInstallByAlias_DistinctBinariesRemainDistinct(t *testing.T) {
	const (
		a = "pinst_bin_a"
		b = "pinst_bin_b"
	)
	alias := canonicalInstallByAlias([]providerinventory.ProviderInstallation{
		pathInstall("grok", "grok", a, "sha256:binary-a", "sha256:pa", 0),
		pathInstall("grok", "grok", b, "sha256:binary-b", "sha256:pb", 1),
	})
	// LookPath-first is a; b has different resolved hash so stays self (not fused to a).
	if alias[a] != a {
		t.Fatalf("LookPath primary a must map to self: %v", alias)
	}
	if alias[b] != b {
		t.Fatalf("distinct binary b must not fuse onto a: %v", alias)
	}
}

// Same resolved hash but different runner command must never fuse.
func TestCanonicalInstallByAlias_DifferentCommandSameResolvedNoFuse(t *testing.T) {
	const rhash = "sha256:same-file"
	a := pathInstall("tool", "cmd-a", "pinst_cmd_a", rhash, "sha256:pa", 0)
	b := pathInstall("tool", "cmd-b", "pinst_cmd_b", rhash, "sha256:pb", 0)
	alias := canonicalInstallByAlias([]providerinventory.ProviderInstallation{a, b})
	if alias["pinst_cmd_a"] != "pinst_cmd_a" || alias["pinst_cmd_b"] != "pinst_cmd_b" {
		t.Fatalf("different commands must not fuse: %v", alias)
	}
}
