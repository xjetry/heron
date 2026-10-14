package update

import "testing"

func TestValidVersion(t *testing.T) {
	for _, value := range []string{"v0.0.0", "v0.2.0", "v12.34.56", "v4294967295.4294967295.4294967295"} {
		if !ValidVersion(value) {
			t.Errorf("rejected stable version %q", value)
		}
	}
	for _, value := range []string{"", "0.2.0", "V0.2.0", "v01.2.0", "v1.02.0", "v1.2.00", "v1.2", "v1.2.3.4", "v1.2.3-rc1", "v1.2.3+build", "v1.2.3\n", "v+1.2.3", "v-1.2.3", "v1..3", "v4294967296.0.0", "v99999999999.0.0", "v１.2.3", "v1/2.3.4"} {
		if ValidVersion(value) {
			t.Errorf("accepted invalid version %q", value)
		}
	}
}

func TestReleaseTag(t *testing.T) {
	for _, c := range []struct {
		tag, core string
		pre, ok   bool
	}{
		{"v1.2.3", "v1.2.3", false, true},
		{"v1.2.3-rc.1", "v1.2.3", true, true},
		{"v1.2.3-0", "v1.2.3", true, true},
		{"v1.2.3-alpha-1.x.7", "v1.2.3", true, true},
		{"", "", false, false},
		{"1.2.3", "", false, false},
		{"v1.2", "", false, false},
		{"v01.2.3", "", false, false},
		{"v1.2.3-", "", false, false},
		{"v1.2.3-rc..1", "", false, false},
		{"v1.2.3-01", "", false, false},
		{"v1.2.3-rc_1", "", false, false},
		{"v1.2.3+b.1", "", false, false},
		{"v1.2.3-rc.1+b.1", "", false, false},
		{"dev", "", false, false},
	} {
		core, pre, ok := ReleaseTag(c.tag)
		if core != c.core || pre != c.pre || ok != c.ok {
			t.Errorf("ReleaseTag(%q) = %q, %v, %v; want %q, %v, %v", c.tag, core, pre, ok, c.core, c.pre, c.ok)
		}
	}
}

func TestNewer(t *testing.T) {
	for _, tc := range []struct {
		target, current string
		want            bool
	}{
		{"v1.0.0", "v0.99.99", true}, {"v1.2.0", "v1.1.99", true}, {"v1.2.10", "v1.2.9", true},
		{"v1.0.0", "v1.0.0", false}, {"v1.99.99", "v2.0.0", false}, {"v1.2.99", "v1.3.0", false}, {"v1.2.2", "v1.2.3", false},
		{"v1.2.3", "dev", false}, {"v1.2.3-rc1", "v1.2.2", false},
	} {
		if got := Newer(tc.target, tc.current); got != tc.want {
			t.Errorf("Newer(%q,%q)=%v want %v", tc.target, tc.current, got, tc.want)
		}
	}
}
