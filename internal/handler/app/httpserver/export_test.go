package httpserver

// Test-only exports: the external test package (httpserver_test) asserts the
// exact §5 degraded/unavailable vocabulary without widening the production
// API. The notes that moved into services (promotion, the cold-probe key, the
// PATCH op vocabulary) are asserted through those packages' own exports now.
const (
	DegradedSearch = degradedSearch
	DegradedGraph  = degradedGraph
	DegradedCold   = degradedCold
)
