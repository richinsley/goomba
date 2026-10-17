package spec

import (
	"testing"

	"github.com/richinsley/goomba/record"
)

func BenchmarkVersionCompare(b *testing.B) {
	pairs := [][2]Version{
		{mustVersion(b, "3.11.15"), mustVersion(b, "3.11.9")},
		{mustVersion(b, "1.26.4"), mustVersion(b, "1.26.4.0")},
		{mustVersion(b, "2.0.0rc1"), mustVersion(b, "2.0.0.post1")},
		{mustVersion(b, "1!2.0+local.3"), mustVersion(b, "1!2.0+local.2")},
	}
	b.ReportAllocs()
	b.ResetTimer()
	n := 0
	for i := 0; i < b.N; i++ {
		p := &pairs[i&3]
		n += p[0].Compare(p[1])
	}
	_ = n
}

func BenchmarkVersionSpecContains(b *testing.B) {
	specs := []VersionSpec{
		mustVersionSpec(b, ">=1.21,<2.0a0"),
		mustVersionSpec(b, "3.11.*"),
		mustVersionSpec(b, ">=2.7,!=3.0.*,!=3.1.*,!=3.2.*,!=3.3.*,<4"),
		mustVersionSpec(b, "1.7.*|>=1.9,<2"),
	}
	v := mustVersion(b, "1.26.4")
	b.ReportAllocs()
	b.ResetTimer()
	n := 0
	for i := 0; i < b.N; i++ {
		if specs[i&3].Contains(v) {
			n++
		}
	}
	_ = n
}

func BenchmarkMatchSpecMatchesVersion(b *testing.B) {
	specs := []MatchSpec{
		mustMatchSpec(b, "numpy >=1.21,<2.0a0"),
		mustMatchSpec(b, "numpy 1.26.* py311*"),
		mustMatchSpec(b, "conda-forge::numpy[version='>=1.2',build_number='>=0']"),
		mustMatchSpec(b, "numpy"),
	}
	r := &record.PackageRecord{Name: "numpy", Version: "1.26.4", Build: "py311h64a7726_0", Subdir: "linux-64"}
	v := mustVersion(b, r.Version)
	b.ReportAllocs()
	b.ResetTimer()
	n := 0
	for i := 0; i < b.N; i++ {
		if specs[i&3].MatchesVersion(r, v) {
			n++
		}
	}
	_ = n
}

func BenchmarkParseVersion(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := ParseVersion("3.11.15"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseMatchSpec(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := ParseMatchSpec("openssl >=3.5.5,<4.0a0"); err != nil {
			b.Fatal(err)
		}
	}
}

// TestHotPathsDoNotAllocate guards the zero-allocation promise of Compare,
// Contains and MatchesVersion.
func TestHotPathsDoNotAllocate(t *testing.T) {
	a, b := mustVersion(t, "1!2.0.0rc1+local.3"), mustVersion(t, "1!2.0.0.post1+local.2")
	vs := mustVersionSpec(t, "(>=1.21,<2.0a0)|1.7.*|~=1.26.0|*.4|!=1.*.3")
	ms := mustMatchSpec(t, "numpy[version='>=1.2,<2',build='py311*',build_number='>=0',subdir='linux-64,noarch',track_features='mkl',md5=abc,sha256=def,license=BSD]")
	r := &record.PackageRecord{Name: "numpy", Version: "1.26.4", Build: "py311h64a7726_0", Subdir: "noarch", TrackFeatures: []string{"mkl"}, MD5: "ABC", SHA256: "def", License: "BSD"}
	v := mustVersion(t, r.Version)
	cases := []struct {
		name string
		f    func()
	}{
		{"Compare", func() { _ = a.Compare(b) }},
		{"StartsWith", func() { _ = a.StartsWith(b) }},
		{"CompatibleWith", func() { _ = a.CompatibleWith(b) }},
		{"Contains", func() { _ = vs.Contains(v) }},
		{"MatchesVersion", func() { _ = ms.MatchesVersion(r, v) }},
	}
	if !ms.MatchesVersion(r, v) {
		t.Fatal("benchmark spec does not match")
	}
	for _, c := range cases {
		if n := testing.AllocsPerRun(100, c.f); n != 0 {
			t.Errorf("%s allocates %v times", c.name, n)
		}
	}
}
