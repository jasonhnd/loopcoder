package conductorhooks

import "testing"

// TestPendingRecordsOrderByRecordedAtThenID locks the fix that makes the relay
// guard print multiple missing ledger blocks in first-seen (RecordedAt) order,
// mirroring the JS hook's insertion-order iteration over its state object, with
// the record id as a deterministic tiebreak for records recorded at the same
// instant. Non-pending records are excluded.
func TestPendingRecordsOrderByRecordedAtThenID(t *testing.T) {
	state := &relayState{
		Records: map[string]*relayRecord{
			"zzz": {ID: "zzz", Status: "pending", RecordedAt: "2026-07-01T00:00:02.000Z"},
			"aaa": {ID: "aaa", Status: "pending", RecordedAt: "2026-07-01T00:00:01.000Z"},
			"mmm": {ID: "mmm", Status: "surfaced", RecordedAt: "2026-07-01T00:00:00.000Z"},
			"bbb": {ID: "bbb", Status: "pending", RecordedAt: "2026-07-01T00:00:01.000Z"},
		},
	}

	got := pendingRecords(state)
	want := []string{"aaa", "bbb", "zzz"}
	if len(got) != len(want) {
		t.Fatalf("pendingRecords returned %d records, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i] {
			ids := make([]string, len(got))
			for j, r := range got {
				ids[j] = r.ID
			}
			t.Fatalf("pendingRecords order = %v, want %v", ids, want)
		}
	}
}
