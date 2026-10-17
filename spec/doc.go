// Package spec implements conda's version ordering, version specs, build-string
// matchers, build-number specs, match specs, platforms and channel names,
// ported from libmamba's specs and checked against rattler_conda_types, conda
// itself (through rattler's corpus of versions parsed by conda) and
// micromamba.
//
// The main types are:
//
//   - Version, from ParseVersion: "1!2.0.3a1.post2+local.1". Compare orders
//     versions; "1.0" equals "1.0.0".
//   - VersionSpec, from ParseVersionSpec: ">=1.2,<2|1.0.*".
//   - StringMatcher (build strings), BuildNumberSpec and GlobSpec (names).
//   - MatchSpec, from ParseMatchSpec: "conda-forge::numpy >=1.2 py311*".
//   - Channel, from ParseChannel: "conda-forge" to its base URL.
//
// Version.Compare, VersionSpec.Contains and MatchSpec.MatchesVersion are the
// solver's hot paths and never allocate.
//
// # Hostile input
//
// Parsers accept only printable ASCII where conda does and reject numbers
// that do not fit in 64 bits. Versions are limited to 256 bytes and 64
// parts, package names and build globs to 256 bytes, build regular
// expressions to 64 bytes, and version specs, match specs and channels to
// 1024 bytes, with at most 16 glob predicates per version spec (the longest
// real version, build and match spec have 55, under 100 and 227 bytes).
// Names and build strings longer than 256 bytes never match. Expression
// nesting is bounded, globs match without backtracking, regular expressions
// run in linear time and may not use counted repetition. Every value's
// String form parses back to an equal value. Channels carrying credentials
// or tokens are refused.
//
// # Semantics
//
// Matching follows conda, or libsolv where micromamba, which goomba is
// tested against, uses libsolv (for the dependencies of repodata records):
//
//   - "," binds tighter than "|" in version specs, as in conda and libsolv;
//     libmamba's own parser reads "a,b|c" as "a,(b|c)".
//   - ">=X.*" means ">=X", and ">X.*", "<X.*", "<=X.*" and "~=X.*" match
//     nothing, as in libsolv (libmamba compares against a version ending in
//     "*", which sorts lowest; conda reads them differently again and
//     rejects "~=X.*"). "==X.*" means "=X", as in libmamba and libsolv;
//     conda compares exactly instead.
//   - Starts-with ("=1.0", "1.0.*") requires every part of the prefix but
//     the last to be equal, as conda and libsolv do (libmamba lets "1.0a1"
//     start with "1.0.0"). As in libsolv but not conda, the prefix's final
//     literal must match exactly: "=1.2a" does not accept "1.2alpha".
//   - Build strings are literal globs where only '*' is special; only
//     "^...$" is a regular expression (libmamba also treats "abc+def" or
//     "py3.10_0" as regular expressions).
//   - Unknown bracket keys are errors; libmamba ignores them, so
//     "foo[versoin='<1']" matched everything.
//   - Version globs such as "*.3" or "1.*.3" match whole parts, each "*"
//     standing for one or more parts, and find a match wherever one exists
//     (libmamba anchors each literal run at its first occurrence and so
//     rejects "3.3" for "*.3"). Unlike conda, which matches the version text
//     with a regular expression, they never cross epochs or local versions.
//   - Package names are limited to [A-Za-z0-9._-] and '*', like rattler's,
//     which rejects pip extras such as "dask[array]".
//
// Kept from libmamba although conda differs:
//
//   - Some versions conda rejects are accepted: a trailing '_' may follow
//     '-' separators ("1-1_", "9-0-0-0_"), and spaces may follow an
//     operator (">= 1.0").
//   - In "foo <1[version='>=2']" the version before the brackets wins;
//     conda lets the bracket win. Of two equal bracket keys the last wins.
//   - "~=1.0.0" requires 1.0.0 <= v and v to start with "1.0" part by part,
//     so it rejects "1.0post1", which conda accepts.
//   - "conda-forge:foo" (one colon) is a namespace, not a channel; conda
//     drops it. Both match foo from any channel.
//   - "=*" and "==*" match every version (conda matches none), and version
//     specs written as regular expressions ("^1.0$") are not supported.
//   - A single trailing '.' is ignored ("1.0." means "1.0"), for broken
//     conda-forge repodata.
//
// All types are immutable values once parsed and are safe for concurrent
// use.
package spec
