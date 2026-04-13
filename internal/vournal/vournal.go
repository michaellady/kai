// Package vournal ingests a plain-text Vournal journal dump — a single file
// of dated entries, each a GPT-era transcription of an old video journal
// recording — into a shape Kai can file into monthly Docs.
//
// The text is already transcribed; Gemini is only used for summary+tags.
package vournal

import (
	"bufio"
	"encoding/csv"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Entry is one dated journal entry.
type Entry struct {
	Process string // "yes" default
	Date    string // YYYY-MM-DD
	EntryID string // "vournal:YYYY-MM-DD:N"
	Preview string // first 80 chars of body
	Body    string // full entry text (not persisted in CSV)
}

var dateHeaderRE = regexp.MustCompile(`^(\d{1,2})/(\d{1,2})/(\d{2,4})\s*$`)

// ParseFile splits the file into per-date entries. Same-date duplicates get
// :1, :2, :3 suffixes in the order they appear.
func ParseFile(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var entries []Entry
	var curDate string
	var curLines []string

	flush := func() {
		if curDate == "" {
			curLines = nil
			return
		}
		body := strings.TrimSpace(strings.Join(curLines, "\n"))
		curLines = nil
		if body == "" {
			return
		}
		entries = append(entries, Entry{Process: "yes", Date: curDate, Body: body})
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if iso := parseDateHeader(strings.TrimSpace(line)); iso != "" {
			flush()
			curDate = iso
			continue
		}
		if curDate == "" {
			// Pre-first-date preamble (e.g. "Recovered transcripts "). Skip.
			continue
		}
		curLines = append(curLines, line)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	flush()

	// Assign per-date ordinals and previews.
	counts := map[string]int{}
	for i := range entries {
		counts[entries[i].Date]++
		n := counts[entries[i].Date]
		entries[i].EntryID = fmt.Sprintf("vournal:%s:%d", entries[i].Date, n)
		entries[i].Preview = previewOf(entries[i].Body, 80)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Date != entries[j].Date {
			return entries[i].Date < entries[j].Date
		}
		return entries[i].EntryID < entries[j].EntryID
	})
	return entries, nil
}

// StripNoise removes audio-cue annotations that the old-GPT transcription
// left sprinkled through the text. Intentionally conservative — drops only
// stand-alone cue lines, not mid-sentence occurrences, to avoid touching
// parenthetical content that might carry semantic meaning.
func StripNoise(body string) string {
	// Per-line filters.
	var out []string
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			out = append(out, "")
			continue
		}
		// Skip lines that are ONLY a bracketed marker: [BLANK_AUDIO], [MUSIC] etc.
		if bracketOnlyRE.MatchString(t) {
			continue
		}
		// Skip lines that are ONLY a parenthesized short descriptor:
		//   (wind howling), (tense music), (dramatic music), (wind blowing)
		if parenOnlyRE.MatchString(t) {
			continue
		}
		out = append(out, line)
	}
	// Collapse runs of blank lines.
	joined := strings.Join(out, "\n")
	joined = multiBlankRE.ReplaceAllString(joined, "\n\n")
	return strings.TrimSpace(joined)
}

var (
	bracketOnlyRE = regexp.MustCompile(`^\[[A-Z_ ]+\]$`)
	parenOnlyRE   = regexp.MustCompile(`^\(\s*[a-zA-Z][a-zA-Z\s]{0,40}\s*\)$`)
	multiBlankRE  = regexp.MustCompile(`\n{3,}`)
)

// ---------------------------------------------------------------------------
// CSV
// ---------------------------------------------------------------------------

func WriteCSV(path string, entries []Entry) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	if err := w.Write([]string{"process", "date", "entry_id", "word_count", "preview"}); err != nil {
		return err
	}
	for _, e := range entries {
		wc := len(strings.Fields(e.Body))
		if err := w.Write([]string{e.Process, e.Date, e.EntryID, strconv.Itoa(wc), e.Preview}); err != nil {
			return err
		}
	}
	return nil
}

// ReadCSV returns rows marked process=="yes".
func ReadCSV(path string) ([]Entry, error) {
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
	iProc, iDate, iID := idx("process"), idx("date"), idx("entry_id")
	var out []Entry
	for _, row := range rows[1:] {
		if len(row) < len(header) {
			continue
		}
		if row[iProc] != "yes" || row[iID] == "" {
			continue
		}
		out = append(out, Entry{Process: row[iProc], Date: row[iDate], EntryID: row[iID]})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func parseDateHeader(line string) string {
	m := dateHeaderRE.FindStringSubmatch(line)
	if m == nil {
		return ""
	}
	mo, err1 := strconv.Atoi(m[1])
	d, err2 := strconv.Atoi(m[2])
	y, err3 := strconv.Atoi(m[3])
	if err1 != nil || err2 != nil || err3 != nil {
		return ""
	}
	if y < 100 {
		y += 2000
	}
	if mo < 1 || mo > 12 || d < 1 || d > 31 {
		return ""
	}
	t := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC)
	return t.Format("2006-01-02")
}

func previewOf(body string, n int) string {
	cleaned := strings.Join(strings.Fields(body), " ")
	if len(cleaned) <= n {
		return cleaned
	}
	return cleaned[:n]
}
