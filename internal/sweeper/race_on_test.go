//go:build race

package sweeper

// raceDetector is true in a `go test -race` build. It exists because the
// wall-clock deadlines in this package's timing tests are sized for an idle
// box, and the race detector multiplies every instrumented operation on a box
// that is already carrying two lane builders and a third container's `go test`
// (a harness fix after Gate 11). Nothing the product does reads it.
const raceDetector = true
