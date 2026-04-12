// Package youtube wraps the yt-dlp CLI to enumerate a channel's past
// livestreams and fetch their auto-generated captions.
package youtube

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const DefaultStreamsURL = "https://www.youtube.com/@EnterpriseVibeCode/streams"

// Stream is one past livestream candidate.
type Stream struct {
	Process       string  // "yes" by default
	UploadDate    string  // YYYY-MM-DD
	DurationHuman string
	DurationSec   float64
	Title         string
	VideoID       string
	URL           string
}

// ListPastStreams enumerates past (completed) livestreams from the channel
// URL. Filters out currently-live and any entries missing `was_live`. Slow:
// requires per-video metadata, ~3s per video.
func ListPastStreams(ctx context.Context, channelStreamsURL string) ([]Stream, error) {
	cmd := exec.CommandContext(ctx, "yt-dlp",
		"--skip-download",
		"--dump-json",
		"--no-warnings",
		channelStreamsURL,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("yt-dlp list: %w (stderr: %s)", err, tail(stderr.String(), 500))
	}
	var out []Stream
	sc := bufio.NewScanner(&stdout)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var v struct {
			ID         string   `json:"id"`
			Title      string   `json:"title"`
			UploadDate string   `json:"upload_date"` // YYYYMMDD
			Duration   *float64 `json:"duration"`
			WasLive    bool     `json:"was_live"`
			IsLive     bool     `json:"is_live"`
			URL        string   `json:"webpage_url"`
		}
		if err := json.Unmarshal(line, &v); err != nil {
			// Skip malformed entries; yt-dlp sometimes prints progress to stdout.
			continue
		}
		if !v.WasLive || v.IsLive {
			continue
		}
		dur := 0.0
		if v.Duration != nil {
			dur = *v.Duration
		}
		out = append(out, Stream{
			Process:       "yes",
			UploadDate:    dateISO(v.UploadDate),
			DurationHuman: humanDuration(dur),
			DurationSec:   dur,
			Title:         v.Title,
			VideoID:       v.ID,
			URL:           v.URL,
		})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UploadDate < out[j].UploadDate })
	return out, nil
}

// FetchCaptions fetches auto-generated English captions (.en.vtt) for videoID
// into destDir. Returns the full path to the .vtt file, or "" with no error
// if no captions are available yet.
func FetchCaptions(ctx context.Context, videoID, destDir string) (string, error) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", err
	}
	outTmpl := filepath.Join(destDir, "%(id)s.%(ext)s")
	url := "https://www.youtube.com/watch?v=" + videoID
	cmd := exec.CommandContext(ctx, "yt-dlp",
		"--skip-download",
		"--write-auto-subs",
		"--sub-lang", "en",
		"--sub-format", "vtt",
		"--no-warnings",
		"-o", outTmpl,
		url,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("yt-dlp captions %s: %w (stderr: %s)", videoID, err, tail(stderr.String(), 500))
	}
	// Expected output path: <destDir>/<videoID>.en.vtt
	vttPath := filepath.Join(destDir, videoID+".en.vtt")
	if _, err := os.Stat(vttPath); err == nil {
		return vttPath, nil
	}
	// Captions not available for this video.
	return "", nil
}

// ---------------------------------------------------------------------------
// VTT → plain text
// ---------------------------------------------------------------------------

var (
	tagRE  = regexp.MustCompile(`<[^>]+>`)
	cueRE  = regexp.MustCompile(`^(\d\d:\d\d:\d\d)\.\d+ --> `)
	blankL = regexp.MustCompile(`^\s*$`)
)

