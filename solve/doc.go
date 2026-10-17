// Package solve resolves conda match specs to an exact set of package records,
// using package cdcl with conda's rules (candidate order, constrains, virtual
// packages, strict channel priority) and explains unsolvable requests.
package solve
