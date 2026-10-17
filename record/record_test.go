package record

import "testing"

func TestDistAndVirtual(t *testing.T) {
	r := &PackageRecord{Name: "python", Version: "3.11.15", Build: "hd63d673_0_cpython"}
	if got, want := r.Dist(), "python-3.11.15-hd63d673_0_cpython"; got != want {
		t.Fatalf("Dist() = %q, want %q", got, want)
	}
	if r.IsVirtual() {
		t.Fatal("python is not virtual")
	}
	if !(&PackageRecord{Name: "__glibc"}).IsVirtual() {
		t.Fatal("__glibc is virtual")
	}
	if (&PackageRecord{Name: "_openmp_mutex"}).IsVirtual() {
		t.Fatal("_openmp_mutex is not virtual")
	}
}

func TestNormalizeTimestamp(t *testing.T) {
	cases := []struct{ in, want int64 }{
		{0, 0},
		{1772730362, 1772730362000},     // seconds
		{1772730362000, 1772730362000},  // already milliseconds
		{253402300799, 253402300799000}, // largest value treated as seconds
		{253402300800, 253402300800},    // first value treated as milliseconds
	}
	for _, c := range cases {
		if got := NormalizeTimestamp(c.in); got != c.want {
			t.Errorf("NormalizeTimestamp(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}
