package agent

import (
	"strings"
	"testing"
)

func TestArgvPermissionProofNeverScansPrompt(t *testing.T) {
	// Adversarial: free-form prompt/model/path contain every former proof token.
	contaminants := []string{
		"workspace-write", "read-only", "--yolo", "safe-mode",
		"--skip-trust", "dangerously-skip-permissions",
		"dangerously-bypass-approvals-and-sandbox", "--sandbox strict",
	}
	for _, token := range contaminants {
		// Prompt as free-form token (Codex uses stdin; Grok uses -p value).
		argv := []string{"grok", "-p", "please use " + token + " mode", "--cwd", "/tmp"}
		if argvHasExactPermissionOption(argv, false) {
			t.Fatalf("write proof must not match prompt containing %q: %#v", token, argv)
		}
		if argvHasExactPermissionOption(argv, true) {
			t.Fatalf("read proof must not match prompt containing %q", token)
		}
		// Model free-form equal to token
		argv2 := []string{"codex", "exec", "-m", token, "-"}
		if argvHasExactPermissionOption(argv2, false) {
			t.Fatalf("write proof must not match model value %q", token)
		}
		// Path containing token
		argv3 := []string{"claude", "--print", "/tmp/" + token + "/project"}
		if argvHasExactPermissionOption(argv3, false) {
			t.Fatalf("write proof must not match path containing %q", token)
		}
	}
	// Exact option positions DO prove.
	if !argvHasExactPermissionOption([]string{"codex", "exec", "-s", "workspace-write", "-"}, false) {
		t.Fatal("exact -s workspace-write must prove write")
	}
	if !argvHasExactPermissionOption([]string{"codex", "exec", "-s", "read-only", "-"}, true) {
		t.Fatal("exact -s read-only must prove read")
	}
	if !argvHasExactPermissionOption([]string{"gemini", "--yolo", "-m", "m"}, false) {
		t.Fatal("exact --yolo must prove write")
	}
	if !argvHasExactPermissionOption([]string{"gemini", "--skip-trust"}, true) {
		t.Fatal("exact --skip-trust must prove read")
	}
	if !argvHasExactPermissionOption([]string{"grok", "--sandbox", "strict"}, false) {
		t.Fatal("exact --sandbox strict must prove write")
	}
	if !argvHasExactPermissionOption([]string{"agy", "--dangerously-skip-permissions"}, false) {
		t.Fatal("exact --dangerously-skip-permissions must prove write")
	}
}

func TestArgvModelOnlyOptionPairing(t *testing.T) {
	// Free-form prompt equal to model must never prove.
	model := "grok-4.5"
	argv := []string{"grok", "-p", model, "--cwd", "/tmp"}
	if argvOptionValueEquals(argv, model, "-m", "--model") {
		t.Fatal("prompt equal to model must not prove model option")
	}
	// Exact -m pairing
	if !argvOptionValueEquals([]string{"codex", "-m", model, "-"}, model, "-m", "--model") {
		t.Fatal("exact -m value must prove")
	}
	if !argvOptionValueEquals([]string{"grok", "--model=" + model}, model, "-m", "--model") {
		t.Fatal("exact --model=value must prove")
	}
	// Contaminant in path
	if argvOptionValueEquals([]string{"claude", "/tmp/" + model}, model, "-m", "--model") {
		t.Fatal("path containing model must not prove")
	}
}

func TestArgvEffortNeverMatchesPrompt(t *testing.T) {
	effort := "medium"
	argv := []string{"grok", "-p", "use medium depth please", "--cwd", "/tmp"}
	if argvHasExactEffortOption(argv, effort) {
		t.Fatal("prompt containing effort token must not prove effort")
	}
	if !argvHasExactEffortOption([]string{"grok", "--effort", "medium"}, effort) {
		t.Fatal("exact --effort medium must prove")
	}
	if !argvHasExactEffortOption([]string{"grok", "--reasoning-effort", "medium"}, effort) {
		t.Fatal("exact --reasoning-effort must prove")
	}
	if !argvHasExactEffortOption([]string{"codex", "-c", "model_reasoning_effort=medium"}, effort) {
		t.Fatal("exact -c model_reasoning_effort= must prove")
	}
	// -c with wrong config key must not prove even if value is medium
	if argvHasExactEffortOption([]string{"codex", "-c", "other=medium"}, effort) {
		t.Fatal("unrelated -c must not prove effort")
	}
}

