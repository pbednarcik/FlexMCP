//go:build race

package server

// raceEnabled reports a build with the race detector, whose instrumentation
// allocates, so allocation counts are not checked under it.
const raceEnabled = true
