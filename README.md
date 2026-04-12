# Kai — Phase 0 POC

Ingest driving selfie videos → transcribe with Gemini → batch into monthly Google Docs → feed into NotebookLM.

See `KAI_PLAN.md` for the full project vision across all phases.

## Prerequisites

```bash
brew install go pipx yt-dlp
pipx install osxphotos
```

## One-time setup

### 1. Gemini API key

Get a free key at <https://aistudio.google.com/apikey>:

```bash
printf '%s' '<YOUR_KEY>' > gemini_api_key.txt
chmod 600 gemini_api_key.txt
```

Or set `GEMINI_API_KEY` in your environment.

### 2. Google Cloud OAuth (for Drive + Docs)

1. <https://console.cloud.google.com/> → create project `kai-poc`.
2. **APIs & Services** → enable **Google Drive API** and **Google Docs API**.
3. **OAuth consent screen** → External → add your email as a test user.
   Scopes to request:
   - `https://www.googleapis.com/auth/drive.file`
   - `https://www.googleapis.com/auth/documents`
4. **Credentials** → **Create Credentials** → **OAuth client ID** → **Desktop app** → download the JSON.
5. Save it here as `client_secrets.json`.

On first `kai process` run a browser will open for consent. The resulting token is cached in `google_token.json`.

## Build

```bash
go build -o kai ./cmd/kai
```

## Usage

```bash
./kai scan                          # Apple Photos → selfie_videos.csv
./kai process --limit 10 --allow-partial-monthly
./kai youtube scan                  # YouTube past livestreams → youtube_videos.csv
./kai youtube process --limit 5 --allow-partial-monthly
./kai stats
```

Edit the CSVs between `scan` and `process`: flip `process` from `yes` to `no` for rows you want to skip. Both sources write to the same monthly Google Docs, tagged with `**Source:** apple-photos` or `**Source:** youtube`.

### Flags

| Command | Flag | Default | Notes |
|---|---|---|---|
| scan | `--min-duration` | `10m` | Videos shorter than this are dropped. |
| scan | `--max-duration` | `40m` | Videos longer than this are dropped. |
| scan | `--landscape-too` | off | By default only portrait-aspect videos are included. |
| process | `--limit` | `10` | Max videos this run. |
| process | `--allow-partial-monthly` | off | Generate a monthly overview even when a month is not fully processed yet. |
| process | `--model` | `gemini-2.5-flash` | Override via flag or `GEMINI_MODEL` env. |
| youtube scan | `--channel-url` | `https://www.youtube.com/@EnterpriseVibeCode/streams` | Point at any channel's past-streams URL. |
| youtube process | `--limit` / `--allow-partial-monthly` / `--model` | same defaults | Uses auto-captions; Gemini does cleanup + summary only. |

## NotebookLM

1. <https://notebooklm.google.com/> → new notebook.
2. Add sources → Google Drive → search `Thoughts —` → add each monthly doc.
3. Ask questions. Log what's useful / misleading / flat in `VALIDATION_NOTES.md`.

## Files this tool writes

| File | Purpose | In .gitignore |
|---|---|---|
| `selfie_videos.csv` | Apple-Photos scan output; user-editable. | ✓ |
| `youtube_videos.csv` | YouTube-streams scan output; user-editable. | ✓ |
| `process_log.json` | Per-video state for idempotent re-runs. | ✓ |
| `monthly_docs.json` | Monthly Doc ID registry. | ✓ |
| `run_summary_*.json` | Per-run audit trail. | ✓ |
| `google_token.json` | OAuth token cache. | ✓ |
| `tmp/downloads/…` | Per-video iCloud download scratch. | ✓ |
| `tmp/yt/…` | Per-video caption download scratch. | ✓ |

## Notes

- osxphotos' `--selfie` flag does not match videos. Scan uses duration + portrait aspect to find candidates; spot-check the CSV before processing.
- All videos are typically iCloud-only. Each `process` invocation downloads just-in-time via `osxphotos export --download-missing --use-photokit` and deletes the local file after Gemini upload.
- Gemini File API files auto-expire after 48 hours. Kai also deletes them explicitly after each transcription.
