# Kai

> **"Thoughts become things."** — Kai Greene

Named for the bodybuilder and two-time Mr. Olympia runner-up Kai Greene, whose line "thoughts become things" is the whole point. Kai the tool turns thinking — driving monologues, livestream riffs, newsletter drafts — into durable, queryable things.

## What it does

Kai ingests three content sources, transcribes or cleans each via Gemini, and files everything into one Google Doc per month. Point NotebookLM at the `Kai Transcripts` folder and you can query years of your own thinking by topic, month, or cross-source evolution.

```
  ┌──────────────────┐   ┌──────────────────┐   ┌──────────────────┐
  │  Apple Photos    │   │  YouTube channel │   │  beehiiv RSS     │
  │  (selfie MOVs)   │   │  (past streams)  │   │  (newsletter)    │
  └────────┬─────────┘   └────────┬─────────┘   └────────┬─────────┘
           │ osxphotos            │ yt-dlp                │ encoding/xml
           │ + ffmpeg (audio)     │ + auto-captions       │ + HTML strip
           ▼                       ▼                       ▼
  ┌──────────────────────────────────────────────────────────────┐
  │                      Gemini 2.5 Flash                         │
  │   audio → transcript+summary  |  captions → clean+summary    │
  │                               |  prose → summary only        │
  └───────────────────────────┬──────────────────────────────────┘
                              ▼
  ┌──────────────────────────────────────────────────────────────┐
  │            Google Drive → `Kai Transcripts` folder            │
  │           1 Doc per YYYY-MM, mixed sources tagged             │
  └───────────────────────────┬──────────────────────────────────┘
                              ▼
                      ┌───────────────┐
                      │  NotebookLM   │
                      └───────────────┘
```

See `KAI_PLAN.md` for the full project vision across all phases.

## Prerequisites

```bash
brew install go pipx yt-dlp ffmpeg
pipx install osxphotos
```

- **Go 1.24+** (tested on 1.25–1.26).
- **osxphotos** shells out to macOS PhotoKit. On first use, grant Terminal (or iTerm) access under **System Settings → Privacy & Security → Photos**.
- **ffmpeg** extracts audio from iPhone videos before Gemini upload. Without it the Apple-Photos path won't work (iPhone .MOV files are often 1 GB+ and Gemini's file processor chokes).
- **Optimize Mac Storage** (Settings → Photos) should be ON before a large batch, otherwise ~200 GB of iCloud originals will land in your Photos library.

## One-time setup

### 1. Gemini API key (paid tier recommended)

Get a key at <https://aistudio.google.com/apikey>:

```bash
printf '%s' '<YOUR_KEY>' > gemini_api_key.txt
chmod 600 gemini_api_key.txt
```

Or set `GEMINI_API_KEY` in your environment.

**Enable billing** on the Google Cloud project that owns the key — the free tier caps at 20 requests/day, which is not enough for any real batch. Paid tier for `gemini-2.5-flash` is well under $0.30 per hour of audio and $0.001 per newsletter post.

### 2. Google Cloud OAuth (for Drive + Docs)

1. <https://console.cloud.google.com/> → create project `kai-poc`.
2. **APIs & Services** → enable **Google Drive API** and **Google Docs API**.
3. **OAuth consent screen** → External → add your email as a test user.
   Scopes:
   - `https://www.googleapis.com/auth/drive.file`
   - `https://www.googleapis.com/auth/documents`
4. **Credentials** → **Create Credentials** → **OAuth client ID** → **Desktop app** → download the JSON.
5. Save it here as `client_secrets.json`.

On first `kai process` run, a browser opens for consent. The token is cached in `google_token.json` and auto-refreshed on expiry.

## Build

```bash
go build -o kai ./cmd/kai
```

## Usage

```bash
./kai scan                                       # Apple Photos → selfie_videos.csv
./kai process --limit 10 --allow-partial-monthly
./kai youtube scan                               # YouTube past livestreams → youtube_videos.csv
./kai youtube process --limit 5 --allow-partial-monthly
./kai newsletter scan                            # beehiiv RSS → newsletter_posts.csv
./kai newsletter process --limit 5 --allow-partial-monthly
./kai stats                                      # tag landscape + per-month counts
./kai tail-run                                   # follow tmp/kai.log from a second shell
```

