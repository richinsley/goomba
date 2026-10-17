package spec

import (
	"errors"
	"reflect"
	"testing"
)

// Cases from libmamba tests/src/specs/test_unresolved_channel.cpp
// ("Parsing", "str"). Windows-only cases are skipped. Upstream's sample
// home-directory paths are rewritten under /srv, since the repository may
// not contain home-directory paths.
func TestParseUnresolvedChannel(t *testing.T) {
	for _, s := range []string{"", "<unknown>", ":///<unknown>", "none"} {
		uc, err := parseUnresolvedChannel(s)
		if err != nil || uc.kind != chanUnknown || uc.location != "<unknown>" || len(uc.platforms) != 0 {
			t.Errorf("%q: %+v %v", s, uc, err)
		}
	}
	if _, err := parseUnresolvedChannel("forgelinux-64]"); !errors.Is(err, ErrChannel) {
		t.Errorf("forgelinux-64]: err = %v", err)
	}

	cases := []struct {
		in       string
		kind     channelKind
		location string
		plats    []string
	}{
		{"https://repo.anaconda.com/conda-forge", chanURL, "https://repo.anaconda.com/conda-forge", nil},
		{"https://repo.anaconda.com/conda-forge/osx-64", chanURL, "https://repo.anaconda.com/conda-forge", []string{"osx-64"}},
		{"https://repo.anaconda.com/conda-forge[win-64|noarch]", chanURL, "https://repo.anaconda.com/conda-forge", []string{"noarch", "win-64"}},
		{"https://repo.anaconda.com/conda-forge/linux-64/pkg-0.0-bld.conda", chanPackageURL, "https://repo.anaconda.com/conda-forge/linux-64/pkg-0.0-bld.conda", nil},
		{"file:///srv/name/conda", chanPath, "file:///srv/name/conda", nil},
		{"file:///srv/name/conda[linux-64]", chanPath, "file:///srv/name/conda", []string{"linux-64"}},
		{"/srv/name/conda", chanPath, "/srv/name/conda", nil},
		{"./folder/../folder/.", chanPath, "./folder", nil},
		{"./folder/subfolder/", chanPath, "./folder/subfolder", nil},
		{"~/folder/", chanPath, "~/folder", nil},
		{"/tmp/pkg-0.0-bld.tar.bz2", chanPackagePath, "/tmp/pkg-0.0-bld.tar.bz2", nil},
		{"pkg-0.0-bld.conda", chanPackagePath, "pkg-0.0-bld.conda", nil},
		{"nested/pkg-0.0-bld.tar.bz2", chanPackagePath, "nested/pkg-0.0-bld.tar.bz2", nil},
		{"C:/tmp//pkg-0.0-bld.tar.bz2", chanPackagePath, "C:/tmp/pkg-0.0-bld.tar.bz2", nil},
		{"conda-forge", chanName, "conda-forge", nil},
		// Unintuitive but correct type, this is not a URL.
		{"repo.anaconda.com", chanName, "repo.anaconda.com", nil},
		{"conda-forge/linux-64", chanName, "conda-forge", []string{"linux-64"}},
		{"conda-forge[linux-avx512]", chanName, "conda-forge", []string{"linux-avx512"}},
		{"conda-forge[linux-64]", chanName, "conda-forge", []string{"linux-64"}},
		{"conda-forge/linux-64/label/foo_dev", chanName, "conda-forge/label/foo_dev", []string{"linux-64"}},
		{"conda-forge/label/foo_dev[linux-64]", chanName, "conda-forge/label/foo_dev", []string{"linux-64"}},
	}
	for _, c := range cases {
		uc, err := parseUnresolvedChannel(c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if uc.kind != c.kind || uc.location != c.location || !reflect.DeepEqual(uc.platforms, c.plats) {
			t.Errorf("%q: got kind %d location %q platforms %v", c.in, uc.kind, uc.location, uc.platforms)
		}
	}

	if s := (unresolvedChannel{location: "location", kind: chanName}).String(); s != "location" {
		t.Errorf("str = %q", s)
	}
	if s := (unresolvedChannel{location: "location", platforms: []string{"linux-64", "noarch"}, kind: chanName}).String(); s != "location[linux-64,noarch]" {
		t.Errorf("str = %q", s)
	}
}

// The name and URL cases of libmamba tests/src/specs/test_channel.cpp
// ("Channel::resolve"). Cases about paths, authentication, custom channels
// and multichannels are out of scope for ParseChannel (see the report).
func TestParseChannel(t *testing.T) {
	cases := []struct {
		in, alias, url, name string
		plats                []string
	}{
		{"conda-forge", "https://conda.anaconda.org/", "https://conda.anaconda.org/conda-forge", "conda-forge", nil},
		{"conda-forge", "mydomain.com/private", "https://mydomain.com/private/conda-forge", "conda-forge", nil},
		{"pkgs/main", DefaultChannelAlias, "https://repo.anaconda.com/pkgs/main", "pkgs/main", nil},
		{"pkgs/main/label/dev", DefaultChannelAlias, "https://repo.anaconda.com/pkgs/main/label/dev", "pkgs/main/label/dev", nil},
		{"testchannel/mylabel/xyz", "https://conda.anaconda.org/", "https://conda.anaconda.org/testchannel/mylabel/xyz", "testchannel/mylabel/xyz", nil},
		{"prefix-and-more", "https://ali.as/", "https://ali.as/prefix-and-more", "prefix-and-more", nil},
		{"https://repo.mamba.pm", DefaultChannelAlias, "https://repo.mamba.pm", "https://repo.mamba.pm", nil},
		{"https://repo.mamba.pm/", DefaultChannelAlias, "https://repo.mamba.pm", "https://repo.mamba.pm", nil},
		{"https://repo.mamba.pm/conda-forge", DefaultChannelAlias, "https://repo.mamba.pm/conda-forge", "https://repo.mamba.pm/conda-forge", nil},
		{"https://repo.mamba.pm/conda-forge[linux-64,noarch]", DefaultChannelAlias, "https://repo.mamba.pm/conda-forge", "https://repo.mamba.pm/conda-forge", []string{"linux-64", "noarch"}},
		{"https://repo.mamba.pm/conda-forge", "https://repo.mamba.pm/", "https://repo.mamba.pm/conda-forge", "conda-forge", nil},
		{"https://repo.mamba.pm/conda-forge", "https://repo.mamba.pm", "https://repo.mamba.pm/conda-forge", "conda-forge", nil},
		{"https://repo.mamba.pm/conda-forge", "repo.mamba.pm", "https://repo.mamba.pm/conda-forge", "conda-forge", nil},
		{"https://repo.mamba.pm/conda-forge", "repo.anaconda.com", "https://repo.mamba.pm/conda-forge", "https://repo.mamba.pm/conda-forge", nil},
		{"https://repo.mamba.pm/conda-forge", "ftp://repo.mamba.pm", "https://repo.mamba.pm/conda-forge", "https://repo.mamba.pm/conda-forge", nil},
		{"https://repo.anaconda.com/pkgs/main", DefaultChannelAlias, "https://repo.anaconda.com/pkgs/main", "pkgs/main", nil},
		{"https://repo.anaconda.com/pkgs/main", "https://repo.anaconda.com", "https://repo.anaconda.com/pkgs/main", "pkgs/main", nil},
		{"file:///some/folder/", DefaultChannelAlias, "file:///some/folder", "file:///some/folder", nil},
		{"ftp://mamba.org/some/folder", DefaultChannelAlias, "ftp://mamba.org/some/folder", "ftp://mamba.org/some/folder", nil},
		// Added for goomba.
		{"conda-forge/linux-64", DefaultChannelAlias, "https://conda.anaconda.org/conda-forge", "conda-forge", []string{"linux-64"}},
		{"conda-forge/label/dev", DefaultChannelAlias, "https://conda.anaconda.org/conda-forge/label/dev", "conda-forge/label/dev", nil},
		{"HTTPS://Conda.Anaconda.ORG/conda-forge/noarch/", DefaultChannelAlias, "https://conda.anaconda.org/conda-forge", "conda-forge", []string{"noarch"}},
		{" conda-forge ", DefaultChannelAlias, "https://conda.anaconda.org/conda-forge", "conda-forge", nil},
	}
	for _, c := range cases {
		ch, err := ParseChannelAlias(c.in, c.alias)
		if err != nil {
			t.Errorf("ParseChannelAlias(%q, %q): %v", c.in, c.alias, err)
			continue
		}
		want := Channel{URL: c.url, Name: c.name, Platforms: c.plats}
		if !reflect.DeepEqual(ch, want) {
			t.Errorf("ParseChannelAlias(%q, %q) = %+v, want %+v", c.in, c.alias, ch, want)
		}
	}
	if ch, err := ParseChannel("conda-forge"); err != nil || ch.URL != "https://conda.anaconda.org/conda-forge" {
		t.Errorf("ParseChannel(conda-forge) = %+v, %v", ch, err)
	}

	bad := []struct{ in, alias string }{
		{"", DefaultChannelAlias},
		{"<unknown>", DefaultChannelAlias},
		{"defaults", DefaultChannelAlias},
		{"/some/folder", DefaultChannelAlias},
		{"./other/folder", DefaultChannelAlias},
		{"~/folder", DefaultChannelAlias},
		{"https://repo.mamba.pm/conda-forge/linux-64/libmamba-1.4.2-hcea66bb_0.conda", DefaultChannelAlias},
		{"https://user:pass@repo.mamba.pm/conda-forge", DefaultChannelAlias},
		{"https://repo.mamba.pm/t/mytoken/conda-forge", DefaultChannelAlias},
		{"https://repo.mamba.pm/conda-forge?x=1", DefaultChannelAlias},
		{"https://repo.mamba.pm/conda forge", DefaultChannelAlias},
		{"https:///conda-forge", DefaultChannelAlias},
		{"../evil", DefaultChannelAlias},
		{"conda-forge/../evil", DefaultChannelAlias},
		{"conda forge", DefaultChannelAlias},
		{"conda-forge", ""},
		{"conda-forge", "https://user:pw@host"},
	}
	for _, c := range bad {
		if ch, err := ParseChannelAlias(c.in, c.alias); !errors.Is(err, ErrChannel) {
			t.Errorf("ParseChannelAlias(%q, %q) = %+v, %v; want ErrChannel", c.in, c.alias, ch, err)
		}
	}
}
