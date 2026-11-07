//go:build !race

package main

// raceDetector is false in an ordinary build. See race_on_test.go.
const raceDetector = false