Edit the CSVs between `scan` and `process`: flip `process` from `yes` to `no` for rows you want to skip. All three sources write to the same monthly Google Docs, tagged with `**Source:** apple-photos | youtube | newsletter`.

### Flags

| Command | Flag | Default | Notes |
|---|---|---|---|
| scan | `--min-duration` | `10m` | Videos shorter than this are dropped. |
| scan | `--max-duration` | `40m` | Videos longer than this are dropped. |
| scan | `--landscape-too` | off | By default only portrait-aspect videos are included. |
| process | `--limit` | `10` | Max videos this run. |
| process | `--allow-partial-monthly` | off | Generate a monthly overview even when a month is not fully processed yet. |
| process | `--dry-run` | off | Print what would be processed (count, hours, est cost) without touching iCloud/Gemini/Docs. |
| process | `--model` | `gemini-2.5-flash` | Override via flag or `GEMINI_MODEL` env. |
| youtube scan | `--channel-url` | `https://www.youtube.com/@EnterpriseVibeCode/streams` | Point at any channel's past-streams URL. |
| youtube process | `--limit` / `--allow-partial-monthly` / `--model` | same defaults | Uses auto-captions; Gemini does cleanup + summary only. |
| newsletter scan | `--feed-url` | EVC beehiiv feed | Fetches an RSS 2.0 feed; works with any beehiiv or similar publication. |
| newsletter process | `--limit` / `--allow-partial-monthly` / `--model` / `--feed-url` | same defaults | Strips HTML locally; Gemini does summary+tags only (cheapest source). |

## Pipeline detail (for the curious)

### Apple Photos → driving / vlog selfies

- **Scan.** osxphotos' built-in `--selfie` flag does **not** match videos (verified against a 4,572-movie library: 0 selfie-flagged movies). Kai pulls every movie via `osxphotos query --only-movies --json` and filters client-side on `original_height > original_width` (portrait) and `exif_info.duration ∈ [min, max]`. osxphotos emits Python's `Infinity`/`NaN` as bare tokens in its JSON; Kai sanitizes these before decoding.
- **Download.** Every candidate is typically `ismissing=true` (iCloud-only). Kai shells to `osxphotos export --uuid <U> --only-movies --download-missing --use-photokit --skip-original-if-edited tmp/downloads/<U>`. Files are stripped from the subdir hidden `.osxphotos_export.db` state.
- **Audio extraction.** iPhone `.MOV` files are often 1 GB+ at 15 Mbps — uploads succeed but Gemini's file processor returns `The file failed to be processed`. Kai uses `ffmpeg -vn -acodec copy` to extract the AAC audio track (about 1% the size, ~7 MB per 10 min), then uploads that. Transcription quality is the same for talking-head content.
- **Transcribe.** `google.golang.org/genai` File API upload → poll until `FileStateActive` → `GenerateContent` with `prompts.Transcription` (verbatim with `[MM:SS]` paragraph markers, `## Key Topics`, `## Open Questions`).
- **Summarize.** Second `GenerateContent` call with `prompts.Summary` returns `{summary, tags}` JSON.
- **File.** Append a timestamped entry to the monthly Google Doc for the video's `date`, update the stable-marker header, delete local audio + video.

### YouTube past livestreams

- `yt-dlp --skip-download --dump-json <channel>/streams` lists all past streams; Kai filters on `was_live=true && is_live=false`.
- `yt-dlp --skip-download --write-auto-subs --sub-lang en --sub-format vtt` fetches auto-captions per video. If captions aren't available yet (YouTube lag for just-ended streams), the entry fails with stage `captions_unavailable` and can be retried later.
- VTT is parsed locally, collapsing YouTube's rolling-caption duplication into plain text with `[HH:MM:SS]` anchors.
- Gemini call #1 (`prompts.CaptionClean`, text-only) structures into the same Markdown format as driving videos. Call #2 is the standard summary+tags.

### Newsletter (beehiiv RSS)

- HTTP GET the feed, parse RSS 2.0 via stdlib `encoding/xml`, read `content:encoded` full HTML.
- Local HTML→text walker using `golang.org/x/net/html` (no new deps — pulled in transitively by the Google API client).
- **One** Gemini call: `prompts.Summary` → `{summary, tags}`. No cleanup pass; newsletter prose is already clean.

### Monthly doc header

Each doc opens with a marker-delimited block:

