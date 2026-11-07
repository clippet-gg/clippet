package worker

import (
	"clippet/internal/db"
	"clippet/internal/transcode"
)

// The LIGHT tag, worker half (DESIGN.md §20 erratum 214).
//
// Every producer of the card's copy — the transcode run, the two-output pass, a
// standalone `light` job, a replacement — burns the same line into it, because
// the tag is a property of the TIER and not of the job that happened to make it.
// What each of them has to supply is the pair of qualities the line states: the
// copy's own, and the best one the link leads to.

// clipQualityShort is the quality of the file `/video.mp4` serves for this clip
// — the clip's own five measurements, which is what a reader who follows the
// link actually gets. It is "" for a row that has none, and a template asking
// for a quality nobody knows draws no tag at all (transcode.ExpandLightTag).
func clipQualityShort(c db.Clip) string {
	if c.Width == nil || c.Height == nil {
		return ""
	}
	var fps transcode.Rational
	if c.FPSNum != nil && c.FPSDen != nil {
		fps = transcode.Rational{Num: *c.FPSNum, Den: *c.FPSDen}
	}
	return transcode.QualityShort(*c.Width, *c.Height, fps)
}