// ParseVTT converts YouTube's rolling auto-caption VTT into plain text with
// periodic [HH:MM:SS] timestamps. De-duplicates YouTube's rolling repetition
// by only emitting new text per cue, and skips inline <timestamp><c>word</c>
// styling.
func ParseVTT(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(b), "\n")

	var out strings.Builder
	var emitted strings.Builder
	lastEmittedLine := ""
	inCue := false
	cueStart := ""
	timestampSinceHeader := "" // pending timestamp to prepend next emission
	lastTSBucket := ""

	flushTS := func() {
		if timestampSinceHeader != "" {
			out.WriteString("\n\n[")
			out.WriteString(timestampSinceHeader)
			out.WriteString("] ")
			timestampSinceHeader = ""
		}
	}

	for _, line := range lines {
		if strings.HasPrefix(line, "WEBVTT") ||
			strings.HasPrefix(line, "Kind:") ||
			strings.HasPrefix(line, "Language:") {
			continue
		}
		if m := cueRE.FindStringSubmatch(line); m != nil {
			cueStart = m[1]
			inCue = true
			// Bucket timestamps by 30 second boundary to avoid over-tagging.
			bucket := cueStart[:5] + ":" + bucketSeconds(cueStart[6:8])
			if bucket != lastTSBucket {
				timestampSinceHeader = cueStart
				lastTSBucket = bucket
			}
			continue
		}
		if !inCue {
			continue
		}
		if blankL.MatchString(line) {
			inCue = false
			continue
		}
		clean := strings.TrimSpace(tagRE.ReplaceAllString(line, ""))
		if clean == "" || clean == lastEmittedLine {
			continue
		}
		lastEmittedLine = clean
		flushTS()
		emitted.WriteString(clean)
		emitted.WriteString(" ")
		// Also write to final output buffer.
		if out.Len() == 0 {
			out.WriteString("[")
			out.WriteString(cueStart)
			out.WriteString("] ")
		}
		out.WriteString(clean)
		out.WriteString(" ")
	}
	return strings.TrimSpace(out.String()), nil
}

// WriteCSV writes the streams list for hand-editing.
func WriteCSV(path string, streams []Stream) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	if err := w.Write([]string{"process", "upload_date", "duration_human", "duration_sec", "title", "video_id", "url"}); err != nil {
		return err
	}
	for _, s := range streams {
		if err := w.Write([]string{
			s.Process, s.UploadDate, s.DurationHuman,
			strconv.FormatFloat(s.DurationSec, 'f', 2, 64),
			s.Title, s.VideoID, s.URL,
		}); err != nil {
			return err
		}
	}
	return nil
}

// ReadCSV returns rows marked process=="yes" with non-empty video_id.
func ReadCSV(path string) ([]Stream, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	rows, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	header := rows[0]
	idx := func(name string) int {
		for i, h := range header {
			if h == name {
				return i
			}
		}
		return -1
	}
	iProc, iDate, iDH, iDS, iTitle, iVID, iURL :=
		idx("process"), idx("upload_date"), idx("duration_human"), idx("duration_sec"),
		idx("title"), idx("video_id"), idx("url")
	var out []Stream
	for _, row := range rows[1:] {
		if len(row) < len(header) {
			continue
		}
		if row[iProc] != "yes" || row[iVID] == "" {
			continue
		}
		dur, _ := strconv.ParseFloat(row[iDS], 64)
		out = append(out, Stream{
			Process: row[iProc], UploadDate: row[iDate], DurationHuman: row[iDH],
			DurationSec: dur, Title: row[iTitle], VideoID: row[iVID], URL: row[iURL],
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func dateISO(ymd string) string {
	if len(ymd) != 8 {
		return ymd
	}
	t, err := time.Parse("20060102", ymd)
	if err != nil {
		return ymd
	}
	return t.Format("2006-01-02")
}

func humanDuration(secs float64) string {
	s := int(secs)
	return fmt.Sprintf("%d:%02d:%02d", s/3600, (s%3600)/60, s%60)
}

func bucketSeconds(ss string) string {
	v, err := strconv.Atoi(ss)
	if err != nil {
		return "00"
	}
	if v < 30 {
		return "00"
	}
	return "30"
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
