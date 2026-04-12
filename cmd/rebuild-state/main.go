// cmd/rebuild-state reconstructs process_log.json and monthly_docs.json
// from the actual contents of the Kai Transcripts folder in Google Drive.
//
// Needed because concurrent `kai process` / `kai youtube process` /
// `kai newsletter process` runs race on the state files — the Google Docs
// themselves are the source of truth.
//
// Usage: go run ./cmd/rebuild-state
package main

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/mikelady/kai/internal/gdocs"
	"github.com/mikelady/kai/internal/state"
	drive "google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"encoding/json"
	docs "google.golang.org/api/docs/v1"
)

const folderName = "Kai Transcripts"

func main() {
	ctx := context.Background()

	// gdocs.New constructs Drive + Docs under the hood; we also want the
	// raw Drive service to list the folder's docs. Simplest: duplicate a
	// tiny OAuth dance here since the fields on gdocs.Service are unexported.
	driveSvc, docsSvc, err := authServices(ctx)
	if err != nil {
		die(err)
	}
	_ = docsSvc // readBody uses it below via svc

	svc, err := gdocs.New(ctx, "client_secrets.json", "google_token.json")
	if err != nil {
		die(err)
	}

	// Find folder.
	q := fmt.Sprintf("name = %q and mimeType = 'application/vnd.google-apps.folder' and trashed = false", folderName)
	res, err := driveSvc.Files.List().Q(q).Spaces("drive").Fields("files(id,name)").Context(ctx).Do()
	if err != nil {
		die(err)
	}
	if len(res.Files) == 0 {
		die(fmt.Errorf("folder %q not found", folderName))
	}
	folderID := res.Files[0].Id

	// List every doc in folder.
	q2 := fmt.Sprintf("%q in parents and mimeType = 'application/vnd.google-apps.document' and trashed = false", folderID)
	docList, err := driveSvc.Files.List().Q(q2).Spaces("drive").Fields("files(id,name,webViewLink)").Context(ctx).Do()
	if err != nil {
		die(err)
	}
	fmt.Printf("Found %d docs in %s.\n", len(docList.Files), folderName)

	// Preserve any existing real entries so we don't destroy tags/summary we
	// already captured correctly.
	existingLog, _ := state.LoadProcessLog("process_log.json")
	newLog := state.ProcessLog{}
	for k, v := range existingLog {
		newLog[k] = v
	}

	newMonthly := state.MonthlyDocs{}

	for _, f := range docList.Files {
		body, err := svc.ReadBody(ctx, f.Id)
		if err != nil {
			fmt.Printf("  %s: read failed: %v\n", f.Name, err)
			continue
		}
		mk, ml := monthFromDocTitle(f.Name)
		if mk == "" {
			fmt.Printf("  %s: not a monthly doc, skipping\n", f.Name)
			continue
		}
		entries, totalDur := parseEntries(body)
		fmt.Printf("  %s → %s: %d entries (total %s)\n", f.Name, mk, len(entries), fmtDur(totalDur))

		summaryDone := ""
		if strings.Contains(body, "Overview compiled from") {
			summaryDone = "partial"
		}
		newMonthly[mk] = state.MonthlyDoc{
			DocID:              f.Id,
			DocURL:             fmt.Sprintf("https://docs.google.com/document/d/%s/edit", f.Id),
			EntryCount:         len(entries),
			TotalDurationSec:   totalDur,
			MonthlySummaryDone: summaryDone,
		}
		_ = ml

		for _, e := range entries {
			key := keyForEntry(e)
			if key == "" {
				continue
			}
			existing, hasExisting := newLog[key]
			if hasExisting && existing.Summary != "" && len(existing.Tags) > 0 {
				// Already have a good record; don't clobber it.
				continue
			}
			newLog[key] = state.ProcessLogEntry{
				DocID:       f.Id,
				DocURL:      fmt.Sprintf("https://docs.google.com/document/d/%s/edit", f.Id),
				MonthKey:    mk,
				Summary:     e.Summary,
				Tags:        e.Tags,
				Date:        e.Date,
				DurationSec: e.DurationSec,
				ProcessedAt: time.Now(),
				Source:      e.Source,
				SourceURL:   e.SourceURL,
			}
		}
	}

	if err := state.SaveProcessLog("process_log.json", newLog); err != nil {
		die(err)
	}
	if err := state.SaveMonthlyDocs("monthly_docs.json", newMonthly); err != nil {
		die(err)
	}
	fmt.Printf("\nRebuilt process_log.json (%d entries) and monthly_docs.json (%d months).\n",
		len(newLog), len(newMonthly))
}

// ---------------------------------------------------------------------------
// parsing
// ---------------------------------------------------------------------------

