package packdarwin_test

import (
	"testing"

	"github.com/jasonhnd/loopcoder/internal/packdarwin"
)

func TestBuildAndDraft(t *testing.T) {
	id := packdarwin.BuildIdentity{
		CommitSHA:       "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ProtectedBranch: "pre-prod", CleanHosted: true, Version: "0.9.0",
	}
	members := packdarwin.RequiredMembers()
	a, err := packdarwin.NewArtifact(id, []byte("archive-bytes"), members)
	if err != nil {
		t.Fatal(err)
	}
	if a.Platform != packdarwin.Platform || a.LocalDev {
		t.Fatalf("%#v", a)
	}
	p, err := packdarwin.BindProvenance(a, "sig:test", "sigstore")
	if err != nil {
		t.Fatal(err)
	}
	sbom := packdarwin.BuildSBOM(a)
	d, err := packdarwin.NewDraftRelease(a, p, sbom)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Draft || d.Published {
		t.Fatal(d)
	}
	pub, err := packdarwin.ApprovePublication(d)
	if err != nil || !pub.Published || pub.Draft {
		t.Fatalf("%v %#v", err, pub)
	}
}

func TestRejectLocalAndWindows(t *testing.T) {
	if err := packdarwin.RejectLocalPromotion(true); err == nil {
		t.Fatal()
	}
	id := packdarwin.BuildIdentity{CommitSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", CleanHosted: false, Version: "0.9.0"}
	if _, err := packdarwin.NewArtifact(id, []byte("x"), packdarwin.RequiredMembers()); err == nil {
		t.Fatal("unclean host")
	}
	if err := packdarwin.ValidateMembers(append(packdarwin.RequiredMembers(), "loopcoder_windows.exe")); err == nil {
		t.Fatal("windows member")
	}
}

func TestMissingMember(t *testing.T) {
	if err := packdarwin.ValidateMembers([]string{"loopcoder"}); err == nil {
		t.Fatal()
	}
}
