package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/artifactqual"
)

func runQualify(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet("qualify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	archive := fs.String("archive", "", "path to exact RC archive (.tar.gz)")
	digest := fs.String("digest", "", "expected sha256 of archive")
	sha := fs.String("sha", "", "source commit SHA")
	work := fs.String("work-dir", "", "scratch directory (default temp)")
	format := fs.String("format", "text", "text|json")
	iv := fs.Bool("integration-verify-ok", false, "remote integration-verify green for SHA")
	ic := fs.Bool("integration-canary-ok", false, "remote integration-canary green for SHA")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*archive) == "" || strings.TrimSpace(*digest) == "" {
		fmt.Fprintln(stderr, "qualify: --archive and --digest required")
		return 2
	}
	wd := strings.TrimSpace(*work)
	if wd == "" {
		var err error
		wd, err = os.MkdirTemp("", "loopcoder-qualify-*")
		if err != nil {
			fmt.Fprintf(stderr, "qualify: temp: %v\n", err)
			return 4
		}
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	ev, err := artifactqual.Qualify(artifactqual.Input{
		Mode:        artifactqual.ModeRelease,
		ArchivePath: *archive, ExpectedDigest: *digest, SHA: *sha,
		WorkDir: wd, IntegrationVerifyOK: *iv, IntegrationCanaryOK: *ic,
		Now: now().UTC(),
	})
	if err != nil {
		fmt.Fprintf(stderr, "qualify: %v\n", err)
		return 4
	}
	if strings.ToLower(*format) == "json" {
		_, _ = stdout.Write(artifactqual.EvidenceJSON(ev))
	} else {
		fmt.Fprintf(stdout, "passed=%v digest=%s probes=%d install_smoke=%v decision=%s\n",
			ev.Passed, ev.ArchiveDigest, len(ev.Probes), ev.InstallSmoke.Passed, ev.Decision.Decision)
		if len(ev.Reasons) > 0 {
			fmt.Fprintf(stdout, "reasons=%s\n", strings.Join(ev.Reasons, ";"))
		}
	}
	// write evidence beside work dir when possible
	_ = os.WriteFile(filepath.Join(wd, "qualification-evidence.json"), artifactqual.EvidenceJSON(ev), 0o600)
	if !ev.Passed {
		return 4
	}
	return 0
}
