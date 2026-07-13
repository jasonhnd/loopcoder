//go:build linux

package supervisedexec

import "testing"

func TestParseLinuxProcStatHandlesProcessNamesWithSpacesAndParens(t *testing.T) {
	line := "4321 (worker (quiet test)) S 1234 777 777 0 -1 4194304 50 0 1 0 42 7 3 2 20 0 4 0 987654 123 456"

	stat, err := parseLinuxProcStat(line)
	if err != nil {
		t.Fatalf("parseLinuxProcStat returned error: %v", err)
	}
	if stat.pid != 4321 || stat.ppid != 1234 || stat.pgrp != 777 {
		t.Fatalf("identity fields = pid:%d ppid:%d pgrp:%d, want 4321/1234/777", stat.pid, stat.ppid, stat.pgrp)
	}
	if stat.utime != 42 || stat.stime != 7 || stat.cutime != 3 || stat.cstime != 2 || stat.starttime != 987654 {
		t.Fatalf("activity fields = %#v, want parsed jiffies and starttime", stat)
	}
}
