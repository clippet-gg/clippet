//go:build race

package main

// raceDetector is true in a `go test -race` build. TestBackupCancelInterruptsVacuum
// builds a ~200 MiB database, which the race detector turns from seven seconds
// into minutes; the ffmpeg-tagged pass runs it without the detector. Nothing the
// product does reads it.
const raceDetector = true
