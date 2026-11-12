//go:build race

package scale

// The race detector multiplies CPU time several-fold, so time budgets are
// widened by this factor when it is on. Memory budgets are widened too: the
// detector's shadow memory is charged to the Go heap figures this suite reads.
const raceFactor = 12
