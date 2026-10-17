# Differential corpus against resolvo

`expected.txt` is what resolvo v0.12.2 answers for the 3000 BundleBox
problems that `randomProblem` in `oracle_test.go` generates from the seeds
0..2999 (the first 2500 small, the rest larger and mostly satisfiable).
For each problem it records the solution in decision order, or the conflict
explanation and its Graphviz rendering, plus the clause and
deferred-requirement counts. An entry `PANIC` marks a problem on which
resolvo itself panics.

`expected_activity.txt` is the same for the 500 problems of
`activityProblem`, solved with non-standard activity parameters (which turn
off the decide queue's activity shortcuts).

`TestOracleCorpus` and `TestOracleActivityCorpus` regenerate the problems,
solve them with cdcl and require byte-identical answers, except for the
problems listed in `deviations.txt` and `deviations_activity.txt`: these
hold cdcl's own answer in the same format, and cdcl must reproduce it
exactly. `deviations_classes.txt` gives the reason for each deviation, and
`TestOracleDeviationClasses` checks that every deviation is classified with
one of the classes below.

## Deliberate deviations

cdcl differs from resolvo in three ways that change answers:

- **Exact conditions.** resolvo encodes a conditional requirement
  `R if C` as `R ∨ (some installed version does not match C)`, using the
  at-most-one helper variables of C's package. That clause is weaker than
  `C ⇒ R` when C's package allows several versions (the condition is then
  never enforced), and stronger than it when no version of C's package is
  installed (it forces some non-matching version or R). cdcl introduces a
  variable `H` per condition version set with `(¬m ∨ H)` for every matching
  candidate `m` and `(¬H ∨ m1 ∨ … ∨ mk)`, so `H` is exactly "some matching
  candidate is installed", and encodes `¬H ∨ R` (one clause per disjunct
  of a compound condition). See `conditions.go`.
- **Stranded implications.** cdcl re-derives implications that the encoder
  made outside propagation and that a backjump left unassigned, just before
  accepting a solution (resolvo does this only for shared-requires gates).
  On this corpus no deviation is due to the repair alone: without it cdcl
  would answer problem 2676 differently (same solution, other counts) but
  still not like resolvo, and every other answer would be unchanged.
  Without it `TestRandomProblemSolutionsAreValid`, `TestPropertyBruteForce`
  and the `FuzzSolveRandom` seeds find invalid solutions.
- **Bounded explanations.** resolvo expands a candidate again each time the
  explanation tree reaches it, which is exponential in the depth of the
  conflict graph and never terminates on a cyclic one. cdcl explains a
  candidate that would require or constrain something once and prints
  later occurrences as `<candidate>, as reported above.` (and caps the
  text). Where a candidate repeats, the explanation is therefore shorter;
  everything else, including the Graphviz rendering, is unchanged.

The answers differ on 449 corpus and 99 activity problems; each was
classified by checking both answers against the problem under strict
semantics (every installed solvable's requirements, constraints and
conditions hold; at most one version per package unless it allows several;
a condition holds when an installed version matches it) with an
independent validity checker, and by deciding every unsat answer with an
independent SAT check. Condition-free problems are all identical.

| class                                      | corpus | activity |
|--------------------------------------------|-------:|---------:|
| identical (not listed)                     |   2551 |      401 |
| same solution, order or counts differ      |    297 |       66 |
| both unsat, explanation or counts differ   |    113 |       21 |
| explanation shows repeated candidates once |     20 |        5 |
| both valid, different solutions            |      4 |        3 |
| resolvo invalid: cond-multi                |      9 |        3 |
| resolvo invalid: root-cond-multi           |      3 |        1 |
| resolvo panics                             |      3 |        0 |

- **Same solution, order or counts differ; both unsat, explanation or
  counts differ.** The extra condition variables and clauses change the
  clause counts and the order of propagation and learning; the installed
  set, or unsatisfiability, is the same. cdcl's unsat answers on these
  problems are all confirmed unsat by the SAT check.
- **Explanation shows repeated candidates once.** Both are unsat with the
  same conflict graph and counts; cdcl's explanation is resolvo's with
  every repeated expansion of a candidate replaced by the back-reference
  line (checked mechanically: applying that rule to resolvo's text gives
  cdcl's). Eleven corpus and two activity problems of the other classes
  also contain back-references.
- **Both valid, different solutions** (corpus 1751, 2669, 2827, 2966;
  activity 71, 146, 251). Both answers satisfy the problem. In corpus 2827
  and activity 71 and 251 a fired condition names a package that allows
  several versions, which resolvo does not enforce, so the clause
  databases differ; in the other four the search diverges after the
  first difference in propagation order. Neither solver minimises: in
  corpus 2669 resolvo also installs `p20=10`, which no requirement needs,
  and in corpus 2827 and activity 71 cdcl installs one package more than
  needed.
- **resolvo invalid: cond-multi, root-cond-multi.** resolvo's solution
  violates a conditional requirement (of an installed solvable, or of the
  root) whose condition holds through a package that allows several
  versions. For example, in corpus 1215 `p5` allows several versions,
  `p2=4` requires `p0 6..11 if p5`, and resolvo installs `p2=4` and a `p5`
  but no `p0 6..11`. cdcl's answers are valid.
- **resolvo panics** (corpus 191, 285, 1980). resolvo's conflict graph
  contains a node that is not reachable from the root, which trips an
  `assert!` in `src/conflict.rs`. cdcl's unsat answers are confirmed by the
  SAT check; its explanation shows the reachable part of the graph.

No problem is a false unsat or an invalid solution of cdcl's.

## Regenerating

1. Write the problems:

       GOTOOLCHAIN=local go test ./cdcl -run TestOracleCorpus \
           -args -write-oracle-problems=/path/to/problems.json

2. In a scratch copy of `$GOOMBA_REFS/resolvo` (drop the `cpp` and `tools`
   workspace members), copy `oracle.rs` to `tests/solver/oracle.rs`, add
   `mod oracle;` to `tests/solver/main.rs` and run the release build (debug
   assertions abort on a few problems that resolvo answers in release):

       ORACLE_IN=/path/to/problems.json ORACLE_OUT=/path/to/expected.txt \
           cargo test --release --test solver oracle

3. Copy the output over `expected.txt`.

4. Write cdcl's answers in the same format:

       GOTOOLCHAIN=local go test ./cdcl -run TestOracleRun \
           -args -oracle-in=/path/to/problems.json -oracle-out=/path/to/cdcl.txt

   Every section of `cdcl.txt` that differs from `expected.txt` goes into
   `deviations.txt`, after classifying it as above; any other class is a
   bug.

Likewise for the activity corpus with `-write-activity-problems`,
`expected_activity.txt` and `deviations_activity.txt`. `TestOracleRun`
solves activity problems with their activity parameters, since those are
part of the problem file.
