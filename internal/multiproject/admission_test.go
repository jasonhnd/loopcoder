package multiproject

import (
	"testing"
	"time"
)

func TestSameNameDifferentOwners(t *testing.T) {
	r := NewRegistry(DefaultBudget(), time.Now)
	p1, err := r.Register("loopcoder", "alice", "/Users/alice/src/loopcoder")
	if err != nil {
		t.Fatal(err)
	}
	p2, err := r.Register("loopcoder", "bob", "/Users/bob/work/loopcoder")
	if err != nil {
		t.Fatal(err)
	}
	if p1.ProjectID == p2.ProjectID || p1.StoreKey == p2.StoreKey {
		t.Fatal("ids must differ")
	}
	same := r.SameNameProjects("loopcoder")
	if len(same) != 2 {
		t.Fatalf("%d", len(same))
	}
	// path collision rejected
	_, err = r.Register("other", "alice", "/Users/alice/src/loopcoder")
	if err == nil {
		t.Fatal("path collision")
	}
}

func TestGlobalAdmissionLimits(t *testing.T) {
	b := DefaultBudget()
	b.MaxWorkers = 2
	r := NewRegistry(b, time.Now)
	p1, _ := r.Register("a", "o1", "/p/a")
	p2, _ := r.Register("b", "o2", "/p/b")
	d1, err := r.Admit(p1.ProjectID, Usage{Workers: 2, Processes: 2, ByProvider: map[string]int{"codex": 1}})
	if err != nil || !d1.Admitted {
		t.Fatalf("%+v %v", d1, err)
	}
	d2, err := r.Admit(p2.ProjectID, Usage{Workers: 1, Processes: 1})
	if err != nil {
		t.Fatal(err)
	}
	if d2.Admitted || !d2.Wait {
		t.Fatalf("should wait: %+v", d2)
	}
	// explainable
	if len(d2.Reasons) == 0 {
		t.Fatal("reasons")
	}
}

func TestSummaryRedactsPaths(t *testing.T) {
	r := NewRegistry(DefaultBudget(), time.Now)
	_, _ = r.Register("x", "alice", "/secret/path/to/repo")
	s := r.Summary()
	if len(s.Projects) != 1 {
		t.Fatal("count")
	}
	if s.Projects[0].PathBasename != "repo" {
		t.Fatalf("%s", s.Projects[0].PathBasename)
	}
	// no full path field
}

func TestRestartNoCrossProject(t *testing.T) {
	r := NewRegistry(DefaultBudget(), time.Now)
	p1, _ := r.Register("a", "o", "/p/a")
	p2, _ := r.Register("b", "o", "/p/b")
	_, _ = r.Admit(p1.ProjectID, Usage{Workers: 1, Processes: 2})
	_, _ = r.Admit(p2.ProjectID, Usage{Workers: 1, Processes: 1})
	// only p1 has live process
	r.RestartReconcile(map[string]int{p1.ProjectID: 1})
	// p2 reservation cleared when processes 0 after reconcile
	// p1 workers may shrink
}

func TestArchiveOnlySelf(t *testing.T) {
	r := NewRegistry(DefaultBudget(), time.Now)
	p1, _ := r.Register("a", "o", "/p/a")
	p2, _ := r.Register("b", "o", "/p/b")
	_, _ = r.Admit(p1.ProjectID, Usage{Workers: 1, Processes: 1})
	_, _ = r.Admit(p2.ProjectID, Usage{Workers: 1, Processes: 1})
	got, err := r.Archive(p1.ProjectID, true)
	if err != nil || !got.Archived {
		t.Fatal(err)
	}
	// p2 intact
	p2g, err := r.Get(p2.ProjectID)
	if err != nil || p2g.Archived {
		t.Fatal("p2 corrupted")
	}
	s := r.Summary()
	// both listed; p1 archived
	var arch, live int
	for _, p := range s.Projects {
		if p.Archived {
			arch++
		} else {
			live++
		}
	}
	if arch != 1 || live != 1 {
		t.Fatalf("arch=%d live=%d", arch, live)
	}
}

func TestProviderLimit(t *testing.T) {
	b := DefaultBudget()
	b.MaxPerProvider = map[string]int{"codex": 1}
	r := NewRegistry(b, time.Now)
	p1, _ := r.Register("a", "o", "/p/a")
	p2, _ := r.Register("b", "o", "/p/b")
	d1, _ := r.Admit(p1.ProjectID, Usage{Workers: 1, Processes: 1, ByProvider: map[string]int{"codex": 1}})
	if !d1.Admitted {
		t.Fatal(d1)
	}
	d2, _ := r.Admit(p2.ProjectID, Usage{Workers: 1, Processes: 1, ByProvider: map[string]int{"codex": 1}})
	if d2.Admitted {
		t.Fatalf("%+v", d2)
	}
}
