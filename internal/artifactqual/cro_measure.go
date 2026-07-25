package artifactqual

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// CROMeasurement holds structural (dry-run / plan / snapshot) prechecks only.
// Real multi-provider execution, depth invocation, capacity after, unavailable
// retry, restart, and PR gates require CanaryEvidence — never dry-run.
type CROMeasurement struct {
	RoutingOK    bool // structural auto-route path
	DecomposeOK  bool // structural plan ≥4 children
	AccountingOK bool // structural capacity policy path
	// StructuralDepthPlanOK: plan emits depth=low and depth=high requirements.
	// NOT real runtime depth binding.
	StructuralDepthPlanOK bool
	// StructuralExcludeOK: injected snapshot routing does not select exhausted.
	// NOT real unavailable retry / no-dup execution evidence.
	StructuralExcludeOK bool
	ProvidersSeen       []string
	DepthsSeen          []string
	ChildCount          int
	EvidenceRefs        map[string]string
	Probes              []Probe
	// MultiProviderOK is catalog/status structural presence only — never actual
	// execution. Prefer Planned* from dry-run goal JSON when present.
	MultiProviderOK bool
	// Planned* from workflow goal --dry-run JSON (goalrun.Result). Actual
	// providers_used / multi_provider_ok must stay empty/false on dry-run.
	PlannedProvidersUsed       []string
	PlannedModelsUsed          []string
	PlannedDepthsUsed          []string
	PlannedMultiProviderOK     bool
	PlannedMultiModelOrDepthOK bool
	// ActualUsageEmpty is true when dry-run JSON has empty actual usage fields.
	ActualUsageEmpty bool
}

