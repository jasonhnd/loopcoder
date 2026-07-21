package proctelemetry

import "testing"

func TestParsePSTime(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"00:01", 1},
		{"01:02", 62},
		{"1:02:03", 3723},
		{"1-00:00:01", 86401},
	}
	for _, tc := range cases {
		got, err := parsePSTime(tc.in)
		if err != nil || got != tc.want {
			t.Fatalf("%q => %v err=%v want %v", tc.in, got, err, tc.want)
		}
	}
}
