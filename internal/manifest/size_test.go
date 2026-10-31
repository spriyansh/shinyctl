package manifest

import "testing"

func TestParseSize(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
		bad  bool
	}{
		{in: "512M", want: 512 << 20},
		{in: "512MB", want: 512 << 20},
		{in: "512Mi", want: 512 << 20},
		{in: "512MiB", want: 512 << 20},
		{in: "512m", want: 512 << 20},
		{in: "512mib", want: 512 << 20},
		{in: "64K", want: 64 << 10},
		{in: "2G", want: 2 << 30},
		{in: "2gi", want: 2 << 30},
		{in: "1T", want: 1 << 40},
		{in: "1.5Gi", want: 3 << 29},
		{in: "0.5M", want: 1 << 19},
		{in: " 512M ", want: 512 << 20},
		{in: "0M", want: 0},
		{in: "0.0000001M", want: 0}, // a tenth of a byte
		{in: "0.0001M", want: 105},
		{in: "512", bad: true},
		{in: "0", want: 0},
		{in: "", bad: true},
		{in: "M", bad: true},
		{in: "-5M", bad: true},
		{in: "+5M", bad: true},
		{in: "5 M", bad: true},
		{in: "1e3M", bad: true},
		{in: "5Mx", bad: true},
		{in: "5MBB", bad: true},
		{in: "5iM", bad: true},
		{in: "5P", bad: true},
		{in: ".5M", bad: true},
		{in: "8388607T", want: 8388607 << 40},
		{in: "8388608T", bad: true}, // 2^63 bytes
		{in: "99999999999T", bad: true},
	} {
		got, err := ParseSize(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("ParseSize(%q) = %d, want an error", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", tc.in, got, err, tc.want)
		}
	}
}
