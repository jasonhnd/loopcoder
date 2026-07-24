package capacitysnapshot

import (
	"testing"

	"github.com/jasonhnd/loopcoder/internal/providerinventory"
)

// Two PATH aliases of one physical binary: primary is earliest DiscoveryOrder
// (LookPath first hit), never lexicographic pinst alone. Secondary maps to primary.
func TestCanonicalInstallByAlias_PATHPrimaryNotLexicographic(t *testing.T) {
	const (
		// Secondary is lexicographically first (3an5 < wrpm) — old bug preferred it.
		secondary = "pinst_3an5v55kgyq352a2bbgkfljbmikrndoq"
		primary   = "pinst_wrpmecvyfayff7nnqvaztkqhfs7ua2hd"
		rhash     = "sha256:same-resolved-binary"
	)
	installs := []providerinventory.ProviderInstallation{
		{
			AdapterID: "grok", ProviderInstallationID: secondary,
			DiscoveryOrder: 1, InstallationState: providerinventory.InstallationInstalled,
			UsableForInvocation: "yes", FreshnessState: providerinventory.FreshnessFresh,
			Confidence: providerinventory.ConfidenceExact,
			ExecutableIdentity: providerinventory.ExecutableIdentity{
				ResolvedPathHash: rhash, PathHash: "sha256:path-secondary",
			},
		},
		{
			AdapterID: "grok", ProviderInstallationID: primary,
			DiscoveryOrder: 0, InstallationState: providerinventory.InstallationInstalled,
			UsableForInvocation: "yes", FreshnessState: providerinventory.FreshnessFresh,
			Confidence: providerinventory.ConfidenceExact,
			ExecutableIdentity: providerinventory.ExecutableIdentity{
				ResolvedPathHash: rhash, PathHash: "sha256:path-primary",
			},
		},
	}
	alias := canonicalInstallByAlias(installs)
	if alias[primary] != primary {
		t.Fatalf("primary must map to self, got %q", alias[primary])
	}
	if alias[secondary] != primary {
		t.Fatalf("secondary alias must map to PATH primary %s, got %q", primary, alias[secondary])
	}
}

// Flipping DiscoveryOrder flips primary so install_ref always tracks LookPath order.
func TestCanonicalInstallByAlias_PATHOrderFlipConsistent(t *testing.T) {
	const (
		a     = "pinst_a_first_on_path"
		b     = "pinst_b_second_on_path"
		rhash = "sha256:same"
	)
	mk := func(id string, order int) providerinventory.ProviderInstallation {
		return providerinventory.ProviderInstallation{
			AdapterID: "codex", ProviderInstallationID: id,
			DiscoveryOrder: order, InstallationState: providerinventory.InstallationInstalled,
			UsableForInvocation: "yes", FreshnessState: providerinventory.FreshnessFresh,
			Confidence: providerinventory.ConfidenceExact,
			ExecutableIdentity: providerinventory.ExecutableIdentity{
				ResolvedPathHash: rhash, PathHash: "sha256:" + id,
			},
		}
	}
	// PATH order a then b
	m1 := canonicalInstallByAlias([]providerinventory.ProviderInstallation{mk(a, 0), mk(b, 1)})
	if m1[a] != a || m1[b] != a {
		t.Fatalf("PATH a,b: want primary a; map=%v", m1)
	}
	// PATH order b then a
	m2 := canonicalInstallByAlias([]providerinventory.ProviderInstallation{mk(b, 0), mk(a, 1)})
	if m2[b] != b || m2[a] != b {
		t.Fatalf("PATH b,a: want primary b; map=%v", m2)
	}
}

// Distinct physical binaries (different ResolvedPathHash) never fuse.
func TestCanonicalInstallByAlias_DistinctBinariesRemainDistinct(t *testing.T) {
	const (
		a = "pinst_bin_a"
		b = "pinst_bin_b"
	)
	installs := []providerinventory.ProviderInstallation{
		{
			AdapterID: "grok", ProviderInstallationID: a, DiscoveryOrder: 0,
			InstallationState: providerinventory.InstallationInstalled, UsableForInvocation: "yes",
			FreshnessState: providerinventory.FreshnessFresh, Confidence: providerinventory.ConfidenceExact,
			ExecutableIdentity: providerinventory.ExecutableIdentity{ResolvedPathHash: "sha256:binary-a"},
		},
		{
			AdapterID: "grok", ProviderInstallationID: b, DiscoveryOrder: 1,
			InstallationState: providerinventory.InstallationInstalled, UsableForInvocation: "yes",
			FreshnessState: providerinventory.FreshnessFresh, Confidence: providerinventory.ConfidenceExact,
			ExecutableIdentity: providerinventory.ExecutableIdentity{ResolvedPathHash: "sha256:binary-b"},
		},
	}
	alias := canonicalInstallByAlias(installs)
	if alias[a] != a || alias[b] != b {
		t.Fatalf("distinct binaries must not fuse: %v", alias)
	}
}
