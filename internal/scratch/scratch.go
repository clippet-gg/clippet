// Package scratch names the scratch-directory layout shared by the upload
// handler (spool), the worker (transcode inputs/outputs) and the janitors
// (boot + every 10th sweeper tick).
//
// The rule (§8.3, §12, §20): a clip's scratch dir is $SCRATCH_DIR/{id}. An
// upload whose clip row does not exist yet spools under
// $SCRATCH_DIR/.upload-{id} — a name ids.Valid rejects — and is renamed to
// $SCRATCH_DIR/{id} only after CreateClipWithJob commits, so the janitors'
// "row missing → remove" rule can never destroy the spool of an in-flight
// upload, however long it takes.
package scratch

import (
	"path/filepath"
	"strings"
)

// uploadTmpPrefix marks a spool dir whose clip row does not exist yet.
// The leading dot keeps it outside ids.Valid.
const uploadTmpPrefix = ".upload-"

// ClipDir is the clip's scratch directory: $SCRATCH_DIR/{id}.
func ClipDir(root, id string) string { return filepath.Join(root, id) }

// UploadTmpDir is the row-less spool directory: $SCRATCH_DIR/.upload-{id}.
func UploadTmpDir(root, id string) string { return filepath.Join(root, uploadTmpPrefix+id) }

// IsUploadTmp reports whether a directory name is a row-less upload spool.
func IsUploadTmp(name string) bool {
	return len(name) > len(uploadTmpPrefix) && name[:len(uploadTmpPrefix)] == uploadTmpPrefix
}

// ReplaceTmpName is the spool directory NAME of one replacement upload
// (§20 erratum 136): `.upload-{id}-{r8}`, which `IsUploadTmp` already
// recognises, so the janitor's 24 h stale rule covers it with no new rule.
//
// A replacement deliberately does NOT use $SCRATCH_DIR/{id}. That directory
// belongs to whatever run is registered for the clip, and a trim of the same
// clip keeps the CURRENT original in it under the same `source.{ext}` name:
// spooling a different file there would overwrite a running trim's input.
// The attempt's own r8 also keeps two replacements of one clip apart, which
// matters because the at-most-one guard is the database's and is only reached
// once the body is already spooled.
func ReplaceTmpName(id, r8 string) string { return uploadTmpPrefix + id + "-" + r8 }

// ReplaceTmpDir is that directory under root.
func ReplaceTmpDir(root, id, r8 string) string { return filepath.Join(root, ReplaceTmpName(id, r8)) }

// ValidTmpName reports whether name is a spool directory name this package
// minted: a single path segment under the root, never a traversal. The
// replacement job carries its spool NAME in the job row, and a row is not a
// place to take a filesystem path on trust.
func ValidTmpName(name string) bool {
	if !IsUploadTmp(name) || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return false
	}
	return name == filepath.Base(name)
}
