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

// CROMeasurement holds product-path routing/decomposition/capacity evidence
// measured against the exact built binary (V090-CRO-010).
type CROMeasurement struct {
	RoutingOK       bool
	DecomposeOK     bool
	AccountingOK    bool
	ProvidersSeen   []string
	DepthsSeen      []string
	ChildCount      int
	EvidenceRefs    map[string]string
	Probes          []Probe
	MultiProviderOK bool
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

	if !out.DecomposeOK {
		return out, fmt.Errorf("artifactqual: workgraph decomposition not measured (≥4 children)")
	}
	if !out.RoutingOK {
		return out, fmt.Errorf("artifactqual: useful-capacity routing path not measured")
	}
	if !out.AccountingOK {
		return out, fmt.Errorf("artifactqual: capacity accounting path not measured")
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
