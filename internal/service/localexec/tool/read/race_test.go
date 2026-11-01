//go:build race

package read

// raceEnabled skips the memory tests, which the race detector slows down
// tenfold and whose measurements it changes.
const raceEnabled = true
