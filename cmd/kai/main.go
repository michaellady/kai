// Command kai is the Phase 0 POC CLI: scan selfie-style driving videos
// in Apple Photos, transcribe with Gemini, and batch into monthly Google
// Docs for NotebookLM ingestion.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/mikelady/kai/internal/gdocs"
	"github.com/mikelady/kai/internal/gemini"
	"github.com/mikelady/kai/internal/scan"
	"github.com/mikelady/kai/internal/state"
	"github.com/mikelady/kai/internal/youtube"
)

const (
	folderName       = "Kai Transcripts"
	candidatesCSV    = "selfie_videos.csv"
	ytCandidatesCSV  = "youtube_videos.csv"
	processLogPath   = "process_log.json"
	monthlyDocsPath  = "monthly_docs.json"
	clientSecretsIn  = "client_secrets.json"
	tokenPath        = "google_token.json"
	tmpDownloadDir   = "tmp/downloads"
	tmpYTDir         = "tmp/yt"
	kaiLogPath       = "tmp/kai.log"
	delayBetweenVids = 5 * time.Second

	sourceApple   = "apple-photos"
	sourceYouTube = "youtube"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	cmd := os.Args[1]
	args := os.Args[2:]
	// Tee output to tmp/kai.log for long-running commands so `kai tail-run`
	// can follow progress from another terminal.
	var teeClose func()
	if cmd == "process" || cmd == "youtube" {
		c, terr := setupTee()
		if terr != nil {
			fmt.Fprintf(os.Stderr, "warning: tee log setup failed: %v\n", terr)
		} else {
			teeClose = c
			defer teeClose()
		}
	}
	var err error
	switch cmd {
	case "scan":
		err = runScan(ctx, args)
	case "process":
		err = runProcess(ctx, args)
	case "stats":
		err = runStats(ctx, args)
	case "tail-run":
		err = runTailRun(ctx, args)
	case "youtube":
		if len(args) < 1 {
			fmt.Fprintln(os.Stderr, "usage: kai youtube {scan|process} [...]")
			os.Exit(2)
		}
		sub, rest := args[0], args[1:]
		switch sub {
		case "scan":
			err = runYouTubeScan(ctx, rest)
		case "process":
			err = runYouTubeProcess(ctx, rest)
		default:
			fmt.Fprintf(os.Stderr, "unknown youtube subcommand %q\n", sub)
			os.Exit(2)
		}
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `kai — Phase 0 POC

usage:
  kai scan                 [--min-duration 10m] [--max-duration 40m] [--landscape-too]
  kai process              [--limit N] [--allow-partial-monthly] [--model MODEL]
  kai stats
  kai youtube scan         [--channel-url URL]
  kai youtube process      [--limit N] [--allow-partial-monthly] [--model MODEL]
  kai tail-run                       follow tmp/kai.log (live progress of a running process)`)
}

// ---------------------------------------------------------------------------
// scan
// ---------------------------------------------------------------------------