func TestAffirmAcceptedInvocationOnlyOnSuccess(t *testing.T) {
	inv := Invocation{Permission: "bounded_write", BoundedWrite: true, Model: "m1", Effort: "high"}
	argv := []string{"codex", "exec", "-s", "workspace-write", "-m", "m1", "-c", "model_reasoning_effort=high"}
	// success=false must not affirm
	res := Result{ExitCode: 1}
	AffirmAcceptedInvocation(&res, inv, argv, false, AcceptedInvocationOpts{
		PermissionNoFallback: true, ModelNoFallback: true, EffortNoFallback: true,
	})
	if res.ActualPermission != "" || res.ActualModel != "" || res.ActualEffort != "" {
		t.Fatalf("failed run must not gain accepted Actual*: %+v", res)
	}
	// success=true affirms with accepted_invocation (not provider_stream)
	res2 := Result{ExitCode: 0}
	AffirmAcceptedInvocation(&res2, inv, argv, true, AcceptedInvocationOpts{
		PermissionNoFallback: true, ModelNoFallback: true, EffortNoFallback: true,
	})
	if res2.ActualSourcePermission != ActualSourceAcceptedInvocation {
		t.Fatalf("source=%q want accepted_invocation", res2.ActualSourcePermission)
	}
	if res2.ActualSourceModel != ActualSourceAcceptedInvocation {
		t.Fatalf("model source=%q", res2.ActualSourceModel)
	}
	if res2.ActualSourceEffort != ActualSourceAcceptedInvocation {
		t.Fatalf("effort source=%q", res2.ActualSourceEffort)
	}
	if res2.ArgvDigest == "" {
		t.Fatal("argv digest required")
	}
}

func TestClearAcceptedActualPreservesAuthBinding(t *testing.T) {
	res := Result{
		ActualPermission: "bounded_write", ActualSourcePermission: ActualSourceAcceptedInvocation,
		ActualModel: "m", ActualSourceModel: ActualSourceAcceptedInvocation,
		ActualAccountRef: "acct-x", ActualSourceAccount: ActualSourceAuthBinding,
		ActualInstallRef: "pinst_x", ActualSourceInstall: ActualSourceInstallBinding,
	}
	ClearAcceptedActual(&res)
	if res.ActualPermission != "" || res.ActualModel != "" {
		t.Fatalf("accepted dims must clear: %+v", res)
	}
	if res.ActualAccountRef != "acct-x" || res.ActualSourceAccount != ActualSourceAuthBinding {
		t.Fatalf("auth binding must remain: %+v", res)
	}
	if res.ActualInstallRef != "pinst_x" || res.ActualSourceInstall != ActualSourceInstallBinding {
		t.Fatalf("install binding must remain: %+v", res)
	}
}

func TestPromptContaminationAllTokens(t *testing.T) {
	// One prompt containing every proof token at once.
	all := strings.Join([]string{
		"workspace-write", "read-only", "--yolo", "safe-mode", "--skip-trust",
		"dangerously-skip-permissions", "dangerously-bypass-approvals-and-sandbox",
		"--sandbox", "strict", "model_reasoning_effort=high",
	}, " ")
	argv := []string{"grok", "-p", all, "-m", "should-not-match-as-permission"}
	if argvHasExactPermissionOption(argv, false) || argvHasExactPermissionOption(argv, true) {
		t.Fatal("combined contaminant prompt must not prove permission")
	}
	if argvHasExactEffortOption(argv, "high") {
		t.Fatal("prompt model_reasoning_effort=high must not prove effort without -c")
	}
}
