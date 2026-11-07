-- 0021_clip_kind.sql — a clip can be sound (DESIGN.md §20 erratum 328).
--
-- `kind` is 'video' or 'audio', and every row that exists is a video: the
-- default IS the backfill. It is decided once, by the ingest probe (a file with
-- no non-cover video stream and one audio stream in an allowlisted container),
-- and nothing ever changes it — a replacement keeps the kind it replaces.
--
-- The four facts after it describe an audio clip's ONE audio stream as the
-- strip stored it, and they are what its labels say: the codec ffprobe names
-- (`flac`, `pcm_s24le`, `mp3`, `opus`), the sample rate in Hz, the bit depth (a
-- lossless or PCM codec's; NULL for a lossy one, which has none) and the channel
-- count. They are NULL on every video row: a video's audio is not described,
-- because nothing labels it.
--
-- An audio clip leaves `width`, `height`, `fps_num` and `fps_den` NULL, which
-- those columns have always allowed: they mean "the shape of the file
-- /video.mp4 serves", and an audio clip has no picture of its own (its card copy
-- carries its dimensions in `social_width`/`social_height`, as every card copy
-- does). No index: every reader of these columns already holds the row.
ALTER TABLE clips ADD COLUMN kind TEXT NOT NULL DEFAULT 'video' CHECK (kind IN ('video', 'audio'));
ALTER TABLE clips ADD COLUMN audio_codec TEXT;
ALTER TABLE clips ADD COLUMN sample_rate INTEGER;
ALTER TABLE clips ADD COLUMN bit_depth INTEGER;
ALTER TABLE clips ADD COLUMN channels INTEGER;
