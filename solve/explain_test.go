package solve

import "testing"

func TestFoldConflictLeaves(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", ""},
		{"No candidates were found for foo *.\n", "No candidates were found for foo *.\n"},
		{
			// A single leaf is kept as it is.
			"└─ foo 1, which conflicts with the versions reported above.\n",
			"└─ foo 1, which conflicts with the versions reported above.\n",
		},
		{
			"├─ foo 2, which conflicts with the versions reported above.\n" +
				"├─ foo 1.0, which conflicts with the versions reported above.\n" +
				"└─ foo 1, which conflicts with the versions reported above.\n",
			"└─ foo 1.0 | 2, which conflicts with the versions reported above.\n",
		},
		{
			// Runs stop at another package, another level, a non-leaf line,
			// or after the last sibling.
			"│  ├─ foo 2, which conflicts with the versions reported above.\n" +
				"│  ├─ foo 1, which conflicts with the versions reported above.\n" +
				"│  ├─ bar 1, which conflicts with the versions reported above.\n" +
				"│  ├─ bar 2 | 3, which conflicts with the versions reported above.\n" +
				"│  │  ├─ bar 4, which conflicts with the versions reported above.\n" +
				"│  ├─ bar 5 would require\n" +
				"│  └─ bar 6, which conflicts with the versions reported above.\n" +
				"│  └─ bar 7, which conflicts with the versions reported above.\n",
			"│  ├─ foo 1 | 2, which conflicts with the versions reported above.\n" +
				"│  ├─ bar 1 | 2 | 3, which conflicts with the versions reported above.\n" +
				"│  │  ├─ bar 4, which conflicts with the versions reported above.\n" +
				"│  ├─ bar 5 would require\n" +
				"│  └─ bar 6, which conflicts with the versions reported above.\n" +
				"│  └─ bar 7, which conflicts with the versions reported above.\n",
		},
		{
			// Many versions are abbreviated; already abbreviated lines are
			// left alone.
			"   ├─ p 3.10.2, which conflicts with the versions reported above.\n" +
				"   ├─ p 3.10.5, which conflicts with the versions reported above.\n" +
				"   ├─ p 3.10.10, which conflicts with the versions reported above.\n" +
				"   ├─ p 3.10.1, which conflicts with the versions reported above.\n" +
				"   ├─ p 3.10.1, which conflicts with the versions reported above.\n" +
				"   ├─ p 3.10.0, which conflicts with the versions reported above.\n" +
				"   ├─ q 1 | 2 | ... | 9, which conflicts with the versions reported above.\n" +
				"   └─ q 10, which conflicts with the versions reported above.\n",
			"   ├─ p 3.10.0 | 3.10.1 | ... | 3.10.10, which conflicts with the versions reported above.\n" +
				"   ├─ q 1 | 2 | ... | 9, which conflicts with the versions reported above.\n" +
				"   └─ q 10, which conflicts with the versions reported above.\n",
		},
		{
			// Unparsable versions are left alone; no trailing newline.
			"├─ foo 1..2, which conflicts with the versions reported above.\n" +
				"└─ foo 1, which conflicts with the versions reported above.",
			"├─ foo 1..2, which conflicts with the versions reported above.\n" +
				"└─ foo 1, which conflicts with the versions reported above.",
		},
	} {
		if got := foldConflictLeaves(c.in); got != c.want {
			t.Errorf("foldConflictLeaves(\n%s) =\n%s\nwant:\n%s", c.in, got, c.want)
		}
	}
}
