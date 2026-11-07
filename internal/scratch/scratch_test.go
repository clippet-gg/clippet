package scratch

import (
	"path/filepath"
	"testing"

	"clippet/internal/ids"
)

const testID = "aZ3kQ9xB2mLp"

// TestDirLayout: ClipDir and UploadTmpDir are single joins under the root
// (§8.3, §20.14): {root}/{id} and {root}/.upload-{id}, nothing nested.
func TestDirLayout(t *testing.T) {
	root := filepath.Join("var", "clippet", "scratch")
	if got, want := ClipDir(root, testID), filepath.Join(root, testID); got != want {
		t.Fatalf("ClipDir = %q, want %q", got, want)
	}
	if got, want := UploadTmpDir(root, testID), filepath.Join(root, ".upload-"+testID); got != want {
		t.Fatalf("UploadTmpDir = %q, want %q", got, want)
	}
	for name, dir := range map[string]string{"ClipDir": ClipDir(root, testID), "UploadTmpDir": UploadTmpDir(root, testID)} {
		if filepath.Dir(dir) != root {
			t.Fatalf("%s parent = %q, want the root %q", name, filepath.Dir(dir), root)
		}
	}
	// The two never coincide; the spool's basename is what the janitors see.
	if ClipDir(root, testID) == UploadTmpDir(root, testID) {
		t.Fatal("ClipDir and UploadTmpDir coincide")
	}
	if base := filepath.Base(UploadTmpDir(root, testID)); !IsUploadTmp(base) {
		t.Fatalf("IsUploadTmp(%q) = false for a spool dir", base)
	}
	if base := filepath.Base(ClipDir(root, testID)); IsUploadTmp(base) {
		t.Fatalf("IsUploadTmp(%q) = true for a clip dir", base)
	}
}

// TestIsUploadTmp: the prefix alone is not a spool (nothing follows it), a
// prefixed name is, and plain names – a valid clip id included – are not.
func TestIsUploadTmp(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{".upload-", false},
		{".upload-abc", true},
		{".upload-" + testID, true},
		{"abc", false},
		{testID, false},
		{"", false},
		{"upload-abc", false},   // no leading dot
		{".Upload-abc", false},  // case matters
		{".uploadabc", false},   // no dash
		{"x.upload-abc", false}, // prefix, not substring
	}
	for _, tc := range tests {
		if got := IsUploadTmp(tc.name); got != tc.want {
			t.Errorf("IsUploadTmp(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestUploadTmpNameIsNeverAnID: the janitors' "dir name is a valid id" gate
// (§12) must never match a spool, whatever id it carries, while a clip dir's
// basename always passes it. BOTH grammars are drawn (§20 erratum 249): the
// gate widened when memorable ids arrived, and the leading dot is what still
// keeps a spool outside it.
func TestUploadTmpNameIsNeverAnID(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 200; i++ {
		id := ids.New()
		switch i % 3 {
		case 1:
			id = ids.NewMemorable(ids.MemorableMinWords)
		case 2:
			id = ids.NewMemorable(ids.MemorableMaxWords)
		}
		spool := filepath.Base(UploadTmpDir(root, id))
		if ids.Valid(spool) {
			t.Fatalf("ids.Valid(%q) = true for a spool dir", spool)
		}
		if !IsUploadTmp(spool) {
			t.Fatalf("IsUploadTmp(%q) = false", spool)
		}
		if clip := filepath.Base(ClipDir(root, id)); !ids.Valid(clip) || IsUploadTmp(clip) {
			t.Fatalf("clip dir basename %q: Valid=%v IsUploadTmp=%v", clip, ids.Valid(clip), IsUploadTmp(clip))
		}
	}
}

// TestReplaceTmpDir: a replacement's spool is a `.upload-` name — so the
// janitor's row-less rule covers it with no new rule — carries the attempt's
// r8 so two replacements of one clip cannot collide, and is never the clip's
// own directory, which belongs to whatever run is registered for it
// (§20 erratum 136).
func TestReplaceTmpDir(t *testing.T) {
	root := filepath.Join("var", "clippet", "scratch")
	dir := ReplaceTmpDir(root, testID, "r8r8r8r8")
	if want := filepath.Join(root, ".upload-"+testID+"-r8r8r8r8"); dir != want {
		t.Fatalf("ReplaceTmpDir = %q, want %q", dir, want)
	}
	if filepath.Dir(dir) != root {
		t.Fatalf("ReplaceTmpDir parent = %q, want the root %q", filepath.Dir(dir), root)
	}
	if dir == ClipDir(root, testID) || dir == UploadTmpDir(root, testID) {
		t.Fatal("a replacement spool must differ from the clip dir and from a first upload's spool")
	}
	if a, b := ReplaceTmpName(testID, "aaaaaaaa"), ReplaceTmpName(testID, "bbbbbbbb"); a == b {
		t.Fatal("two attempts of one clip share a spool name")
	}
	if !IsUploadTmp(filepath.Base(dir)) {
		t.Fatalf("IsUploadTmp(%q) = false: the janitor would treat it as a clip dir", filepath.Base(dir))
	}
}

// TestValidTmpName: the job row carries the spool NAME, and a row is not a
// place to take a filesystem path on trust.
func TestValidTmpName(t *testing.T) {
	good := []string{ReplaceTmpName(testID, "r8r8r8r8"), ".upload-" + testID, ".upload-x"}
	for _, n := range good {
		if !ValidTmpName(n) {
			t.Errorf("ValidTmpName(%q) = false", n)
		}
	}
	bad := []string{"", ".upload-", testID, "upload-x", "../etc", ".upload-../../etc",
		".upload-a/b", `.upload-a\b`, ".upload-..", "/etc/passwd"}
	for _, n := range bad {
		if ValidTmpName(n) {
			t.Errorf("ValidTmpName(%q) = true", n)
		}
	}
}
