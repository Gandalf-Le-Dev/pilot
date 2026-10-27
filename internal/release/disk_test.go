package release

import "testing"

func TestParseDiskUsed(t *testing.T) {
	const header = "Filesystem 1024-blocks Used Available Capacity Mounted on\n"
	tests := []struct {
		name string
		out  string
		want int
		ok   bool
	}{
		{"normal df output", header + "/dev/sda1 41251136 13320520 25811560 34% /", 34, true},
		{"nearly full", header + "/dev/sda1 100 95 5 95% /opt", 95, true},
		{"full", header + "/dev/sda1 100 100 0 100% /opt", 100, true},
		{"empty volume", header + "/dev/sda1 100 0 100 0% /opt", 0, true},
		{"trailing newline", header + "/dev/sda1 100 34 66 34% /\n", 34, true},
		{"header only", header, 0, false},
		{"empty", "", 0, false},
		{"garbage", "not\ndf output", 0, false},
		{"dash percent", header + "none 0 0 0 -% /", 0, false},
		{"negative", header + "/dev/sda1 100 86 14 -5% /", 0, false},
		{"over 100", header + "/dev/sda1 100 86 14 105% /", 0, false},
		{
			"two volumes",
			header + "/dev/sdb1 100 95 5 95% /opt/pilot\n" + header + "/dev/sda1 100 34 66 34% /",
			0, false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseDiskUsed(tc.out)
			if ok != tc.ok || got != tc.want {
				t.Errorf("ParseDiskUsed = %d, %v; want %d, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}
