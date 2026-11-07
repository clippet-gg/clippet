package main

import "testing"

// TestSameVolumePath pins the whole of the Windows same-device decision that a
// Linux container can reach (§20 erratum 308). The two GetVolumePathName calls
// cannot run here; what they ANSWER is two strings, and this is what
// sameDevice does with them.
//
// The case that matters is the third block: a volume mounted into a folder.
// `C:\clippet\data` and `C:\clippet\scratch` are the DATA_DIR/SCRATCH_DIR
// split an operator makes on purpose, and if the comparison were over drive
// letters they would read as one device — `admin doctor` would then print
// "SCRATCH_MIN_FREE_BYTES guards the database too" on the one deployment where
// it does not.
func TestSameVolumePath(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b string
		want bool
	}{
		{"the same root", `C:\`, `C:\`, true},
		{"two drives", `C:\`, `D:\`, false},
		{"case is not a volume", `C:\`, `c:\`, true},
		{"a trailing separator is not a volume", `C:\data\`, `C:\data`, true},
		{"a root with no separator", `C:`, `C:\`, true},
		{"forward slashes are the same path", `C:/data`, `C:\data`, true},

		// A volume mounted into a folder: the same drive letter, two volumes.
		{"a mounted folder is not its host drive", `C:\clippet\data\`, `C:\`, false},
		{"two mounted folders", `C:\clippet\data\`, `C:\clippet\scratch\`, false},
		{"the same mounted folder, spelled twice", `C:\Clippet\Data\`, `c:\clippet\data`, true},
		// A prefix is not a volume: `C:\data` must not match `C:\database`.
		{"one name is a prefix of the other", `C:\data`, `C:\database`, false},

		// UNC shares are volume path names too, and two shares on one server
		// are two volumes.
		{"the same share", `\\nas\clips\`, `\\nas\clips`, true},
		{"two shares on one server", `\\nas\clips\`, `\\nas\backup\`, false},

		// "No answer" is never a match, including against itself.
		{"both empty", "", "", false},
		{"one empty", `C:\`, "", false},
		{"separators only", `\`, `\\`, false},
	} {
		if got := sameVolumePath(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: sameVolumePath(%q, %q) = %v, want %v", tc.name, tc.a, tc.b, got, tc.want)
		}
		// It is a question about two volumes, not about the order they were
		// asked in.
		if got := sameVolumePath(tc.b, tc.a); got != tc.want {
			t.Errorf("%s reversed: sameVolumePath(%q, %q) = %v, want %v", tc.name, tc.b, tc.a, got, tc.want)
		}
	}
}
