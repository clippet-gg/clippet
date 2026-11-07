package main

import "strings"

// The portable half of freebytes_windows.go's sameDevice (§20 erratum 308).
//
// It carries NO build constraint on purpose. The Windows halves of this
// program can only be compiled in this container, never run in it — there is
// no Windows kernel here — so the only part of the volume comparison that can
// be PROVEN by the suite is the part that is a decision about two strings.
// That part is the whole of the comparison: sameDevice's Windows-only work is
// two GetVolumePathName calls, and everything it then decides is here, where a
// table test reaches it on every platform.

// sameVolumePath reports whether two volume path names name the same volume.
//
// The inputs are two answers from ONE GetVolumePathName on one machine —
// `C:\`, or `C:\mounted\folder\` for a volume mounted into a directory — so
// they are already in the API's own spelling and this is a comparison rather
// than a path resolution. Two things still have to be normalized away:
//
//   - The TRAILING SEPARATOR. The API returns one for a root (`C:\`) and, on
//     a mounted folder, whether the path it was asked about carried one; two
//     spellings of one volume must not read as two volumes.
//   - CASE. Windows filenames are case-insensitive, so `C:\Data\` and
//     `c:\data\` are one directory and must compare equal. EqualFold is
//     Unicode simple folding rather than the filesystem's own uppercase table:
//     it agrees for every ASCII path and for the accented letters a Windows
//     path realistically holds, and where it could differ it errs towards
//     calling two spellings the SAME volume — which costs an advisory line its
//     precision and can never cost an operator a warning they needed, because
//     the warning fires on DIFFERENT devices.
//
// A forward slash is folded to a backslash for the same reason: Go code
// reaches this API with paths a person typed into `.env`, and Windows accepts
// either separator everywhere.
//
// An EMPTY name is never the same volume as anything, itself included. It can
// only mean "no answer", and two absent answers are not evidence of one
// volume; sameDevice never produces it (an API failure is an error there), and
// this keeps a future caller from reading a pair of blanks as a match.
func sameVolumePath(a, b string) bool {
	na, nb := normalizeVolumePath(a), normalizeVolumePath(b)
	if na == "" || nb == "" {
		return false
	}
	return strings.EqualFold(na, nb)
}

// normalizeVolumePath folds a volume path name to the form sameVolumePath
// compares: backslashes, and no trailing separator. `C:\` becomes `C:` and
// `C:\data\` becomes `C:\data`, which stay distinct — the trailing separator
// is all that is dropped, never a path segment.
func normalizeVolumePath(p string) string {
	p = strings.ReplaceAll(p, "/", `\`)
	return strings.TrimRight(p, `\`)
}
