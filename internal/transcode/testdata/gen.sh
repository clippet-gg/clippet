#!/usr/bin/env sh
# gen.sh — regenerate the ffprobe JSON fixtures in this directory with the
# container's ffmpeg/ffprobe 7.1.1 (DESIGN.md §16.2 "transcode" row).
#
# Run from the repo root, INSIDE the test image so the binaries match CI:
#   MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd -W)":/src -w /src \
#     clippet-gotest:ffmpeg sh internal/transcode/testdata/gen.sh
#
# Every fixture is the verbatim output of
#   ffprobe -v error -print_format json -show_format -show_streams FILE
# on a clip generated here with lavfi, with two documented exceptions
# (ffmpeg's own muxers cannot produce these layouts from a lavfi clip, but
# real-world files do, and the parser rules of §10.3 exist for them):
#   - rotated90-sidedata.json: real output of a -display_rotation 90 remux
#     with a "Mastering display metadata" entry inserted at side_data_list
#     index 0 (phone HDR captures put HDR entries ahead of the matrix);
#     the insertion is done below with awk, so it IS regenerated.
#   - attached-pic-first.json: derived once from attach.mkv's real output
#     (mkv -attach cover.jpg) by moving the mjpeg attached_pic stream to
#     index 0 and renumbering (mov appends covr streams last, matroska reads
#     attachments after tracks). Not regenerated here.
set -eu
cd "$(dirname "$0")"
T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT
FF="ffmpeg -hide_banner -loglevel error -y"
V="-c:v libx264 -preset ultrafast -pix_fmt yuv420p"
A="-c:a aac -b:a 96k"
D=2

probe() { ffprobe -v error -print_format json -show_format -show_streams "$1" > "$2"; echo "  + $2"; }

