package update

import "testing"

func TestIsNewer(t *testing.T) {
	cases := []struct {
		name    string
		latest  string
		current string
		want    bool
	}{
		{"equal", "v1.2.3", "v1.2.3", false},
		{"patch_newer", "v1.2.4", "v1.2.3", true},
		{"minor_newer", "v1.3.0", "v1.2.5", true},
		{"major_newer", "v2.0.0", "v1.9.9", true},
		{"older", "v1.2.2", "v1.2.3", false},
		{"dev_current", "v1.2.3", "dev", true},
		{"invalid_latest", "not-a-version", "v1.2.3", false},
		{"invalid_current", "v1.2.3", "not-a-version", false},
		{"both_invalid", "nope", "nope", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isNewer(tc.latest, tc.current)
			if got != tc.want {
				t.Errorf("isNewer(%q, %q) = %v, want %v", tc.latest, tc.current, got, tc.want)
			}
		})
	}
}
