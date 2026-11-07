//go:build !race

package sweeper

// raceDetector is false in an ordinary build. See race_on_test.go.
const raceDetector = false
