// Package solve resolves conda match specs to an exact set of package records,
// using package cdcl with conda's rules (candidate order, constrains, virtual
// packages, strict channel priority) and explains unsolvable requests.
//
// It is a port of rattler's resolvo backend (crates/rattler_solve: the
// CondaDependencyProvider of src/resolvo/mod.rs and the candidate order of
// src/resolvo/conda_sorting.rs, BSD-3-Clause, see NOTICE), checked against
// rattler's tests and snapshots and against micromamba.
//
// # Rules
//
// Solve returns one record per package name such that:
//
//   - every spec of Request.Specs is satisfied;
//   - every depends of every selected record is satisfied;
//   - every constrains of a selected record, and every
//     Request.Constraints entry, holds for the package it names whenever
//     that package is selected (nothing requires it to be);
//   - packages whose names start with "__" are the virtual packages of
//     Request.Virtual, never records of the source; each of them counts as
//     installed, so constrains on it apply, and a dependency on one the
//     machine lacks makes the record that has it uninstallable.
//
// Matching goes through package spec. A channel-qualified spec
// ("conda-forge::numpy") matches only records whose Channel is the spec's
// channel resolved with Request.ChannelAlias; a package URL spec matches
// only the record with that URL. Virtual packages are matched on their
// version and build string only, as in rattler.
//
// Strict channel priority is the source's job (repodata.Multi answers each
// package name from the first channel that lists it); the solver never
// mixes channels for one name except for Request.Favored and
// Request.Locked records, which the caller supplies.
//
// Records the solver must not install stay visible as excluded candidates,
// so explanations say why they were not used:
//
//   - a record without a sha256 (or with a malformed one): "no sha256:
//     cannot be verified";
//   - a record with a depends or constrains entry that does not parse
//     ("unparsable dependency ..."), that names a package pattern rather
//     than a package, or that uses a conditional dependency ("foo; if
//     __win", "foo[when=...]") or extras ("foo[extras=[bar]]"), which are
//     out of scope for now;
//   - a record whose version does not parse, or whose name is not the
//     package's (a source answering "foo" with "Foo");
//   - every record of a package that has a Request.Locked record other
//     than that record ("bors=1.0=bla_1 is locked").
//
// A build listed as both .conda and .tar.bz2 by one channel subdir is kept
// once (the .conda unless only the .tar.bz2 is installable); a file listed
// twice is an error wrapping ErrDuplicateRecord.
//
// # Candidate order
//
// The solver tries the candidates of a package in this order (rattler's
// conda_sorting.rs, default "highest" strategy):
//
//  1. records without track_features first;
//  2. higher versions first;
//  3. higher build numbers first;
//  4. among candidates equal so far, the one whose dependencies (on the
//     packages every one of them depends on, in name order) can reach the
//     highest versions; a candidate's score for a package is the lowest of
//     the highest versions its requirements on that package match;
//  5. newer timestamps first.
//
// Before step 4, the candidates of a run whose dependencies are unknown
// (records excluded for an unparsable dependency and the like) are moved to
// the end of the run by swapping them with the last candidates, as rattler
// does; this can reorder the swapped candidates among themselves. Apart
// from that, remaining ties keep a canonical order (by subdir, file name,
// name, channel, URL and then every other field), so the answer does not
// depend on the order in which the source returns records.
//
// A Request.Favored record moves to the front of its package's candidates,
// so the solver tries it first whenever it decides that package. It is a
// preference, not a guarantee: a decision made earlier for another package
// can rule it out although another solution would keep it (with foo 1
// favored, deciding bar first picks bar 2, which needs foo >=2, even if
// bar 1 with foo 1 is also a solution).
//
// # Explanations
//
// An unsolvable request returns an *Unsolvable whose Explanation is cdcl's
// tree (see package cdcl), with runs of sibling candidates of one package
// that "conflict with the versions reported above" folded into one line.
// For example (shortened):
//
//	The following packages are incompatible
//	├─ numpy 1.21.* cannot be installed because there are no viable options:
//	│  └─ numpy 1.21.0 | 1.21.1 | ... | 1.21.6 would require
//	│     └─ python >=3.9,<3.10.0a0, which cannot be installed because there are no viable options:
//	│        └─ python 3.9.0 | 3.9.1 | ... | 3.9.23, which conflicts with the versions reported above.
//	└─ python 3.12.* cannot be installed because there are no viable options:
//	   └─ python 3.12.0 | 3.12.1 | ... | 3.12.13, which conflicts with the versions reported above.
//
// A spec that names a channel ("b::foo") the source did not use for the
// package, because strict channel priority answers foo from another
// channel, is explained as "foo from <channel b> is excluded because the
// channels in use offer foo only from <channel a> (strict channel
// priority)". That message depends on the order of loading: it appears
// only if the channel-qualified spec was known when foo's candidates were
// loaded. Root specs always are; a dependency's spec is known only if the
// record that carries it was loaded before foo. Otherwise the explanation
// says "No candidates were found for foo * [channel=b]".
//
// Version sets are shown as rattler shows them, with the version in
// conda-build form ("3.12.*") and any other constrained field in brackets
// ("foo >=2 [channel=conda-forge]").
//
// Names, versions, build strings, dependency strings, channels and URLs
// come from repodata or from the caller and are hostile input: wherever
// explanations and error messages show them, characters that could
// change how a terminal displays the text (controls, so also newlines that
// would forge lines of the tree, bidi and other format characters, line
// separators, invalid UTF-8, and the backslash) are written as Go escapes
// such as \x1b.
//
// # Determinism and concurrency
//
// The same request against the same records always gives the same solution
// and the same explanation. Solve keeps no state between calls: it is safe
// for concurrent use when the source is. Validate and RequestedPythonMinor
// are pure functions. Solve validates every solution it returns with
// Validate and fails closed (ErrInvalidSolution) if one ever fails.
//
// Solving is NP-complete: conda's real channels solve in milliseconds, but
// a crafted channel (a pigeonhole problem of a few hundred records) can keep
// the solver busy for minutes or longer. Solve honours ctx; callers should
// pass a context with a deadline.
//
// # Differences from rattler
//
//   - Only the default strategy (highest versions) and strict channel
//     priority exist; there is no flexible or disabled priority, no
//     exclude-newer or minimum age, no dependency overrides, no
//     caller-supplied exclusions, no variant flags.
//   - Conditional dependencies and extras are not supported: records using
//     them are excluded with a reason.
//   - Candidates are loaded lazily, one package name at a time, from a
//     repodata.Source; rattler receives every record up front.
//   - A channel-qualified spec restricts the records it matches, in a root
//     spec or a dependency alike; rattler excludes other channels' records
//     of a root spec's package. With strict priority applied by the source,
//     a spec naming a channel that does not answer for the package is
//     unsolvable, as in conda, and the explanation says which channels the
//     package comes from (for specs known when the package is loaded);
//     rattler lets a named channel bypass strict priority.
//   - A locked package's other records are excluded (with the lock as the
//     reason) besides being locked out through cdcl, so explanations blame
//     the lock where rattler may blame another rule.
//   - Records without a sha256 are excluded.
//   - Exact ties between candidates are broken by a canonical record order
//     instead of the order the records were given in.
package solve
