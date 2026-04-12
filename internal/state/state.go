// Package state persists per-video and per-month progress across Kai runs,
// and emits an audit trail after each process run.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ProcessLog maps video UUID -> ProcessLogEntry.
type ProcessLog map[string]ProcessLogEntry

type ProcessLogEntry struct {
	DocID       string    `json:"doc_id"`
	DocURL      string    `json:"doc_url"`
	MonthKey    string    `json:"month_key"`
	Summary     string    `json:"summary"`
	Tags        []string  `json:"tags"`
	Date        string    `json:"date"`
	DurationSec float64   `json:"duration_sec"`
	ProcessedAt time.Time `json:"processed_at"`
	Source      string    `json:"source,omitempty"` // "apple-photos" | "youtube"; empty = legacy apple-photos
	SourceURL   string    `json:"source_url,omitempty"`
}

// MonthlyDocs maps "YYYY-MM" -> MonthlyDoc.
type MonthlyDocs map[string]MonthlyDoc

type MonthlyDoc struct {
	DocID              string  `json:"doc_id"`
	DocURL             string  `json:"doc_url"`
	EntryCount         int     `json:"entry_count"`
	TotalDurationSec   float64 `json:"total_duration_sec"`
	MonthlySummaryDone string  `json:"monthly_summary_done"` // "", "partial", "full"
}

func LoadProcessLog(path string) (ProcessLog, error) {
	var out ProcessLog
	if err := loadJSON(path, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = make(ProcessLog)
	}
	return out, nil
}

func SaveProcessLog(path string, log ProcessLog) error { return saveJSONAtomic(path, log) }

func LoadMonthlyDocs(path string) (MonthlyDocs, error) {
	var out MonthlyDocs
	if err := loadJSON(path, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = make(MonthlyDocs)
	}
	return out, nil
}

func SaveMonthlyDocs(path string, docs MonthlyDocs) error { return saveJSONAtomic(path, docs) }

// RunSummary is the audit artifact written at the end of a process batch.
type RunSummary struct {
	StartedAt           time.Time    `json:"started_at"`
	FinishedAt          time.Time    `json:"finished_at"`
	WallClockSec        float64      `json:"wall_clock_sec"`
	Attempted           int          `json:"attempted"`
	Succeeded           int          `json:"succeeded"`
	Failed              int          `json:"failed"`
	Failures            []RunFailure `json:"failures"`
	GeminiBilledSeconds float64      `json:"gemini_billed_seconds"`
	EstimatedCostUSD    float64      `json:"estimated_cost_usd"`
	DocsTouched         []string     `json:"docs_touched"`
	Model               string       `json:"model"`
}

type RunFailure struct {
	UUID  string `json:"uuid"`
	Stage string `json:"stage"`
	Error string `json:"error"`
}

// SaveRunSummary writes run_summary_<RFC3339>.json in dir and returns the path.
func SaveRunSummary(dir string, s RunSummary) (string, error) {
	name := fmt.Sprintf("run_summary_%s.json", s.FinishedAt.UTC().Format("20060102T150405Z"))
	path := filepath.Join(dir, name)
	return path, saveJSONAtomic(path, s)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func loadJSON(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	return json.NewDecoder(f).Decode(v)
}

func saveJSONAtomic(path string, v any) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