type entry struct {
	Date        string
	DurationSec float64
	Source      string
	SourceURL   string
	Tags        []string
	Summary     string
}

var (
	headerRE = regexp.MustCompile(`^### (\d{4}-\d{2}-\d{2})(?: — (\d+):(\d+):(\d+))?\s*$`)
	srcRE    = regexp.MustCompile(`^\*\*Source:\*\* (\S+)(?: \(\[link\]\((\S+)\)\))?\s*$`)
	tagsRE   = regexp.MustCompile(`^\*\*Tags:\*\* (.+)$`)
	sumRE    = regexp.MustCompile(`^\*\*Summary:\*\* (.+)$`)
)

func parseEntries(body string) ([]entry, float64) {
	lines := strings.Split(body, "\n")
	var out []entry
	var total float64
	var cur *entry
	for _, line := range lines {
		if m := headerRE.FindStringSubmatch(line); m != nil {
			if cur != nil {
				out = append(out, *cur)
				total += cur.DurationSec
			}
			dur := 0.0
			if m[2] != "" {
				var h, mn, s int
				fmt.Sscanf(m[2]+":"+m[3]+":"+m[4], "%d:%d:%d", &h, &mn, &s)
				dur = float64(h*3600 + mn*60 + s)
			}
			cur = &entry{Date: m[1], DurationSec: dur}
			continue
		}
		if cur == nil {
			continue
		}
		if m := srcRE.FindStringSubmatch(line); m != nil {
			cur.Source = m[1]
			cur.SourceURL = m[2]
			continue
		}
		if m := tagsRE.FindStringSubmatch(line); m != nil {
			raw := strings.TrimSpace(m[1])
			if raw != "" {
				parts := strings.Split(raw, ",")
				for _, p := range parts {
					t := strings.TrimSpace(p)
					if t != "" {
						cur.Tags = append(cur.Tags, t)
					}
				}
			}
			continue
		}
		if m := sumRE.FindStringSubmatch(line); m != nil {
			cur.Summary = strings.TrimSpace(m[1])
			continue
		}
	}
	if cur != nil {
		out = append(out, *cur)
		total += cur.DurationSec
	}
	return out, total
}

var ytIDRE = regexp.MustCompile(`watch\?v=([a-zA-Z0-9_-]+)`)

func keyForEntry(e entry) string {
	switch e.Source {
	case "newsletter":
		return e.SourceURL
	case "youtube":
		// main.go keys the process log by bare YouTube video ID to match
		// what `kai youtube process` writes. Extract it from the watch URL.
		if m := ytIDRE.FindStringSubmatch(e.SourceURL); m != nil {
			return m[1]
		}
		return e.SourceURL
	}
	// apple-photos entries don't carry their UUID in the doc; nothing to key on.
	// We haven't processed any yet, so returning "" is fine.
	return ""
}

var docTitleRE = regexp.MustCompile(`^Thoughts — (January|February|March|April|May|June|July|August|September|October|November|December) (\d{4})$`)

func monthFromDocTitle(title string) (string, string) {
	m := docTitleRE.FindStringSubmatch(title)
	if m == nil {
		return "", ""
	}
	t, err := time.Parse("January 2006", m[1]+" "+m[2])
	if err != nil {
		return "", ""
	}
	return t.Format("2006-01"), t.Format("January 2006")
}

// ---------------------------------------------------------------------------
// inline OAuth for Drive list (gdocs.Service doesn't expose the raw client)
// ---------------------------------------------------------------------------

func authServices(ctx context.Context) (*drive.Service, *docs.Service, error) {
	secrets, err := os.ReadFile("client_secrets.json")
	if err != nil {
		return nil, nil, err
	}
	cfg, err := google.ConfigFromJSON(secrets, drive.DriveFileScope, docs.DocumentsScope)
	if err != nil {
		return nil, nil, err
	}
	b, err := os.ReadFile("google_token.json")
	if err != nil {
		return nil, nil, err
	}
	var tok oauth2.Token
	if err := json.Unmarshal(b, &tok); err != nil {
		return nil, nil, err
	}
	ts := cfg.TokenSource(ctx, &tok)
	driveSvc, err := drive.NewService(ctx, option.WithTokenSource(ts))
	if err != nil {
		return nil, nil, err
	}
	docsSvc, err := docs.NewService(ctx, option.WithTokenSource(ts))
	if err != nil {
		return nil, nil, err
	}
	return driveSvc, docsSvc, nil
}

func fmtDur(sec float64) string {
	s := int(sec)
	return fmt.Sprintf("%d:%02d:%02d", s/3600, (s%3600)/60, s%60)
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
