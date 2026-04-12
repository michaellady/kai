// Package scan wraps the osxphotos CLI to enumerate candidate driving videos
// and also provides just-in-time iCloud download.
//
// Design notes:
//   - osxphotos' --selfie flag does not match videos in practice; the selfie
//     attribute on PhotoKit is only populated for photos. We therefore pull
//     all movies and filter client-side on portrait aspect + duration.
//   - Videos are typically stored in iCloud only ("ismissing": true). Scan
//     enumerates; DownloadVideo downloads a single UUID on demand.
package scan

import (
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
	"time"
)

// osxphotos emits Python's extended JSON with NaN/Infinity/-Infinity, which
// encoding/json rejects. Replace bare-word occurrences with null before decode.
// These only appear as unquoted values in osxphotos output.
// Match bare JSON tokens -Infinity, Infinity, NaN. Word boundary alone
// doesn't help for -Infinity (the leading `-` is a non-word char), so
// match the leading `-` explicitly.
var nonStdJSON = regexp.MustCompile(`-Infinity|Infinity|NaN`)

func sanitizeJSON(b []byte) []byte {
	return nonStdJSON.ReplaceAll(b, []byte("null"))
}

// Candidate is one row of selfie_videos.csv (name kept for parity with Python).
type Candidate struct {
	Process       string // "yes" default; user edits to "no" to skip
	Date          string // ISO date
	DurationHuman string
	DurationSec   float64
	Title         string
	Filename      string
	UUID          string
	Width         int
	Height        int
	Missing       bool // true => not local; must iCloud-download before use
	Portrait      bool
	HasLocation   bool
	Latitude      *float64
	Longitude     *float64
}

type Options struct {
	MinDuration  time.Duration
	MaxDuration  time.Duration
	LandscapeToo bool // default false: portrait-only
}

// Defaults reflect the user's actual driving-video profile (≥ 10 min portrait).
func Defaults() Options {
	return Options{MinDuration: 10 * time.Minute, MaxDuration: 40 * time.Minute}
}

// Query runs osxphotos and returns filtered candidates.
func Query(ctx context.Context, opts Options) ([]Candidate, error) {
	cmd := exec.CommandContext(ctx, "osxphotos", "query", "--only-movies", "--json")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("osxphotos query: %w (stderr: %s)", err, stderr.String())
	}
	var raw []osxPhoto
	if err := json.Unmarshal(sanitizeJSON(stdout.Bytes()), &raw); err != nil {
		return nil, fmt.Errorf("osxphotos json decode: %w", err)
	}

	var out []Candidate
	for _, p := range raw {
		dur := 0.0
		if p.ExifInfo != nil {
			dur = p.ExifInfo.Duration
		}
		if dur < opts.MinDuration.Seconds() || dur > opts.MaxDuration.Seconds() {
			continue
		}
		portrait := p.Height > p.Width
		if !portrait && !opts.LandscapeToo {
			continue
		}
		c := Candidate{
			Process:       "yes",
			Date:          p.Date,
			DurationHuman: humanDuration(dur),
			DurationSec:   dur,
			Title:         p.Title,
			Filename:      firstNonEmpty(p.OriginalFilename, p.Filename),
			UUID:          p.UUID,
			Width:         p.Width,
			Height:        p.Height,
			Missing:       p.IsMissing,
			Portrait:      portrait,
			HasLocation:   p.Latitude != nil && p.Longitude != nil,
			Latitude:      p.Latitude,
			Longitude:     p.Longitude,
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date < out[j].Date })
	return out, nil
}

// WriteCSV writes candidates in a format stable for hand-editing (user flips
// Process from "yes" to "no" to skip).
func WriteCSV(path string, cands []Candidate) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	header := []string{"process", "date", "duration_human", "duration_sec", "title", "filename", "uuid", "width", "height", "missing", "portrait", "has_location", "latitude", "longitude"}
	if err := w.Write(header); err != nil {
		return err
	}
	for _, c := range cands {
		row := []string{
			c.Process,
			c.Date,
			c.DurationHuman,
			strconv.FormatFloat(c.DurationSec, 'f', 2, 64),
			c.Title,
			c.Filename,
			c.UUID,
			strconv.Itoa(c.Width),
			strconv.Itoa(c.Height),
			strconv.FormatBool(c.Missing),
			strconv.FormatBool(c.Portrait),
			strconv.FormatBool(c.HasLocation),
			floatPtr(c.Latitude),
			floatPtr(c.Longitude),
		}
		if err := w.Write(row); err != nil {
			return err
		}
	}
	return nil
}

