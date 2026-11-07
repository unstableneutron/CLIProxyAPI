//go:build race

package executor

// raceDetectorEnabled lets wall-clock performance assertions skip under -race,
// whose instrumentation slows execution by an order of magnitude.
const raceDetectorEnabled = true
