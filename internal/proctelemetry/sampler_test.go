package proctelemetry

import (
	"errors"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/processtree"
)

type fakeReader struct {
	data map[int]ProcResources
	err  error
	// calls records PID sets requested
	calls [][]int
}

func (f *fakeReader) Read(pids []int) (map[int]ProcResources, error) {
	cp := append([]int(nil), pids...)
	f.calls = append(f.calls, cp)
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[int]ProcResources, len(pids))
	for _, p := range pids {
		if r, ok := f.data[p]; ok {
			out[p] = r
		} else {
			out[p] = ProcResources{PID: p, OK: false}
		}
	}
	return out, nil
}

func TestAggregateIncludesOwnedExcludesUnrelated(t *testing.T) {
	fr := &fakeReader{data: map[int]ProcResources{
		10: {PID: 10, CPUTimeSecs: 1.5, RSSBytes: 1000, OK: true},
		11: {PID: 11, CPUTimeSecs: 0.5, RSSBytes: 2000, OK: true},
		99: {PID: 99, CPUTimeSecs: 100, RSSBytes: 99999, OK: true}, // unrelated, not requested
	}}
	var clock time.Time
	s := &Sampler{
		Reader: fr, MinInterval: time.Millisecond,
		Now: func() time.Time { return clock },
	}
	clock = time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	sample := s.SamplePIDs([]int{10, 11}, false, clock)
	if sample.Quality != QualityFull {
		t.Fatalf("%#v", sample)
	}
	if sample.ProcessCount != 2 || sample.CPUTimeSecs != 2.0 || sample.RSSBytes != 3000 {
		t.Fatalf("%#v", sample)
	}
	if len(fr.calls) != 1 || len(fr.calls[0]) != 2 {
		t.Fatalf("calls=%v", fr.calls)
	}
	// Unrelated 99 never sampled.
	for _, p := range fr.calls[0] {
		if p == 99 {
			t.Fatal("unrelated pid sampled")
		}
	}
}

func TestPIDReuseExcluded(t *testing.T) {
	s := &Sampler{Reader: &fakeReader{data: map[int]ProcResources{
		1: {PID: 1, CPUTimeSecs: 9, RSSBytes: 9, OK: true},
	}}}
	a := processtree.Assessment{
		Liveness: processtree.LivenessUnknown,
		Reasons:  []string{"pid_reuse"},
		Snapshot: processtree.Snapshot{Nodes: []processtree.Node{{PID: 1, Owned: true}}},
	}
	sample := s.SampleFromAssessment(a)
	if sample.Quality != QualityUnavailable || sample.RSSBytes != 0 {
		t.Fatalf("%#v", sample)
	}
	if !contains(sample.Reasons, "pid_reuse_excluded") {
		t.Fatalf("reasons=%v", sample.Reasons)
	}
}

func TestUnavailableNotZeroUse(t *testing.T) {
	s := &Sampler{
		Reader:      &fakeReader{err: errors.New("permission denied")},
		MinInterval: time.Millisecond,
	}
	sample := s.SamplePIDs([]int{1, 2}, false, time.Now())
	if sample.Quality != QualityUnavailable {
		t.Fatalf("%#v", sample)
	}
	// Explicit unavailable — ProcessCount/RSS may be 0 but quality must not be full.
	if sample.Quality == QualityFull {
		t.Fatal("must not report full on failure")
	}
	if !contains(sample.Reasons, "read_failed") {
		t.Fatalf("%v", sample.Reasons)
	}
}

func TestPartialObservation(t *testing.T) {
	fr := &fakeReader{data: map[int]ProcResources{
		1: {PID: 1, CPUTimeSecs: 1, RSSBytes: 100, OK: true},
		// 2 missing → not OK
	}}
	s := &Sampler{Reader: fr, MinInterval: time.Millisecond}
	sample := s.SamplePIDs([]int{1, 2}, false, time.Now())
	if sample.Quality != QualityPartial || sample.MissedPIDs != 1 || sample.SampledPIDs != 1 {
		t.Fatalf("%#v", sample)
	}
	if sample.RSSBytes != 100 {
		t.Fatalf("rss=%d", sample.RSSBytes)
	}
}

func TestBoundedPIDsNoBusyLoop(t *testing.T) {
	data := map[int]ProcResources{}
	var many []int
	for i := 1; i <= 100; i++ {
		many = append(many, i)
		data[i] = ProcResources{PID: i, CPUTimeSecs: 0.01, RSSBytes: 10, OK: true}
	}
	fr := &fakeReader{data: data}
	s := &Sampler{Reader: fr, MaxPIDs: 8, MinInterval: time.Millisecond}
	sample := s.SamplePIDs(many, false, time.Now())
	if !sample.Truncated || sample.SampledPIDs != 8 {
		t.Fatalf("%#v", sample)
	}
	if len(fr.calls[0]) != 8 {
		t.Fatalf("read size=%d", len(fr.calls[0]))
	}
}

func TestCPURateWithInjectedClock(t *testing.T) {
	fr := &fakeReader{data: map[int]ProcResources{
		1: {PID: 1, CPUTimeSecs: 1.0, RSSBytes: 100, OK: true},
	}}
	var clock time.Time
	s := &Sampler{
		Reader: fr, MinInterval: time.Millisecond,
		Now: func() time.Time { return clock },
	}
	clock = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s1 := s.SamplePIDs([]int{1}, false, clock)
	if s1.HasCPURate {
		t.Fatal("first sample should not have rate")
	}
	fr.data[1] = ProcResources{PID: 1, CPUTimeSecs: 3.0, RSSBytes: 100, OK: true}
	clock = clock.Add(2 * time.Second)
	s2 := s.SamplePIDs([]int{1}, false, clock)
	if !s2.HasCPURate || s2.CPURate != 1.0 { // Δ2s CPU / 2s wall
		t.Fatalf("%#v", s2)
	}
}