// ReadCSV reads a previously-written CSV. Only rows with process=="yes" and
// non-empty UUID are returned.
func ReadCSV(path string) ([]Candidate, error) {
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
	iProcess, iDate, iDurHuman, iDurSec, iTitle, iFilename, iUUID, iWidth, iHeight, iMissing, iPortrait :=
		idx("process"), idx("date"), idx("duration_human"), idx("duration_sec"), idx("title"),
		idx("filename"), idx("uuid"), idx("width"), idx("height"), idx("missing"), idx("portrait")
	var out []Candidate
	for _, row := range rows[1:] {
		if len(row) < len(header) {
			continue
		}
		if row[iProcess] != "yes" || row[iUUID] == "" {
			continue
		}
		dur, _ := strconv.ParseFloat(row[iDurSec], 64)
		w, _ := strconv.Atoi(row[iWidth])
		h, _ := strconv.Atoi(row[iHeight])
		miss, _ := strconv.ParseBool(row[iMissing])
		port, _ := strconv.ParseBool(row[iPortrait])
		out = append(out, Candidate{
			Process:       row[iProcess],
			Date:          row[iDate],
			DurationHuman: row[iDurHuman],
			DurationSec:   dur,
			Title:         row[iTitle],
			Filename:      row[iFilename],
			UUID:          row[iUUID],
			Width:         w,
			Height:        h,
			Missing:       miss,
			Portrait:      port,
		})
	}
	return out, nil
}

// AllRows reads every CSV row regardless of process flag — used by `stats`.
func AllRows(path string) ([]Candidate, error) {
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
	iProcess, iDate, iDurSec, iUUID :=
		idx("process"), idx("date"), idx("duration_sec"), idx("uuid")
	var out []Candidate
	for _, row := range rows[1:] {
		if len(row) < len(header) {
			continue
		}
		dur, _ := strconv.ParseFloat(row[iDurSec], 64)
		out = append(out, Candidate{
			Process:     row[iProcess],
			Date:        row[iDate],
			DurationSec: dur,
			UUID:        row[iUUID],
		})
	}
	return out, nil
}

// DownloadVideo uses osxphotos export --download-missing --use-photokit to
// pull a single UUID from iCloud into destDir. Returns the downloaded file
// path.
func DownloadVideo(ctx context.Context, uuid, destDir string) (string, error) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", err
	}
	// Export to a clean per-uuid subdir so the result file is trivial to locate.
	sub := filepath.Join(destDir, uuid)
	if err := os.RemoveAll(sub); err != nil {
		return "", err
	}
	if err := os.MkdirAll(sub, 0o755); err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "osxphotos", "export",
		"--uuid", uuid,
		"--only-movies",
		"--download-missing",
		"--use-photokit",
		"--skip-original-if-edited",
		sub,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("osxphotos export uuid=%s: %w (stderr: %s)", uuid, err, stderr.String())
	}
	// Find the first file in sub.
	entries, err := os.ReadDir(sub)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if !e.IsDir() {
			return filepath.Join(sub, e.Name()), nil
		}
	}
	return "", fmt.Errorf("osxphotos export uuid=%s produced no file (stderr: %s)", uuid, stderr.String())
}

// ---------------------------------------------------------------------------
// internal
// ---------------------------------------------------------------------------

type osxPhoto struct {
	UUID             string    `json:"uuid"`
	Date             string    `json:"date"`
	Title            string    `json:"title"`
	Filename         string    `json:"filename"`
	OriginalFilename string    `json:"original_filename"`
	Width            int       `json:"width"`
	Height           int       `json:"height"`
	IsMissing        bool      `json:"ismissing"`
	InCloud          bool      `json:"incloud"`
	Latitude         *float64  `json:"latitude"`
	Longitude        *float64  `json:"longitude"`
	ExifInfo         *exifInfo `json:"exif_info"`
}

type exifInfo struct {
	Duration float64 `json:"duration"`
}

func humanDuration(secs float64) string {
	s := int(secs)
	return fmt.Sprintf("%d:%02d:%02d", s/3600, (s%3600)/60, s%60)
}

func floatPtr(p *float64) string {
	if p == nil {
		return ""
	}
	return strconv.FormatFloat(*p, 'f', 6, 64)
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