```
<!-- kai:header:start -->
# Thoughts — January 2026
**Recordings:** 23  **Total Duration:** 51:57:11
<!-- kai:header:end -->
```

On every header update, Kai walks the Docs `Document.Body.Content` to find the markers' UTF-16 indices and `BatchUpdate { DeleteContentRange + InsertText }` within them. Entries below are untouched. Legacy docs (no markers) are upgraded on first contact.

### Idempotency

- `process_log.json` is keyed by source-specific ID (Apple UUID / YouTube video ID / newsletter URL). Re-running any `process` command skips IDs already present.
- `monthly_docs.json` tracks per-month doc ID + running entry count + total duration.
- `run_summary_<UTC>.json` is written at the end of every `process` run with success/failure breakdown, cost estimate, and failing stages.

### Concurrency gotcha

Running two `process` commands in parallel against the same state files races — the last writer clobbers the other's updates. The Google Docs themselves are safe (Docs API serializes BatchUpdates per doc), but `process_log.json` / `monthly_docs.json` can end up stale. **Run one at a time**, or use `cmd/rebuild-state` (below) to reconcile.

## Ops tools

| Command | Purpose |
|---|---|
| `go run ./cmd/preview` | Export cached Photos-library thumbnails for every scan candidate into `tmp/previews/` and open Finder, so you can visually sanity-check the filter before spending Gemini budget. |
| `go run ./cmd/inspect <docID>` | Dump a single Google Doc's body to stdout. Useful for duplicate-entry spot checks. |
| `go run ./cmd/rebuild-state` | Walk every doc in `Kai Transcripts`, re-parse entries, and rewrite `process_log.json` + `monthly_docs.json` from what's actually in Drive. Run this after a concurrent-run mistake or any time you suspect state drift. |
| `./kai tail-run` | `tail -f`-style follow of `tmp/kai.log` from another shell during long `process` runs. |

## NotebookLM

1. <https://notebooklm.google.com/> → new notebook.
2. Add sources → Google Drive → search `Thoughts —` → add each monthly doc.
3. **Syncing after new ingestion:** don't re-add — hover the existing source and click the refresh icon. Re-adding creates duplicate sources.
4. Ask questions. Log what's useful / misleading / flat in `VALIDATION_NOTES.md`.

## Files this tool writes

| File | Purpose | In .gitignore |
|---|---|---|
| `selfie_videos.csv` | Apple-Photos scan output; user-editable. | ✓ |
| `youtube_videos.csv` | YouTube-streams scan output; user-editable. | ✓ |
| `newsletter_posts.csv` | Newsletter scan output; user-editable. | ✓ |
| `process_log.json` | Per-item state for idempotent re-runs. | ✓ |
| `monthly_docs.json` | Monthly Doc ID registry. | ✓ |
| `run_summary_*.json` | Per-run audit trail. | ✓ |
| `google_token.json` | OAuth token cache. | ✓ |
| `gemini_api_key.txt` | Gemini key (alt to env var). | ✓ |
| `client_secrets.json` | Google OAuth desktop client. | ✓ |
| `tmp/downloads/…` | Per-video iCloud download scratch (auto-cleaned per item). | ✓ |
| `tmp/yt/…` | Per-video caption download scratch. | ✓ |
| `tmp/previews/…` | Preview thumbnails from `cmd/preview`. | ✓ |
| `tmp/kai.log` | Tee of long-running command output for `tail-run`. | ✓ |

## Gotchas worth knowing

- **osxphotos `--selfie` flag does not match videos.** Scan relies on portrait + duration heuristics; spot-check via `go run ./cmd/preview`.
- **iPhone videos are too large for Gemini video.** Kai's apple-photos path extracts audio via ffmpeg first. Audio is ~1% the size and loses no verbal content.
- **iCloud downloads go through your Photos library.** Turn on **Optimize Mac Storage** for large batches so macOS auto-evicts originals under disk pressure.
- **Gemini free tier caps at 20 requests/day.** You'll want paid tier to run anything meaningful. Billing-propagation sometimes takes a few minutes after linking.
- **Concurrent `process` runs race state files.** Run one at a time; use `cmd/rebuild-state` to recover.
- **NotebookLM re-adds do not deduplicate.** Use the per-source refresh icon to sync, not "Add from Drive" again.
- **Gemini output-token cap can truncate summary JSON.** Kai's summary parser salvages the partial summary text and proceeds with empty tags rather than failing the entry.
