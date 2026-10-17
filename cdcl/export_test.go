package cdcl

// EnableDebugChecks turns on the solver's expensive invariant checks (the
// equivalent of resolvo's debug assertions) for tests.
func EnableDebugChecks(s *Solver) { s.debugChecks = true }
