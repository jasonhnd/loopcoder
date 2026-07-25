package goalrun

import (
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

func fullSHA256Seed(seed string) string {
	const hex = "0123456789abcdef"
	b := "sha256:"
	for i := 0; i < 64; i++ {
		if i < len(seed) {
			b += string(hex[int(seed[i])%16])
		} else {
			b += string(hex[i%16])
		}
	}
	return b
}

func TestBindOpenPRVerifierFromChildren(t *testing.T) {
	verEvid := fullSHA256Seed("verify-ok")
	implEvid := fullSHA256Seed("impl-ok")
	goodVerify := workflowrun.ChildOutcome{
		WorkItemID: "wi_verify", TaskClass: "soul", Terminal: "succeeded",
		Provider: "codex", AttemptID: "att-v-1", OutputEvidence: verEvid,
	}
	goodImpl := workflowrun.ChildOutcome{
		WorkItemID: "wi_implement", TaskClass: "tera", Terminal: "succeeded",
		Provider: "grok", AttemptID: "att-i-1", OutputEvidence: implEvid,
	}

	t.Run("positive_exact", func(t *testing.T) {
		prov, evid, ok := bindOpenPRVerifierFromChildren([]workflowrun.ChildOutcome{goodImpl, goodVerify})
		if !ok {
			t.Fatal("want ok")
		}
		if prov != "codex" || evid != verEvid {
			t.Fatalf("prov=%q evid=%q", prov, evid)
		}
	})

	t.Run("wi_verify_notes_fails", func(t *testing.T) {
		notes := goodVerify
		notes.WorkItemID = "wi_verify_notes"
		_, _, ok := bindOpenPRVerifierFromChildren([]workflowrun.ChildOutcome{goodImpl, notes})
		if ok {
			t.Fatal("substring id must fail")
		}
	})

	t.Run("short_digest_fails", func(t *testing.T) {
		v := goodVerify
		v.OutputEvidence = "sha256:short"
		_, _, ok := bindOpenPRVerifierFromChildren([]workflowrun.ChildOutcome{goodImpl, v})
		if ok {
			t.Fatal("short digest must fail")
		}
	})

	t.Run("same_provider_case_insensitive_fails", func(t *testing.T) {
		v := goodVerify
		v.Provider = "GROK"
		_, _, ok := bindOpenPRVerifierFromChildren([]workflowrun.ChildOutcome{goodImpl, v})
		if ok {
			t.Fatal("same provider must fail")
		}
	})

	t.Run("duplicate_exact_verify_fails", func(t *testing.T) {
		v2 := goodVerify
		v2.AttemptID = "att-v-2"
		_, _, ok := bindOpenPRVerifierFromChildren([]workflowrun.ChildOutcome{goodImpl, goodVerify, v2})
		if ok {
			t.Fatal("duplicate verify must fail")
		}
	})

	t.Run("missing_verify_attempt_fails", func(t *testing.T) {
		v := goodVerify
		v.AttemptID = ""
		_, _, ok := bindOpenPRVerifierFromChildren([]workflowrun.ChildOutcome{goodImpl, v})
		if ok {
			t.Fatal("missing attempt must fail")
		}
	})

	t.Run("wrong_class_fails", func(t *testing.T) {
		v := goodVerify
		v.TaskClass = "tera"
		_, _, ok := bindOpenPRVerifierFromChildren([]workflowrun.ChildOutcome{goodImpl, v})
		if ok {
			t.Fatal("wrong class must fail")
		}
	})

	t.Run("whitespace_work_item_id_fails", func(t *testing.T) {
		v := goodVerify
		v.WorkItemID = " wi_verify"
		_, _, ok := bindOpenPRVerifierFromChildren([]workflowrun.ChildOutcome{goodImpl, v})
		if ok {
			t.Fatal("whitespace WorkItemID must fail")
		}
		v.WorkItemID = "wi_verify "
		_, _, ok = bindOpenPRVerifierFromChildren([]workflowrun.ChildOutcome{goodImpl, v})
		if ok {
			t.Fatal("trailing whitespace WorkItemID must fail")
		}
	})

	t.Run("invalid_exact_verify_alone_fails", func(t *testing.T) {
		// Exact wi_verify id but failed terminal — fail closed.
		v := goodVerify
		v.Terminal = "failed"
		_, _, ok := bindOpenPRVerifierFromChildren([]workflowrun.ChildOutcome{goodImpl, v})
		if ok {
			t.Fatal("failed exact verify must fail")
		}
	})

	// Sanity: forged pin strings are not inputs to the helper at all.
	_ = strings.Contains("sha256:forged-pin", "sha256")
}