# probe_hdr_first inserts an HDR side-data entry at index 0 of every
# side_data_list, ahead of whatever the file really carries.
probe_hdr_first() {
  ffprobe -v error -print_format json -show_format -show_streams "$1" | awk '
    { print }
    /"side_data_list": \[/ {
      print "                {"
      print "                    \"side_data_type\": \"Mastering display metadata\","
      print "                    \"red_x\": \"34000/50000\","
      print "                    \"red_y\": \"16000/50000\","
      print "                    \"green_x\": \"13250/50000\","
      print "                    \"green_y\": \"34500/50000\","
      print "                    \"blue_x\": \"7500/50000\","
      print "                    \"blue_y\": \"3000/50000\","
      print "                    \"white_point_x\": \"15635/50000\","
      print "                    \"white_point_y\": \"16450/50000\","
      print "                    \"min_luminance\": \"1/10000\","
      print "                    \"max_luminance\": \"10000000/10000\""
      print "                },"
    }' > "$2"
  echo "  + $2 (HDR entry inserted at side_data_list[0])"
}

# baseline 720p30 with audio
$FF -f lavfi -i "testsrc2=duration=$D:size=1280x720:rate=30" -f lavfi -i "sine=frequency=440:duration=$D" $V $A -shortest "$T/std.mp4"
probe "$T/std.mp4" std-720p30.json

# rotation at side-data index 1 behind an HDR entry — the "scan ALL
# entries" trap of §10.3.
$FF -display_rotation 90 -i "$T/std.mp4" -c copy "$T/rot90.mp4"
probe_hdr_first "$T/rot90.mp4" rotated90-sidedata.json

# negative rotation (-90 → 270, still a swap)
$FF -display_rotation -90 -i "$T/std.mp4" -c copy "$T/rotneg.mp4"
probe "$T/rotneg.mp4" rotated-neg90.json

# legacy tags.rotate (matroska keeps the tag as ROTATE, no side data)
$FF -i "$T/std.mp4" -c copy -metadata:s:v:0 rotate=90 "$T/tagrot.mkv"
probe "$T/tagrot.mkv" tags-rotate.json

# anamorphic SAR 16:11 (coded 720x576 → display 1047x576)
$FF -f lavfi -i "testsrc2=duration=$D:size=720x576:rate=25" -f lavfi -i "sine=frequency=300:duration=$D" -vf setsar=16/11 $V $A -shortest "$T/sar.mp4"
probe "$T/sar.mp4" anamorphic-sar.json

# fractional 30000/1001
$FF -f lavfi -i "testsrc2=duration=$D:size=1280x720:rate=30000/1001" -f lavfi -i "sine=frequency=400:duration=$D" $V $A -shortest "$T/frac.mp4"
probe "$T/frac.mp4" fractional-2997.json

# no audio
$FF -f lavfi -i "testsrc2=duration=$D:size=1280x720:rate=30" $V -an "$T/noaudio.mp4"
probe "$T/noaudio.mp4" noaudio.json

# dual mono audio tracks (game + mic)
$FF -f lavfi -i "testsrc2=duration=$D:size=1280x720:rate=30" -f lavfi -i "sine=frequency=440:duration=$D" -f lavfi -i "sine=frequency=880:duration=$D" \
  -map 0:v -map 1:a -map 2:a $V $A -shortest -metadata:s:a:0 title=game -metadata:s:a:1 title=mic "$T/dual.mp4"
probe "$T/dual.mp4" dualaudio.json

# missing duration: matroska written to a PIPE, so the muxer cannot seek back
# to fill the Duration element and neither the format nor a stream declares one
$FF -i "$T/std.mp4" -c copy -f matroska - > "$T/nodur.mkv"
probe "$T/nodur.mkv" no-duration.json

# a raw H.264 elementary stream: not a container, refused by the §8.3
# container allowlist (nothing produces one as a clip)
$FF -i "$T/std.mp4" -an -c:v copy -bsf:v h264_mp4toannexb -f h264 "$T/raw.h264"
probe "$T/raw.h264" elementary-h264.json

# the list formats the container allowlist exists for: each is a text file
# naming further resources, which ffmpeg would go and read (§20 erratum 91).
# HLS — a playlist beside its segment
$FF -i "$T/std.mp4" -c copy -f mpegts "$T/seg0.ts"
printf '#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:3\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:2.000000,\nseg0.ts\n#EXT-X-ENDLIST\n' > "$T/list.m3u8"
(cd "$T" && ffprobe -v error -print_format json -show_format -show_streams list.m3u8) > hls-playlist.json
echo "  + hls-playlist.json"
# DASH — the same idea in XML (segmented, so ffprobe can open the first one)
mkdir -p "$T/dash"
$FF -i "$T/std.mp4" -c copy -f dash -seg_duration 1 "$T/dash/manifest.mpd"
(cd "$T/dash" && ffprobe -v error -print_format json -show_format -show_streams manifest.mpd) > dash-manifest.json
echo "  + dash-manifest.json"
# concat — an ffconcat script naming local files
printf 'ffconcat version 1.0\nfile std.mp4\n' > "$T/list.ffconcat"
(cd "$T" && ffprobe -v error -print_format json -show_format -show_streams list.ffconcat) > concat-list.json
echo "  + concat-list.json"

# cover art only (mp3 + attached_pic): no video, and mp3 is not a container the
# §8.3 allowlist accepts either — it is refused before the stream scan
$FF -f lavfi -i "color=c=red:size=160x90:duration=0.1" -frames:v 1 -update 1 "$T/cover.jpg"
$FF -i "$T/cover.jpg" -f lavfi -i "sine=frequency=440:duration=1" -map 0:v -map 1:a -c:v copy -c:a libmp3lame -disposition:v:0 attached_pic -id3v2_version 3 "$T/cover.mp3"
probe "$T/cover.mp3" cover-only.json

# a still picture (png_pipe demuxer): a "video" stream that is not a video
$FF -f lavfi -i "color=c=red:size=160x90:duration=0.1" -frames:v 1 -update 1 "$T/still.png"
probe "$T/still.png" still-image.json

# an encoded OUTPUT as the pipeline produces it (ParseOutput fixture)
$FF -i "$T/std.mp4" -filter_complex "[0:0]scale=1280:720:flags=lanczos,setsar=1,format=yuv420p[v];[0:1]aresample=48000[a]" -map "[v]" -map "[a]" \
  -r 30/1 -fps_mode cfr $V -g 60 -keyint_min 30 -c:a aac -b:a 128k -ar 48000 -ac 2 -movflags +faststart -f mp4 "$T/out.mp4"
probe "$T/out.mp4" output-720p30.json
echo "done"