func runScan(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	def := scan.Defaults()
	min := fs.Duration("min-duration", def.MinDuration, "minimum video duration to include")
	max := fs.Duration("max-duration", def.MaxDuration, "maximum video duration to include")
	landscapeToo := fs.Bool("landscape-too", false, "also include landscape videos (default portrait-only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	opts := scan.Options{MinDuration: *min, MaxDuration: *max, LandscapeToo: *landscapeToo}
	fmt.Printf("Scanning Apple Photos for movies (duration %s–%s, %s)…\n",
		opts.MinDuration, opts.MaxDuration, portraitLabel(*landscapeToo))
	cands, err := scan.Query(ctx, opts)
	if err != nil {
		return err
	}
	if err := scan.WriteCSV(candidatesCSV, cands); err != nil {
		return err
	}
	var missing int
	for _, c := range cands {
		if c.Missing {
			missing++
		}
	}
	fmt.Printf("Wrote %s: %d candidates (%d missing from local disk — will iCloud-download on process).\n",
		candidatesCSV, len(cands), missing)
	fmt.Println("Edit the CSV: set `process` to `no` for rows you want to skip.")
	return nil
}

func portraitLabel(landscapeToo bool) string {
	if landscapeToo {
		return "portrait + landscape"
	}
	return "portrait only"
}

// ---------------------------------------------------------------------------
// process
// ---------------------------------------------------------------------------

func runProcess(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("process", flag.ExitOnError)
	limit := fs.Int("limit", 10, "max videos to process this run")
	allowPartial := fs.Bool("allow-partial-monthly", false, "write a monthly overview for every month touched, even if not all videos done")
	modelFlag := fs.String("model", "", "Gemini model (default env GEMINI_MODEL or gemini-2.5-flash)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	model := *modelFlag
	if model == "" {
		model = os.Getenv("GEMINI_MODEL")
	}

	// Load CSV + logs.
	cands, err := scan.ReadCSV(candidatesCSV)
	if err != nil {
		return fmt.Errorf("read %s: %w (run `kai scan` first)", candidatesCSV, err)
	}
	procLog, err := state.LoadProcessLog(processLogPath)
	if err != nil {
		return err
	}
	monthly, err := state.LoadMonthlyDocs(monthlyDocsPath)
	if err != nil {
		return err
	}

	// Pick batch: candidates not yet in procLog.
	var batch []scan.Candidate
	for _, c := range cands {
		if _, done := procLog[c.UUID]; done {
			continue
		}
		batch = append(batch, c)
		if len(batch) >= *limit {
			break
		}
	}
	if len(batch) == 0 {
		fmt.Println("Nothing to process — every `yes` row is already in process_log.json.")
		return nil
	}
	fmt.Printf("Processing %d video(s).\n", len(batch))

	// Clients.
	apiKey, err := gemini.LoadAPIKey()
	if err != nil {
		return err
	}
	gem, err := gemini.NewClient(ctx, apiKey, model)
	if err != nil {
		return fmt.Errorf("gemini client: %w", err)
	}
	gd, err := gdocs.New(ctx, clientSecretsIn, tokenPath)
	if err != nil {
		return fmt.Errorf("gdocs: %w", err)
	}
	folderID, err := gd.GetOrCreateFolder(ctx, folderName)
	if err != nil {
		return fmt.Errorf("folder: %w", err)
	}

	summary := state.RunSummary{StartedAt: time.Now(), Model: model}
	touchedMonths := map[string]bool{}
	docsTouched := map[string]bool{}

	for i, c := range batch {
		fmt.Printf("\n[%d/%d] %s  %s  %s\n", i+1, len(batch), c.Date, c.DurationHuman, c.Filename)
		summary.Attempted++
		if err := processOne(ctx, gem, gd, folderID, &c, procLog, monthly, docsTouched, &summary); err != nil {
			summary.Failed++
			summary.Failures = append(summary.Failures, state.RunFailure{UUID: c.UUID, Stage: stageOf(err), Error: err.Error()})
			fmt.Printf("  FAILED: %v\n", err)
			// persist partial state
			_ = state.SaveProcessLog(processLogPath, procLog)
			_ = state.SaveMonthlyDocs(monthlyDocsPath, monthly)
			continue
		}
		summary.Succeeded++
		touchedMonths[monthKey(c.Date)] = true
		// persist after each video
		if err := state.SaveProcessLog(processLogPath, procLog); err != nil {
			return err
		}
		if err := state.SaveMonthlyDocs(monthlyDocsPath, monthly); err != nil {
			return err
		}
		if i < len(batch)-1 {
			select {
			case <-time.After(delayBetweenVids):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}

	// Monthly overviews.
	if *allowPartial && len(touchedMonths) > 0 {
		fmt.Println("\nCompiling monthly overviews…")
		for m := range touchedMonths {
			mdoc, ok := monthly[m]
			if !ok || mdoc.DocID == "" {
				continue
			}
			body, err := gd.ReadBody(ctx, mdoc.DocID)
			if err != nil {
				fmt.Printf("  %s: read body failed: %v\n", m, err)
				continue
			}
			overview, err := gem.MonthlyOverview(ctx, body)
			if err != nil {
				fmt.Printf("  %s: overview generate failed: %v\n", m, err)
				continue
			}
			// Partial-month disclaimer + markers to make it easy to replace later.
			block := fmt.Sprintf("\n\n---\n\n_Overview compiled from %d recordings so far._\n\n%s\n",
				mdoc.EntryCount, overview)
			if err := gd.AppendEntry(ctx, mdoc.DocID, block); err != nil {
				fmt.Printf("  %s: append overview failed: %v\n", m, err)
				continue
			}
			mdoc.MonthlySummaryDone = "partial"
			monthly[m] = mdoc
			fmt.Printf("  %s: overview written (%d entries).\n", m, mdoc.EntryCount)
		}
		if err := state.SaveMonthlyDocs(monthlyDocsPath, monthly); err != nil {
			return err
		}
	}

	summary.FinishedAt = time.Now()
	summary.WallClockSec = summary.FinishedAt.Sub(summary.StartedAt).Seconds()
	for id := range docsTouched {
		summary.DocsTouched = append(summary.DocsTouched, id)
	}
	sort.Strings(summary.DocsTouched)
	summary.EstimatedCostUSD = summary.GeminiBilledSeconds / 3600.0 * 0.25

	path, err := state.SaveRunSummary(".", summary)
	if err != nil {
		return err
	}
	fmt.Printf("\nTL;DR: %d/%d succeeded, %d failed, ~$%.2f, %s  →  %s\n",
		summary.Succeeded, summary.Attempted, summary.Failed,
		summary.EstimatedCostUSD, fmtDuration(summary.WallClockSec), path)
	return nil
}

func processOne(
	ctx context.Context,
	gem *gemini.Client,
	gd *gdocs.Service,
	folderID string,
	c *scan.Candidate,
	procLog state.ProcessLog,
	monthly state.MonthlyDocs,
	docsTouched map[string]bool,
	summary *state.RunSummary,
) error {
	// 1. Export a local copy via osxphotos. Works whether the video is
	// iCloud-only (downloads) or already local (copies). Always deleted
	// after the entry is appended, regardless of success.
	if c.Missing {
		fmt.Printf("  downloading from iCloud…\n")
	} else {
		fmt.Printf("  exporting local copy…\n")
	}
	localPath, err := scan.DownloadVideo(ctx, c.UUID, tmpDownloadDir)
	if err != nil {
		return stageErr("icloud_download", err)
	}
	defer func() { _ = os.RemoveAll(tmpDownloadDir + "/" + c.UUID) }()

	// 2. Transcribe via Gemini.
	fmt.Printf("  transcribing…\n")
	res, err := gem.Transcribe(ctx, localPath)
	if err != nil {
		return stageErr("gemini", err)
	}
	summary.GeminiBilledSeconds += c.DurationSec

	// 3. Find/create monthly doc.
	mk := monthKey(c.Date)
	ml := monthLabel(c.Date)
	mdoc, ok := monthly[mk]
	if !ok || mdoc.DocID == "" {
		title := fmt.Sprintf("Thoughts — %s", ml)
		// Check Drive first (may exist from a prior run whose state file was lost).
		if id, err := gd.FindDocInFolder(ctx, folderID, title); err != nil {
			return stageErr("drive_find", err)
		} else if id != "" {
			mdoc = state.MonthlyDoc{DocID: id, DocURL: fmt.Sprintf("https://docs.google.com/document/d/%s/edit", id)}
		} else {
			id, url, err := gd.CreateDoc(ctx, folderID, title, ml)
			if err != nil {
				return stageErr("drive_create", err)
			}
			mdoc = state.MonthlyDoc{DocID: id, DocURL: url}
		}
	}

	// 4. Append entry.
	block := formatEntry(c.Date, c.DurationHuman, sourceApple, "", res)
	if err := gd.AppendEntry(ctx, mdoc.DocID, block); err != nil {
		return stageErr("docs_append", err)
	}
	mdoc.EntryCount++
	mdoc.TotalDurationSec += c.DurationSec
	monthly[mk] = mdoc
	docsTouched[mdoc.DocID] = true

	// 5. Update header.
	if err := gd.UpdateHeader(ctx, mdoc.DocID, ml, mdoc.EntryCount, humanDuration(mdoc.TotalDurationSec)); err != nil {
		fmt.Printf("  warning: header update failed: %v\n", err)
	}

	// 6. Record in process log.
	procLog[c.UUID] = state.ProcessLogEntry{
		DocID:       mdoc.DocID,
		DocURL:      mdoc.DocURL,
		MonthKey:    mk,
		Summary:     res.Summary,
		Tags:        res.Tags,
		Date:        c.Date,
		DurationSec: c.DurationSec,
		ProcessedAt: time.Now(),
		Source:      sourceApple,
	}
	fmt.Printf("  done.  tags=%v\n", res.Tags)
	return nil
}

// ---------------------------------------------------------------------------
// youtube scan + process
// ---------------------------------------------------------------------------

func runYouTubeScan(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("youtube scan", flag.ExitOnError)
	url := fs.String("channel-url", youtube.DefaultStreamsURL, "channel /streams URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	fmt.Printf("Listing past livestreams from %s…\n", *url)
	fmt.Println("(This requires full metadata per video — takes a few seconds per stream.)")
	streams, err := youtube.ListPastStreams(ctx, *url)
	if err != nil {
		return err
	}
	if err := youtube.WriteCSV(ytCandidatesCSV, streams); err != nil {
		return err
	}
	fmt.Printf("Wrote %s: %d past livestreams.\n", ytCandidatesCSV, len(streams))
	fmt.Println("Edit the CSV: set `process` to `no` for rows you want to skip.")
	return nil
}

func runYouTubeProcess(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("youtube process", flag.ExitOnError)
	limit := fs.Int("limit", 10, "max videos to process this run")
	allowPartial := fs.Bool("allow-partial-monthly", false, "write a monthly overview for every month touched, even if not all videos done")
	modelFlag := fs.String("model", "", "Gemini model (default env GEMINI_MODEL or gemini-2.5-flash)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	model := *modelFlag
	if model == "" {
		model = os.Getenv("GEMINI_MODEL")
	}

	streams, err := youtube.ReadCSV(ytCandidatesCSV)
	if err != nil {
		return fmt.Errorf("read %s: %w (run `kai youtube scan` first)", ytCandidatesCSV, err)
	}
	procLog, err := state.LoadProcessLog(processLogPath)
	if err != nil {
		return err
	}
	monthly, err := state.LoadMonthlyDocs(monthlyDocsPath)
	if err != nil {
		return err
	}

	var batch []youtube.Stream
	for _, s := range streams {
		if _, done := procLog[s.VideoID]; done {
			continue
		}
		batch = append(batch, s)
		if len(batch) >= *limit {
			break
		}
	}
	if len(batch) == 0 {
		fmt.Println("Nothing to process — every `yes` row is already in process_log.json.")
		return nil
	}
	fmt.Printf("Processing %d livestream(s).\n", len(batch))

	apiKey, err := gemini.LoadAPIKey()
	if err != nil {
		return err
	}
	gem, err := gemini.NewClient(ctx, apiKey, model)
	if err != nil {
		return fmt.Errorf("gemini client: %w", err)
	}
	gd, err := gdocs.New(ctx, clientSecretsIn, tokenPath)
	if err != nil {
		return fmt.Errorf("gdocs: %w", err)
	}
	folderID, err := gd.GetOrCreateFolder(ctx, folderName)
	if err != nil {
		return fmt.Errorf("folder: %w", err)
	}

	summary := state.RunSummary{StartedAt: time.Now(), Model: model}
	touchedMonths := map[string]bool{}
	docsTouched := map[string]bool{}

	for i, s := range batch {
		fmt.Printf("\n[%d/%d] %s  %s  %s\n", i+1, len(batch), s.UploadDate, s.DurationHuman, s.Title)
		summary.Attempted++
		if err := processOneYouTube(ctx, gem, gd, folderID, &s, procLog, monthly, docsTouched, &summary); err != nil {
			summary.Failed++
			summary.Failures = append(summary.Failures, state.RunFailure{UUID: s.VideoID, Stage: stageOf(err), Error: err.Error()})
			fmt.Printf("  FAILED: %v\n", err)
			_ = state.SaveProcessLog(processLogPath, procLog)
			_ = state.SaveMonthlyDocs(monthlyDocsPath, monthly)
			continue
		}
		summary.Succeeded++
		touchedMonths[monthKey(s.UploadDate)] = true
		if err := state.SaveProcessLog(processLogPath, procLog); err != nil {
			return err
		}
		if err := state.SaveMonthlyDocs(monthlyDocsPath, monthly); err != nil {
			return err
		}
		if i < len(batch)-1 {
			select {
			case <-time.After(2 * time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}

	if *allowPartial && len(touchedMonths) > 0 {
		fmt.Println("\nCompiling monthly overviews…")
		for m := range touchedMonths {
			mdoc, ok := monthly[m]
			if !ok || mdoc.DocID == "" {
				continue
			}
			body, err := gd.ReadBody(ctx, mdoc.DocID)
			if err != nil {
				fmt.Printf("  %s: read body failed: %v\n", m, err)
				continue
			}
			overview, err := gem.MonthlyOverview(ctx, body)
			if err != nil {
				fmt.Printf("  %s: overview generate failed: %v\n", m, err)
				continue
			}
			block := fmt.Sprintf("\n\n---\n\n_Overview compiled from %d recordings so far._\n\n%s\n",
				mdoc.EntryCount, overview)
			if err := gd.AppendEntry(ctx, mdoc.DocID, block); err != nil {
				fmt.Printf("  %s: append overview failed: %v\n", m, err)
				continue
			}
			mdoc.MonthlySummaryDone = "partial"
			monthly[m] = mdoc
			fmt.Printf("  %s: overview written (%d entries).\n", m, mdoc.EntryCount)
		}
		if err := state.SaveMonthlyDocs(monthlyDocsPath, monthly); err != nil {
			return err
		}
	}

	summary.FinishedAt = time.Now()
	summary.WallClockSec = summary.FinishedAt.Sub(summary.StartedAt).Seconds()
	for id := range docsTouched {
		summary.DocsTouched = append(summary.DocsTouched, id)
	}
	sort.Strings(summary.DocsTouched)
	summary.EstimatedCostUSD = float64(summary.Succeeded) * 0.006

	path, err := state.SaveRunSummary(".", summary)
	if err != nil {
		return err
	}
	fmt.Printf("\nTL;DR: %d/%d succeeded, %d failed, ~$%.2f, %s  →  %s\n",
		summary.Succeeded, summary.Attempted, summary.Failed,
		summary.EstimatedCostUSD, fmtDuration(summary.WallClockSec), path)
	return nil
}

func processOneYouTube(
	ctx context.Context,
	gem *gemini.Client,
	gd *gdocs.Service,
	folderID string,
	s *youtube.Stream,
	procLog state.ProcessLog,
	monthly state.MonthlyDocs,
	docsTouched map[string]bool,
	_ *state.RunSummary,
) error {
	// 1. Fetch captions.
	fmt.Printf("  fetching captions…\n")
	vttPath, err := youtube.FetchCaptions(ctx, s.VideoID, tmpYTDir)
	if err != nil {
		return stageErr("captions_fetch", err)
	}
	if vttPath == "" {
		return stageErr("captions_unavailable", fmt.Errorf("no auto-captions available yet for %s", s.VideoID))
	}
	defer func() { _ = os.Remove(vttPath) }()

	raw, err := youtube.ParseVTT(vttPath)
	if err != nil {
		return stageErr("captions_parse", err)
	}
	if raw == "" {
		return stageErr("captions_empty", fmt.Errorf("VTT parsed to empty for %s", s.VideoID))
	}

	// 2. Gemini clean + summarize (text only).
	fmt.Printf("  cleaning + summarizing (%d KB of captions)…\n", len(raw)/1024)
	res, err := gem.CleanCaption(ctx, raw)
	if err != nil {
		return stageErr("gemini", err)
	}

	// 3. Monthly doc.
	mk := monthKey(s.UploadDate)
	ml := monthLabel(s.UploadDate)
	mdoc, ok := monthly[mk]
	if !ok || mdoc.DocID == "" {
		title := fmt.Sprintf("Thoughts — %s", ml)
		if id, err := gd.FindDocInFolder(ctx, folderID, title); err != nil {
			return stageErr("drive_find", err)
		} else if id != "" {
			mdoc = state.MonthlyDoc{DocID: id, DocURL: fmt.Sprintf("https://docs.google.com/document/d/%s/edit", id)}
		} else {
			id, url, err := gd.CreateDoc(ctx, folderID, title, ml)
			if err != nil {
				return stageErr("drive_create", err)
			}
			mdoc = state.MonthlyDoc{DocID: id, DocURL: url}
		}
	}

	// 4. Append.
	block := formatEntry(s.UploadDate, s.DurationHuman, sourceYouTube, s.URL, res)
	if err := gd.AppendEntry(ctx, mdoc.DocID, block); err != nil {
		return stageErr("docs_append", err)
	}
	mdoc.EntryCount++
	mdoc.TotalDurationSec += s.DurationSec
	monthly[mk] = mdoc
	docsTouched[mdoc.DocID] = true

	// 5. Header update.
	if err := gd.UpdateHeader(ctx, mdoc.DocID, ml, mdoc.EntryCount, humanDuration(mdoc.TotalDurationSec)); err != nil {
		fmt.Printf("  warning: header update failed: %v\n", err)
	}

	// 6. Log.
	procLog[s.VideoID] = state.ProcessLogEntry{
		DocID:       mdoc.DocID,
		DocURL:      mdoc.DocURL,
		MonthKey:    mk,
		Summary:     res.Summary,
		Tags:        res.Tags,
		Date:        s.UploadDate,
		DurationSec: s.DurationSec,
		ProcessedAt: time.Now(),
		Source:      sourceYouTube,
		SourceURL:   s.URL,
	}
	fmt.Printf("  done.  tags=%v\n", res.Tags)
	return nil
}

func formatEntry(date, durationHuman, source, sourceURL string, res *gemini.TranscribeResult) string {
	tags := strings.Join(res.Tags, ", ")
	srcLine := fmt.Sprintf("**Source:** %s", source)
	if sourceURL != "" {
		srcLine = fmt.Sprintf("**Source:** %s ([link](%s))", source, sourceURL)
	}
	return fmt.Sprintf(
		"\n### %s — %s\n\n%s\n**Tags:** %s\n**Summary:** %s\n\n%s\n\n---\n",
		date, durationHuman, srcLine, tags, res.Summary, res.Transcript,
	)
}

// ---------------------------------------------------------------------------
// stats
// ---------------------------------------------------------------------------

func runStats(_ context.Context, _ []string) error {
	cands, err := scan.AllRows(candidatesCSV)
	if err != nil {
		return fmt.Errorf("read %s: %w", candidatesCSV, err)
	}
	procLog, err := state.LoadProcessLog(processLogPath)
	if err != nil {
		return err
	}
	monthly, err := state.LoadMonthlyDocs(monthlyDocsPath)
	if err != nil {
		return err
	}

	var totalSec, markedSec, remainingSec float64
	var marked, processed int
	for _, c := range cands {
		totalSec += c.DurationSec
		if c.Process == "yes" {
			marked++
			markedSec += c.DurationSec
			if _, done := procLog[c.UUID]; !done {
				remainingSec += c.DurationSec
			}
		}
		if _, done := procLog[c.UUID]; done {
			processed++
		}
	}
	fmt.Printf("Scanned candidates:     %d  (%s)\n", len(cands), fmtDuration(totalSec))
	fmt.Printf("Marked `yes` to process: %d  (%s)\n", marked, fmtDuration(markedSec))
	fmt.Printf("Already processed:      %d\n", processed)
	fmt.Printf("Remaining to process:   %s  (~$%.2f Gemini)\n", fmtDuration(remainingSec), remainingSec/3600.0*0.25)

	// Monthly docs.
	if len(monthly) > 0 {
		fmt.Println("\nMonthly docs:")
		keys := make([]string, 0, len(monthly))
		for k := range monthly {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			m := monthly[k]
			marker := " "
			if m.MonthlySummaryDone != "" {
				marker = "✓"
			}
			fmt.Printf("  %s  %s  %3d entries  %s\n", marker, k, m.EntryCount, fmtDuration(m.TotalDurationSec))
		}
	}

	// Top tags.
	tagCount := map[string]int{}
	for _, e := range procLog {
		for _, t := range e.Tags {
			tagCount[t]++
		}
	}
	if len(tagCount) > 0 {
		type kv struct {
			k string
			v int
		}
		tags := make([]kv, 0, len(tagCount))
		for k, v := range tagCount {
			tags = append(tags, kv{k, v})
		}
		sort.Slice(tags, func(i, j int) bool {
			if tags[i].v != tags[j].v {
				return tags[i].v > tags[j].v
			}
			return tags[i].k < tags[j].k
		})
		fmt.Println("\nTop tags:")
		limit := 20
		if len(tags) < limit {
			limit = len(tags)
		}
		maxN := tags[0].v
		for _, t := range tags[:limit] {
			bar := strings.Repeat("█", (t.v*30)/maxN)
			fmt.Printf("  %-24s %s %d\n", t.k, bar, t.v)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func monthKey(dateISO string) string {
	if len(dateISO) >= 7 {
		return dateISO[:7]
	}
	return dateISO
}

func monthLabel(dateISO string) string {
	t, err := time.Parse("2006-01-02", dateISO[:10])
	if err != nil {
		return monthKey(dateISO)
	}
	return t.Format("January 2006")
}

func fmtDuration(sec float64) string {
	s := int(sec)
	return fmt.Sprintf("%d:%02d:%02d", s/3600, (s%3600)/60, s%60)
}

func humanDuration(sec float64) string { return fmtDuration(sec) }

// stageErr is a tiny typed-wrapper that lets us extract the failing pipeline
// stage for the run_summary artifact.
type stagedError struct {
	stage string
	err   error
}

func (s *stagedError) Error() string { return s.err.Error() }
func (s *stagedError) Unwrap() error { return s.err }
func stageErr(stage string, err error) error {
	return &stagedError{stage: stage, err: err}
}
func stageOf(err error) string {
	var s *stagedError
	if errors.As(err, &s) {
		return s.stage
	}
	return "unknown"
}

// ---------------------------------------------------------------------------
// tee stdout/stderr to tmp/kai.log for live tailing
// ---------------------------------------------------------------------------

func setupTee() (func(), error) {
	if err := os.MkdirAll("tmp", 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(kaiLogPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	origOut, origErr := os.Stdout, os.Stderr

	rOut, wOut, err := os.Pipe()
	if err != nil {
		f.Close()
		return nil, err
	}
	os.Stdout = wOut
	doneOut := make(chan struct{})
	go func() { _, _ = io.Copy(io.MultiWriter(origOut, f), rOut); close(doneOut) }()

	rErr, wErr, err := os.Pipe()
	if err != nil {
		wOut.Close()
		<-doneOut
		f.Close()
		os.Stdout = origOut
		return nil, err
	}
	os.Stderr = wErr
	doneErr := make(chan struct{})
	go func() { _, _ = io.Copy(io.MultiWriter(origErr, f), rErr); close(doneErr) }()

	return func() {
		wOut.Close()
		wErr.Close()
		<-doneOut
		<-doneErr
		f.Close()
		os.Stdout = origOut
		os.Stderr = origErr
	}, nil
}

// runTailRun follows tmp/kai.log, printing existing content then streaming
// appends. Exits on Ctrl-C.
func runTailRun(ctx context.Context, _ []string) error {
	f, err := os.Open(kaiLogPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no log at %s (no process run has happened yet)", kaiLogPath)
		}
		return err
	}
	defer f.Close()
	// Print whatever's already there.
	if _, err := io.Copy(os.Stdout, f); err != nil {
		return err
	}
	// Poll for appends.
	buf := make([]byte, 8192)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(400 * time.Millisecond):
		}
		for {
			n, err := f.Read(buf)
			if n > 0 {
				os.Stdout.Write(buf[:n])
			}
			if err == io.EOF || n == 0 {
				break
			}
			if err != nil {
				return err
			}
		}
	}
}
