package solve

import "testing"

func TestRequestedPythonMinor(t *testing.T) {
	// Cases from libmamba tests/src/core/test_shard_python_minor_prefilter.cpp
	// (TEST_CASE "extract_requested_python_minor").
	for _, c := range []struct {
		specs []string
		want  string
	}{
		{nil, ""}, // empty specs
		{[]string{"numpy >=1.0", "openssl 3"}, ""},   // no python package
		{[]string{"python 3.12.5"}, "3.12"},          // two-token exact pin relaxes to minor
		{[]string{"python 3.7.12 0_73_pypy"}, "3.7"}, // three-token conda pin
		{[]string{"python ==3.11"}, "3.11"},          // explicit equality operator form
		{[]string{"python=3.13"}, "3.13"},            // single equals conda pin form
		{[]string{"python=2"}, ""},                   // major-only pin yields no minor
		{[]string{"python=3"}, ""},
		{[]string{"python >=3.12,<3.13"}, ""},                           // range spec
		{[]string{"numpy 1.0", "python 3.10.0", "python 3.11"}, "3.10"}, // first matching python spec
		{[]string{"python"}, ""},                                        // bare python name
	} {
		if got := RequestedPythonMinor(c.specs); got != c.want {
			t.Errorf("RequestedPythonMinor(%q) = %q, want %q", c.specs, got, c.want)
		}
	}
}

func TestRequestedPythonMinorMore(t *testing.T) {
	for _, c := range []struct {
		specs []string
		want  string
	}{
		{[]string{"python=3.11.*"}, "3.11"},
		{[]string{"python 3.11.*"}, "3.11"},
		{[]string{"python=3.13 *_cp313t"}, "3.13"},
		{[]string{"python=3.11.4=h955ad1f_0_cpython"}, "3.11"},
		// Pre-release pins sort below X.Y, outside the series the prefilter
		// keeps (WP7 review); libmamba gives "3.11" for them.
		{[]string{"python ==3.11.0rc1"}, ""},
		{[]string{"python=3.11.0a1"}, ""},
		{[]string{"python=3.11.dev0"}, ""},
		{[]string{"python 3.11.0b2"}, ""},
		{[]string{"python=3.11.0"}, "3.11"},
		{[]string{"python=3.11.0.post1"}, "3.11"},
		{[]string{"python 3"}, ""},
		{[]string{"python=3", "python=3.11"}, ""}, // a major-only pin stops the search, as in libmamba
		{[]string{"python", "python=3.11"}, "3.11"},
		{[]string{"python >=3.11", "python=3.12"}, "3.12"},
		{[]string{"python ~=3.11"}, ""},
		{[]string{"python !=3.11"}, ""},
		{[]string{"python=1!3.11"}, ""}, // libmamba: 1!3.11, not a MAJOR.MINOR
		{[]string{"python=3.11+local"}, ""},
		{[]string{"python=3.11|3.12"}, ""},
		{[]string{"conda-forge::python=3.11"}, ""}, // libmamba's extract_name keeps the channel
		{[]string{"python[version='3.11.*']"}, ""}, // not a "python" name for extract_name
		{[]string{"python=3.11a"}, ""},
		{[]string{"PYTHON=3.11"}, "3.11"},
		{[]string{"python=3.11", "garbage[["}, "3.11"},
		{[]string{"python[[", "python=3.12"}, "3.12"},
	} {
		if got := RequestedPythonMinor(c.specs); got != c.want {
			t.Errorf("RequestedPythonMinor(%q) = %q, want %q", c.specs, got, c.want)
		}
	}
}