func TestThresholdCrossingOncePerTransition(t *testing.T) {
	fr := &fakeReader{data: map[int]ProcResources{
		1: {PID: 1, CPUTimeSecs: 0, RSSBytes: 100, OK: true},
	}}
	var clock time.Time
	s := &Sampler{
		Reader: fr, MinInterval: time.Millisecond,
		Thresholds: Thresholds{RSSBytes: 500, ProcessCount: 2, CPURate: 0.5},
		Now:        func() time.Time { return clock },
	}
	clock = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Init sample — no crossings.
	s0 := s.SamplePIDs([]int{1}, false, clock)
	if len(s0.Crossings) != 0 {
		t.Fatalf("init crossings=%v", s0.Crossings)
	}
	// Cross RSS up.
	fr.data[1] = ProcResources{PID: 1, CPUTimeSecs: 0, RSSBytes: 1000, OK: true}
	clock = clock.Add(time.Second)
	s1 := s.SamplePIDs([]int{1}, false, clock)
	if !hasCrossing(s1.Crossings, MetricRSSBytes, CrossingUp) {
		t.Fatalf("want rss up: %v", s1.Crossings)
	}
	// Stay above — no repeat flood.
	clock = clock.Add(time.Second)
	s2 := s.SamplePIDs([]int{1}, false, clock)
	if hasCrossing(s2.Crossings, MetricRSSBytes, CrossingUp) {
		t.Fatalf("repeat crossing: %v", s2.Crossings)
	}
	// Drop below then up again → one more crossing.
	fr.data[1] = ProcResources{PID: 1, CPUTimeSecs: 0, RSSBytes: 10, OK: true}
	clock = clock.Add(time.Second)
	s3 := s.SamplePIDs([]int{1}, false, clock)
	if !hasCrossing(s3.Crossings, MetricRSSBytes, CrossingDown) {
		t.Fatalf("want down: %v", s3.Crossings)
	}
}

func TestWrapperExitDescendantsStillCounted(t *testing.T) {
	// Assessment: wrapper dead, owned kids live.
	a := processtree.Assessment{
		Liveness: processtree.LivenessAlive,
		Reasons:  []string{"wrapper_exited_descendants_alive"},
		Snapshot: processtree.Snapshot{Nodes: []processtree.Node{
			{PID: 2, Owned: true},
			{PID: 3, Owned: true},
			{PID: 9, Owned: false, Escaped: true}, // excluded
		}},
	}
	fr := &fakeReader{data: map[int]ProcResources{
		2: {PID: 2, CPUTimeSecs: 1, RSSBytes: 10, OK: true},
		3: {PID: 3, CPUTimeSecs: 2, RSSBytes: 20, OK: true},
		9: {PID: 9, CPUTimeSecs: 50, RSSBytes: 50, OK: true},
	}}
	s := &Sampler{Reader: fr, MinInterval: time.Millisecond}
	sample := s.SampleFromAssessment(a)
	if sample.ProcessCount != 2 || sample.RSSBytes != 30 {
		t.Fatalf("%#v", sample)
	}
}

func TestStaleInterval(t *testing.T) {
	fr := &fakeReader{data: map[int]ProcResources{
		1: {PID: 1, CPUTimeSecs: 1, RSSBytes: 1, OK: true},
	}}
	var clock time.Time
	s := &Sampler{
		Reader: fr, MinInterval: time.Second,
		Now: func() time.Time { return clock },
	}
	clock = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_ = s.SamplePIDs([]int{1}, false, clock)
	clock = clock.Add(10 * time.Millisecond)
	stale := s.SamplePIDs([]int{1}, false, clock)
	if stale.Quality != QualityStale {
		t.Fatalf("%#v", stale)
	}
	if len(fr.calls) != 1 {
		t.Fatalf("stale should not re-read: calls=%d", len(fr.calls))
	}
}

func TestResetClearsRateState(t *testing.T) {
	fr := &fakeReader{data: map[int]ProcResources{
		1: {PID: 1, CPUTimeSecs: 1, RSSBytes: 1, OK: true},
	}}
	var clock time.Time
	s := &Sampler{Reader: fr, MinInterval: time.Millisecond, Now: func() time.Time { return clock }}
	clock = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_ = s.SamplePIDs([]int{1}, false, clock)
	s.Reset()
	fr.data[1] = ProcResources{PID: 1, CPUTimeSecs: 5, RSSBytes: 1, OK: true}
	clock = clock.Add(time.Second)
	s2 := s.SamplePIDs([]int{1}, false, clock)
	if s2.HasCPURate {
		t.Fatal("after reset first sample has no rate")
	}
}

func TestNoneSampledUnavailable(t *testing.T) {
	s := &Sampler{
		Reader:      &fakeReader{data: map[int]ProcResources{}},
		MinInterval: time.Millisecond,
	}
	sample := s.SamplePIDs([]int{1}, false, time.Now())
	if sample.Quality != QualityUnavailable {
		t.Fatalf("%#v", sample)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func hasCrossing(cs []Crossing, m MetricName, d CrossingDirection) bool {
	for _, c := range cs {
		if c.Metric == m && c.Direction == d {
			return true
		}
	}
	return false
}
