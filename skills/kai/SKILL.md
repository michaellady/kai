---
name: kai
description: Operate the Kai personal-knowledge pipeline — scan four sources (Apple Photos selfies, YouTube past livestreams, beehiiv RSS newsletter, recovered Vournal transcripts), transcribe via Gemini, file into monthly Google Docs, sync NotebookLM. Use when the user says "run kai", "kai ingest", "kai stats", "sync my knowledge base", "what's in kai", or names a specific source (apple / youtube / newsletter / vournal).
---

# Kai operator skill

Kai is a Go CLI at `/Users/mikelady/dev/kai/kai` that ingests four content sources into one set of monthly Google Docs consumed by NotebookLM. This skill captures the operational knowledge needed to run it without re-deriving it every time.

## Fast triggers

- **"run kai" / "kai ingest"** — full sweep: for each source, scan → process. Long.
- **"ingest apple" / "process selfies" / "run selfie batch"** — `./kai scan && ./kai process --limit 500 --allow-partial-monthly`.
- **"ingest youtube" / "process livestreams"** — `./kai youtube scan && ./kai youtube process --limit 100 --allow-partial-monthly`.
- **"ingest newsletter" / "process posts"** — `./kai newsletter scan && ./kai newsletter process --limit 100 --allow-partial-monthly`.
- **"ingest vournal" / "process recovered"** — `./kai vournal scan && ./kai vournal process --limit 200 --allow-partial-monthly`.
- **"kai stats" / "what's in kai"** — `./kai stats`.
- **"kai preview" / "show the candidates"** — `go run ./cmd/preview`.
- **"rebuild kai state" / "fix kai state"** — `go run ./cmd/rebuild-state`.
- **"tail kai" / "watch kai"** — `./kai tail-run`.

## Always do

1. **Work from `/Users/mikelady/dev/kai`.** `cd` there or use absolute paths.
2. **`export PATH="$HOME/.local/bin:$PATH"`** before any command that shells out to `osxphotos` (pipx-installed tools live there).
3. **Never run two `process` commands concurrently.** They race on `process_log.json` + `monthly_docs.json`. If the user requests parallel runs, explain the race and sequence them instead.
4. **Kick long runs with `run_in_background: true`.** Anything touching ≥ 20 items will take more than 10 minutes.
5. **After the run starts, use `ScheduleWakeup`** to check progress on a cadence appropriate to expected duration (see "Monitoring pace" below). Don't sleep; the wakeup system was built for this.
6. **Always tell the user to hit the NotebookLM refresh icon per source** after a new ingestion — re-adding creates duplicate sources.

## Prerequisites (verify before first run of a session)

- `client_secrets.json` + `gemini_api_key.txt` + `google_token.json` present in repo root — if missing, stop and point the user at the setup section of `README.md`. Do not try to create them.
- `ffmpeg`, `osxphotos`, `yt-dlp` on PATH.
- macOS **Settings → Photos → Optimize Mac Storage** on, otherwise iCloud originals accumulate ~200 GB.
- Gemini API billing enabled on the linked GCP project. Free tier caps at 20 requests/day.

## Source cheat sheet

| Source | Scan output CSV | Process writes | Gemini flow | Notes |
|---|---|---|---|---|
| Apple Photos | `selfie_videos.csv` | Apple UUID → process_log | Audio upload + Transcribe + Summary | Portrait 10-40 min default. `--min-duration 5m` widens to selfie-monologue territory but picks up BJJ clips. |
| YouTube | `youtube_videos.csv` | Video ID → process_log | Caption fetch + CleanCaption + Summary | Uses auto-captions; recent streams may need re-run once YouTube generates them. |
| Newsletter | `newsletter_posts.csv` | Post URL → process_log | HTML strip + Summary only | Default feed is EVC beehiiv. Override with `--feed-url`. |
| Vournal | `vournal_entries.csv` | `vournal:DATE:N` → process_log | Noise strip + Summary only | Default file is `recovered_vournal_transcripts`. One-time recovery source. |

## Monitoring pace

Typical per-item wall-clock:

- Apple Photos: **2–3 min/video** (iCloud download dominates).
- YouTube: **2–3 min/stream**.
- Newsletter: **~20 sec/post**.
- Vournal: **~10 sec/entry**.

Monthly-overview compilation at end adds **~20–30 sec per touched month**. Batches spanning many years will spend 5–15 min on overviews.

Cadence for scheduled checks: first check at ~10% of expected wall-clock, then halve toward completion. For a 3-hour batch: 20 min → 60 min → 30 min → 15 min.

## Failure patterns + fixes

| Symptom | Cause | Fix |
|---|---|---|
| `ffmpeg extract audio: ... pcm_s16be ... not currently supported in container` | Source video has PCM audio (Sony/Panasonic C-series MP4s). | Already mitigated: `internal/scan/scan.go` uses `-acodec aac -b:a 128k` to transcode rather than copy. |
| `wait active: file processing failed` | iPhone video too large (>1 GB) choked Gemini's file processor. | Audio-extract path (current default) sidesteps this. If it recurs, check that ffmpeg step ran. |
| `RESOURCE_EXHAUSTED`, daily quota | Free-tier Gemini hit. | Enable billing on the GCP project linked to the key; no code change. |
| `Your project has been denied access` (PERMISSION_DENIED) | GCP billing flagged mid-run. | User must resolve via Cloud Console billing. Retry with `kai process --limit 50` once cleared. |
| `summary parse: decode summary...` | Gemini output-token cap truncated JSON. | Already mitigated: salvage path extracts partial summary, writes entry with empty tags. |
| `osxphotos export ...: signal: killed` | Per-video download timeout (15 min). | Retry — re-running `kai process` picks up only the failed row via `process_log.json`. |
| `captions_unavailable` | YouTube hasn't generated auto-captions for a recent livestream yet. | Re-run after a few hours. |
| State-file race (missing entries in `process_log.json` after concurrent runs) | Two `process` commands writing the same JSON. | `go run ./cmd/rebuild-state` — rebuilds from Google Docs as source of truth. |

## What not to do

- Don't propose adding a BJJ classifier, video-content filter, or album-based scan without being asked. User declined these; the current duration heuristic is considered good enough.
- Don't edit the Python reference file `thoughtstream_v3.py`. It stays as a comparison artifact.
- Don't run `osxphotos export` without `--download-missing` when the video is iCloud-only — it silently succeeds with nothing.
- Don't `rm -rf tmp/` mid-run.
- Don't suggest Twitter posts when promoting Kai updates via social — user has no Twitter Buffer channel (see `~/.claude/projects/-Users-mikelady-dev-kai/memory/reference_buffer_channels.md`).

## Verification after any ingestion

1. `./kai stats` shows the expected new count by source.
2. Open one monthly doc in the `Kai Transcripts` Drive folder, confirm:
   - `<!-- kai:header:start -->` / `kai:header:end` markers both present exactly once.
   - New entries have a `**Source:**` header line.
   - No duplicated entries (scan for repeated URLs).
3. Tell user to sync NotebookLM per-source (refresh icon, not "Add from Drive").