// MeasureCROFromBinary runs the extracted binary for workflow plan + auto-route
// capacity path and records measured (not fixture) product metrics.
func MeasureCROFromBinary(bin, workDir string, envBase []string) (CROMeasurement, error) {
	out := CROMeasurement{EvidenceRefs: map[string]string{}}
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return out, err
	}
	home := filepath.Join(workDir, "cro-home")
	_ = os.MkdirAll(home, 0o700)
	env := append([]string{}, envBase...)
	env = append(env, "LOOPCODER_HOME="+home, "HOME="+home)

	// 1) WorkGraph decomposition via exact binary
	t0 := time.Now()
	planOut, planErr, planCode := runBin(bin, env, workDir, "workflow", "plan",
		"--goal", "implement multi-provider capacity-aware routing with tests and verification",
		"--issue", "1343", "--format", "json")
	out.Probes = append(out.Probes, Probe{
		Name: "cro_workgraph_plan", ExitCode: planCode, Passed: planCode == 0,
		Duration: time.Since(t0), OutputDigest: digestBytes(planOut),
		Reasons: reasonsIf(planCode != 0, string(planErr)),
	})
	if planCode == 0 {
		var g map[string]any
		if json.Unmarshal(planOut, &g) == nil {
			if items, ok := g["items"].([]any); ok {
				out.ChildCount = len(items)
				out.DecomposeOK = len(items) >= 4
			}
			if src, _ := g["source"].(string); src == "goal_decompose" && out.DecomposeOK {
				out.EvidenceRefs["decompose"] = "probe:workflow_plan_goal_decompose"
			}
		}
	}

	// 2) Useful-capacity routing + accounting on exact binary with an explicit
	// measured capacity snapshot file (not DefaultInventory; honest windows).
	snapPath := filepath.Join(workDir, "cro-capacity-snapshot.json")
	if err := writeCROMeasureSnapshot(snapPath); err != nil {
		return out, err
	}
	t1 := time.Now()
	routeOut, routeErr, routeCode := runBin(bin, env, workDir, "run",
		"--repo", "acme/cro-qual", "--issue", "1343",
		"--auto-route", "--dry-run", "--format", "json",
		"--ui-required", "terminal",
		"--capacity-snapshot", snapPath)
	combined := string(routeOut) + string(routeErr)
	routed := strings.Contains(combined, "auto-route selected") || strings.Contains(combined, "task class=")
	capLine := strings.Contains(combined, "capacity policy=")
	out.RoutingOK = routed && (capLine || routeCode == 0)
	out.AccountingOK = capLine && (strings.Contains(combined, "before=") || strings.Contains(combined, "capacity_before") || strings.Contains(combined, "reserved="))
	if strings.Contains(combined, "use-before-reset") {
		out.EvidenceRefs["policy"] = "probe:use_before_reset"
	}
	out.Probes = append(out.Probes, Probe{
		Name: "cro_auto_route_capacity", ExitCode: routeCode, Passed: out.RoutingOK && out.AccountingOK,
		Duration: time.Since(t1), OutputDigest: digestBytes([]byte(combined)),
		Reasons: reasonsIf(!out.RoutingOK || !out.AccountingOK, "routing/capacity path incomplete: "+truncate(combined, 200)),
	})
	if out.AccountingOK {
		out.EvidenceRefs["accounting"] = "probe:capacity_ledger_path"
	}
	if out.RoutingOK {
		out.EvidenceRefs["routing"] = "probe:run_auto_route_capacity"
	}

	// 3) Multi-provider presence: providers status from binary if available
	t2 := time.Now()
	provOut, _, provCode := runBin(bin, env, workDir, "providers", "status", "--format", "json")
	out.Probes = append(out.Probes, Probe{
		Name: "cro_providers_status", ExitCode: provCode, Passed: provCode == 0 || provCode == 1,
		Duration: time.Since(t2), OutputDigest: digestBytes(provOut),
	})
	for _, name := range []string{"codex", "claude", "grok", "antigravity", "gemini"} {
		if bytes.Contains(provOut, []byte(name)) || bytes.Contains(routeOut, []byte(name)) {
			out.ProvidersSeen = append(out.ProvidersSeen, name)
		}
	}
	out.MultiProviderOK = len(uniqueStrings(out.ProvidersSeen)) >= 2
	for _, d := range []string{"low", "medium", "high"} {
		if strings.Contains(combined, "depth="+d) || strings.Contains(string(planOut), "depth="+d) {
			out.DepthsSeen = append(out.DepthsSeen, d)
		}
	}

	// 4) Structural only: plan carries multi-depth requirements (low+high).
	// Dry-run route preview may show requirement→selection text, but that is
	// NOT real provider execution and MUST NOT set real_runtime multi_depth.
	hasLow := strings.Contains(string(planOut), "depth=low")
	hasHigh := strings.Contains(string(planOut), "depth=high")
	out.StructuralDepthPlanOK = hasLow && hasHigh
	if out.StructuralDepthPlanOK {
		out.DepthsSeen = uniqueStrings(append(out.DepthsSeen, "low", "high"))
		out.EvidenceRefs["structural_depth_plan"] = "probe:workflow_plan_depth_requirements"
	}
	t3 := time.Now()
	goalOut, goalErr, goalCode := runBin(bin, env, workDir, "workflow", "goal",
		"--goal", "implement multi-provider capacity-aware routing with tests and verification",
		"--issue", "1343", "--format", "json", "--dry-run",
		"--repo", workDir,
		"--capacity-snapshot", snapPath)
	goalCombined := string(goalOut) + string(goalErr)
	// Parse Planned* structural fields; actual usage must stay empty on dry-run.
	var goalJSON map[string]any
	if json.Unmarshal(goalOut, &goalJSON) == nil {
		out.PlannedProvidersUsed = stringSliceField(goalJSON, "planned_providers_used")
		out.PlannedModelsUsed = stringSliceField(goalJSON, "planned_models_used")
		out.PlannedDepthsUsed = stringSliceField(goalJSON, "planned_depths_used")
		if v, ok := goalJSON["planned_multi_provider_ok"].(bool); ok {
			out.PlannedMultiProviderOK = v
		}
		if v, ok := goalJSON["planned_multi_model_or_depth_ok"].(bool); ok {
			out.PlannedMultiModelOrDepthOK = v
		}
		actualProv := stringSliceField(goalJSON, "providers_used")
		actualModels := stringSliceField(goalJSON, "models_used")
		actualDepths := stringSliceField(goalJSON, "depths_used")
		multiP, _ := goalJSON["multi_provider_ok"].(bool)
		multiMD, _ := goalJSON["multi_model_or_depth_ok"].(bool)
		out.ActualUsageEmpty = len(actualProv) == 0 && len(actualModels) == 0 && len(actualDepths) == 0 && !multiP && !multiMD
		if len(out.PlannedProvidersUsed) > 0 {
			out.ProvidersSeen = uniqueStrings(append(out.ProvidersSeen, out.PlannedProvidersUsed...))
		}
		if len(out.PlannedDepthsUsed) > 0 {
			out.DepthsSeen = uniqueStrings(append(out.DepthsSeen, out.PlannedDepthsUsed...))
			out.EvidenceRefs["planned_depths"] = "probe:goal_dry_run_planned_depths"
		}
		if out.PlannedMultiProviderOK {
			out.EvidenceRefs["planned_multi_provider"] = "probe:goal_dry_run_planned_multi_provider"
		}
	}
	// Structural probe only — never claim real multi-depth runtime from dry-run.
	out.Probes = append(out.Probes, Probe{
		Name: "structural_goal_dry_run", ExitCode: goalCode, Passed: goalCode == 0 || goalCode == 1,
		Duration: time.Since(t3), OutputDigest: digestBytes([]byte(goalCombined)),
		Reasons: []string{"structural_only: dry-run Planned* only; actual multi_provider/multi_depth stay false"},
	})

	// 5) Structural only: injected exhausted snapshot routing (deterministic).
	// NOT real unavailable retry / no-dup claim evidence.
	exPath := filepath.Join(workDir, "cro-exhausted-snapshot.json")
	if err := writeCROExhaustedSnapshot(exPath); err != nil {
		return out, err
	}
	t4 := time.Now()
	exOut, exErr, exCode := runBin(bin, env, workDir, "run",
		"--repo", "acme/cro-excl", "--issue", "1343",
		"--auto-route", "--dry-run", "--format", "json",
		"--ui-required", "terminal",
		"--capacity-snapshot", exPath)
	exCombined := string(exOut) + string(exErr)
	selectedCodexOnly := strings.Contains(exCombined, "auto-route selected codex") &&
		!strings.Contains(exCombined, "antigravity")
	out.StructuralExcludeOK = exCode == 0 && !selectedCodexOnly
	if strings.Contains(exCombined, "antigravity") {
		out.StructuralExcludeOK = true
		out.EvidenceRefs["structural_exclude"] = "probe:structural_exhausted_route_inventory"
	}
	out.Probes = append(out.Probes, Probe{
		Name: "structural_unavailable_inventory", ExitCode: exCode, Passed: out.StructuralExcludeOK,
		Duration: time.Since(t4), OutputDigest: digestBytes([]byte(exCombined)),
		Reasons: []string{"structural_only: snapshot dry-run cannot satisfy real_runtime unavailable_retry metrics"},
	})

	// Structural failures are soft for measure return (recorded as probes).
	// real_runtime required metrics come only from CanaryEvidence.
	if !out.DecomposeOK {
		return out, fmt.Errorf("artifactqual: structural workgraph decomposition not measured (≥4 children)")
	}
	if !out.RoutingOK {
		return out, fmt.Errorf("artifactqual: structural useful-capacity routing path not measured")
	}
	if !out.AccountingOK {
		return out, fmt.Errorf("artifactqual: structural capacity accounting path not measured")
	}
	return out, nil
}

func runBin(bin string, env []string, dir string, args ...string) (stdout, stderr []byte, code int) {
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	cmd.Dir = dir
	var outB, errB bytes.Buffer
	cmd.Stdout = &outB
	cmd.Stderr = &errB
	err := cmd.Run()
	code = 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = 1
			errB.WriteString(err.Error())
		}
	}
	return outB.Bytes(), errB.Bytes(), code
}

func reasonsIf(cond bool, msg string) []string {
	if cond {
		return []string{msg}
	}
	return nil
}

func digestBytes(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// stringSliceField extracts a JSON string array field from an untyped map.
func stringSliceField(m map[string]any, key string) []string {
	raw, ok := m[key]
	if !ok || raw == nil {
		return nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, v := range arr {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// writeCROMeasureSnapshot writes a dual-provider capacity snapshot used only for
// exact-binary product-path measurement (explicit file, not silent DefaultInventory).
func writeCROMeasureSnapshot(path string) error {
	now := time.Now().UTC()
	reset := now.Add(45 * time.Minute)
	return writeSnapshotFile(path, now, reset)
}
