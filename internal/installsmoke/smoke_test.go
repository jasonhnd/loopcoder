package installsmoke_test

import (
	"testing"

	"github.com/jasonhnd/loopcoder/internal/installsmoke"
	"github.com/jasonhnd/loopcoder/internal/privacy"
)

func TestGreenFixturePasses(t *testing.T) {
	art, env := installsmoke.FixtureEnvironment("deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	r := installsmoke.Run(art, env)
	if !r.Passed {
		t.Fatalf("%#v", r)
	}
	if r.RebuiltDuringSmoke {
		t.Fatal()
	}
	if len(r.Steps) < 10 {
		t.Fatal(len(r.Steps))
	}
}

func TestRebuildFails(t *testing.T) {
	art, env := installsmoke.FixtureEnvironment("aa")
	env.Rebuilt = true
	r := installsmoke.Run(art, env)
	if r.Passed {
		t.Fatal("rebuild must fail smoke")
	}
}

func TestRepoLocalWriteFails(t *testing.T) {
	art, env := installsmoke.FixtureEnvironment("bb")
	env.RepoLocalRuntimeWrite = true
	r := installsmoke.Run(art, env)
	if r.Passed {
		t.Fatal()
	}
}

func TestSourceHashChangeFails(t *testing.T) {
	art, env := installsmoke.FixtureEnvironment("cc")
	env.OldV08SourceHashAfter = "changed"
	r := installsmoke.Run(art, env)
	if r.Passed {
		t.Fatal()
	}
}

func TestMarkerLeak(t *testing.T) {
	if err := installsmoke.AssertNoMarkerLeak("ok"); err != nil {
		t.Fatal(err)
	}
	if err := installsmoke.AssertNoMarkerLeak(privacy.MarkerCredential); err == nil {
		t.Fatal("expected leak")
	}
}

func TestLocalDevArtifact(t *testing.T) {
	r := installsmoke.Run(installsmoke.ArtifactRef{Digest: "x", LocalDev: true}, installsmoke.Environment{})
	if r.Passed {
		t.Fatal()
	}
}
