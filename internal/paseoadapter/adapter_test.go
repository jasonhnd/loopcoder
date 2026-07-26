package paseoadapter_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/paseoadapter"
	"github.com/jasonhnd/loopcoder/internal/uiconform"
	"github.com/jasonhnd/loopcoder/internal/uireport"
	"github.com/jasonhnd/loopcoder/internal/uisub"
)

func TestConformanceAndReconnect(t *testing.T) {
	p, err := paseoadapter.RunConformance("proj_paseo")
	if err != nil {
		t.Fatal(err)
	}
	if !p.ConformancePass {
		t.Fatalf("conformance fail: %+v", p)
	}
	if p.HighestStage != string(uisub.StageRendered) {
		t.Fatalf("stage=%s", p.HighestStage)
	}
	if p.RealHostClaim {
		t.Fatal("must not claim real host support from fixture")
	}
	if p.InterfaceGap != nil {
		t.Fatal("fixture path should not gap")
	}
}

func TestDualPathTerminalAndAdapter(t *testing.T) {
	if err := paseoadapter.DualPath("proj_dual"); err != nil {
		t.Fatal(err)
	}
}

func TestOptInSmokeRecordsGapWithoutWeakening(t *testing.T) {
	t.Setenv("PASEO_ADAPTER_SMOKE", "1")
	p, err := paseoadapter.MaybeRealSmoke("proj")
	if err != nil {
		t.Fatal(err)
	}
	if p.InterfaceGap == nil || p.InterfaceGap.Code == "" {
		t.Fatal("expected interface gap when rendered unproven")
	}
	if p.HighestStage == string(uisub.StageRendered) {
		t.Fatal("must not claim rendered without proof")
	}
	if p.RealHostClaim {
		t.Fatal("no support claim")
	}
}

func TestSmokeDisabledByDefault(t *testing.T) {
	t.Setenv("PASEO_ADAPTER_SMOKE", "")
	if _, err := paseoadapter.MaybeRealSmoke("p"); err == nil {
		t.Fatal("expected disabled")
	}
}

func TestNoPaseoSourceInPackage(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	dir := filepath.Dir(file)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if !paseoadapter.HasNoPaseoImport(string(b)) {
			t.Fatalf("paseo import-like token in %s", e.Name())
		}
		// Disallow a distinctive paste marker without embedding the marker text itself.
		if strings.Contains(string(b), "COPY FROM "+"PASEO") {
			t.Fatal("forbidden marker")
		}
	}
	for _, g := range paseoadapter.LicenseGuard() {
		if g == "" {
			t.Fatal("empty guard")
		}
	}
}

type noRender struct{}

func (noRender) ShowActivity(uireport.Kind, string) error { return nil }
func (noRender) NotifyAttention(string) error             { return nil }
func (noRender) AckRendered() bool                        { return false }
func (noRender) Name() string                             { return "no_render" }
func (noRender) Close() error                             { return nil }

func TestConsumeAcksOnlyWhenSurfaceRendered(t *testing.T) {
	l := uisub.NewLedger("p", 8, time.Now)
	_ = l.RegisterClient(uisub.ClientIdentity{ClientID: "x", SessionID: "s", ProjectID: "p"})
	for _, e := range uiconform.GoldenTranscript("p")[:1] {
		_ = l.Publish(e)
	}
	ad := paseoadapter.New(l, "x", noRender{})
	_, err := ad.Consume(context.Background())
	if err == nil {
		t.Fatal("expected error when surface cannot prove rendered")
	}
}
