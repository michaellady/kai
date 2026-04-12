#!/usr/bin/env python3
"""
ThoughtStream v3 — Monthly Batched Transcripts

Scans Apple Photos for selfie videos, transcribes with Gemini,
batches transcripts into monthly Google Docs for NotebookLM.

Each month becomes one Google Doc: "Thoughts — 2024-03"
containing all transcripts from that month, separated by headers.

Prerequisites:
  pip install google-genai google-api-python-client google-auth-oauthlib osxphotos

  1. Get a Gemini API key from https://aistudio.google.com/apikey
  2. Create Google Cloud OAuth credentials (Desktop app) with Drive + Docs scopes
  3. Download client_secrets.json to this directory

Usage:
  python thoughtstream_v3.py scan                  # Find selfie videos
  python thoughtstream_v3.py process               # Transcribe + batch into monthly docs
  python thoughtstream_v3.py process --limit 20    # Process 20 videos at a time
  python thoughtstream_v3.py stats                 # Show progress + topic landscape
"""

import argparse
import csv
import json
import os
import sys
import time
from collections import defaultdict
from datetime import datetime, timedelta
from pathlib import Path

# ---------------------------------------------------------------------------
# CONFIGURATION
# ---------------------------------------------------------------------------
CONFIG = {
    "min_duration_seconds": 30,
    "max_duration_seconds": 1200,

    "candidates_csv": "selfie_videos.csv",
    "process_log": "process_log.json",
    "monthly_docs_log": "monthly_docs.json",

    "drive_folder_name": "ThoughtStream Transcripts",
    "drive_video_folder_name": "ThoughtStream Videos",
    "upload_videos_to_drive": False,

    "gemini_model": "gemini-2.5-flash",

    "client_secrets_file": "client_secrets.json",
    "token_file": "google_token.json",

    "default_batch_size": 20,
    "delay_between_videos": 5,
}

TRANSCRIPTION_PROMPT = """Transcribe this video's audio verbatim. This is a personal voice recording
made while driving, spoken directly to a front-facing camera.

Rules:
- Transcribe every word spoken, preserving the speaker's natural language
- Remove only filler sounds (um, uh) but keep discourse markers (like, you know, so, right)
  when they carry conversational meaning
- Add paragraph breaks at natural topic shifts
- Add timestamps in [MM:SS] format at the start of each paragraph
- If the speaker references something visible, note it briefly in [brackets]
- Preserve emotional emphasis — if something is said emphatically, note [emphatic] or [excited]
- Do NOT summarize, rewrite, or clean up the language — this is a verbatim transcript
- At the end, add a section "## Key Topics" with a bullet list of main subjects discussed
- At the end, add a section "## Open Questions" listing any questions the speaker raised
  but did not resolve

Output the transcript as clean Markdown."""

SUMMARY_PROMPT = """Based on this transcript of a personal voice recording, generate:

1. A 2-3 sentence summary of what was discussed
2. A list of 3-8 topic tags (lowercase, hyphenated, e.g., "career-strategy", "bjj-coaching")

Return as JSON only, no markdown fences:
{"summary": "...", "tags": ["tag1", "tag2"]}"""

MONTHLY_SUMMARY_PROMPT = """You are given the complete set of personal voice recording transcripts
from one month. Generate a monthly overview:

1. A 3-5 sentence summary of the major themes and evolution of thinking across the month
2. A list of all unique topic tags mentioned, with counts
3. A list of recurring open questions that span multiple recordings
4. Any notable contradictions or shifts in thinking across the month

Return as Markdown with these sections:
## Monthly Overview
## Topic Frequency
## Recurring Questions
## Shifts in Thinking"""


# ---------------------------------------------------------------------------
# GOOGLE AUTH
# ---------------------------------------------------------------------------
def get_google_services():
    """Authenticate and return Drive + Docs API clients."""
    from google_auth_oauthlib.flow import InstalledAppFlow
    from google.oauth2.credentials import Credentials
    from google.auth.transport.requests import Request
    from googleapiclient.discovery import build

    scopes = [
        "https://www.googleapis.com/auth/drive.file",
        "https://www.googleapis.com/auth/documents",
    ]

    creds = None
    if os.path.exists(CONFIG["token_file"]):
        creds = Credentials.from_authorized_user_file(CONFIG["token_file"], scopes)

    if not creds or not creds.valid:
        if creds and creds.expired and creds.refresh_token:
            creds.refresh(Request())
        else:
            if not os.path.exists(CONFIG["client_secrets_file"]):
                print(f"ERROR: {CONFIG['client_secrets_file']} not found.")
                print()
                print("Setup:")
                print("  1. https://console.cloud.google.com/")
                print("  2. Enable Google Drive API + Google Docs API")
                print("  3. Credentials → OAuth Client ID → Desktop app")
                print(f"  4. Download JSON → save as '{CONFIG['client_secrets_file']}'")
                sys.exit(1)
            flow = InstalledAppFlow.from_client_secrets_file(
                CONFIG["client_secrets_file"], scopes
            )
            creds = flow.run_local_server(port=0)

        with open(CONFIG["token_file"], "w") as f:
            f.write(creds.to_json())

    drive = build("drive", "v3", credentials=creds)
    docs = build("docs", "v1", credentials=creds)
    return drive, docs


def get_or_create_folder(drive, folder_name, parent_id=None):
    """Find or create a Drive folder. Returns folder ID."""
    query = (
        f"name = '{folder_name}' and mimeType = 'application/vnd.google-apps.folder'"
        f" and trashed = false"
    )
    if parent_id:
        query += f" and '{parent_id}' in parents"

    results = drive.files().list(q=query, spaces="drive", fields="files(id)").execute()
    files = results.get("files", [])
    if files:
        return files[0]["id"]

    metadata = {
        "name": folder_name,
        "mimeType": "application/vnd.google-apps.folder",
    }
    if parent_id:
        metadata["parents"] = [parent_id]
    folder = drive.files().create(body=metadata, fields="id").execute()
    print(f"  Created Drive folder: {folder_name}")
    return folder["id"]


# ---------------------------------------------------------------------------
# GEMINI
# ---------------------------------------------------------------------------
def get_gemini_client():
    """Initialize Gemini client."""
    from google import genai

    api_key = os.environ.get("GEMINI_API_KEY")
    if not api_key:
        key_file = Path("gemini_api_key.txt")
        if key_file.exists():
            api_key = key_file.read_text().strip()
        else:
            print("ERROR: GEMINI_API_KEY not set.")
            print("  export GEMINI_API_KEY='your-key'")
            print("  OR save to gemini_api_key.txt")
            print("  Get a free key: https://aistudio.google.com/apikey")
            sys.exit(1)

    return genai.Client(api_key=api_key)


def transcribe_video(gemini_client, video_path, display_name):
    """Upload video to Gemini File API, transcribe, return transcript + summary."""
    from google.genai import types

    file_size_mb = os.path.getsize(video_path) / (1024 * 1024)
    print(f"    Uploading to Gemini ({file_size_mb:.1f} MB)...", end="", flush=True)

    uploaded_file = gemini_client.files.upload(
        file=video_path,
        config={"display_name": display_name},
    )

    while uploaded_file.state.name == "PROCESSING":
        print(".", end="", flush=True)
        time.sleep(3)
        uploaded_file = gemini_client.files.get(name=uploaded_file.name)

    if uploaded_file.state.name == "FAILED":
        raise RuntimeError(f"Gemini file processing failed: {uploaded_file.state}")
    print(" done")

    # Transcribe
    print("    Transcribing...", end="", flush=True)
    transcript_response = gemini_client.models.generate_content(
        model=CONFIG["gemini_model"],
        contents=[uploaded_file, TRANSCRIPTION_PROMPT],
    )
    transcript = transcript_response.text
    print(" done")

    # Summary + tags
    print("    Extracting topics...", end="", flush=True)
    summary_response = gemini_client.models.generate_content(
        model=CONFIG["gemini_model"],
        contents=[f"Here is a transcript:\n\n{transcript}\n\n{SUMMARY_PROMPT}"],
    )

    summary_data = {"summary": "", "tags": []}
    try:
        raw = summary_response.text.strip()
        if raw.startswith("```"):
            raw = raw.split("\n", 1)[1].rsplit("```", 1)[0]
        summary_data = json.loads(raw)
    except (json.JSONDecodeError, IndexError):
        summary_data["summary"] = summary_response.text[:200]
    print(" done")

    try:
        gemini_client.files.delete(name=uploaded_file.name)
    except Exception:
        pass

    return transcript, summary_data


# ---------------------------------------------------------------------------
# MONTHLY DOC MANAGEMENT
# ---------------------------------------------------------------------------
def get_month_key(date_str):
    """Extract 'YYYY-MM' from an ISO date string."""
    if not date_str or date_str == "unknown":
        return "unknown"
    return date_str[:7]


def get_month_display(month_key):
    """Convert '2024-03' to 'March 2024'."""
    if month_key == "unknown":
        return "Unknown Date"
    try:
        dt = datetime.strptime(month_key, "%Y-%m")
        return dt.strftime("%B %Y")
    except ValueError:
        return month_key


def load_monthly_docs_log():
    """Load the registry of monthly Google Docs."""
    if os.path.exists(CONFIG["monthly_docs_log"]):
        with open(CONFIG["monthly_docs_log"]) as f:
            return json.load(f)
    return {}


def save_monthly_docs_log(log):
    """Save the monthly docs registry."""
    with open(CONFIG["monthly_docs_log"], "w") as f:
        json.dump(log, f, indent=2)


def format_entry_block(video, transcript, summary_data):
    """Format a single transcript entry for insertion into the monthly doc."""
    date_str = video["date"][:10] if video["date"] != "unknown" else "unknown"
    time_str = video["date"][11:16] if len(video["date"]) > 11 else ""
    tags = ", ".join(summary_data.get("tags", []))

    block = f"""

---

### {date_str} {time_str} — {video['duration_human']}

**Tags:** {tags}
**Summary:** {summary_data.get('summary', 'N/A')}

{transcript}

"""
    return block


def build_monthly_header(month_key, entry_count, total_duration_sec):
    """Build the header for a monthly doc."""
    display = get_month_display(month_key)
    duration = str(timedelta(seconds=int(total_duration_sec)))

    return f"""# Thoughts — {display}

**Recordings:** {entry_count}
**Total Duration:** {duration}
**Generated by:** ThoughtStream

This document contains verbatim transcripts of personal voice recordings
from {display}, ordered chronologically. Each entry includes timestamps,
topic tags, and a brief summary.

"""


def create_monthly_doc(drive, docs, folder_id, month_key, header_text):
    """Create a new monthly Google Doc and return its ID + URL."""
    display = get_month_display(month_key)
    doc_title = f"Thoughts — {display}"

    doc_metadata = {
        "name": doc_title,
        "mimeType": "application/vnd.google-apps.document",
        "parents": [folder_id],
    }
    doc_file = drive.files().create(body=doc_metadata, fields="id").execute()
    doc_id = doc_file["id"]

    # Insert header
    docs.documents().batchUpdate(
        documentId=doc_id,
        body={"requests": [{"insertText": {"location": {"index": 1}, "text": header_text}}]},
    ).execute()

    doc_url = f"https://docs.google.com/document/d/{doc_id}/edit"
    return doc_id, doc_url


def append_to_monthly_doc(docs, doc_id, text_to_append):
    """Append a transcript entry to an existing monthly doc."""
    # Get current doc length
    doc = docs.documents().get(documentId=doc_id).execute()
    body = doc.get("body", {})
    content = body.get("content", [])

    # Find the end index (last element's endIndex - 1)
    end_index = 1
    if content:
        end_index = content[-1].get("endIndex", 1) - 1

    # Insert at end
    docs.documents().batchUpdate(
        documentId=doc_id,
        body={"requests": [{"insertText": {"location": {"index": end_index}, "text": text_to_append}}]},
    ).execute()


def update_monthly_header(docs, doc_id, month_key, entry_count, total_duration_sec):
    """Replace the header block (first few lines) with updated counts."""
    display = get_month_display(month_key)
    duration = str(timedelta(seconds=int(total_duration_sec)))

    # Read current doc to find where header ends (first ---)
    doc = docs.documents().get(documentId=doc_id).execute()
    full_text = ""
    for element in doc.get("body", {}).get("content", []):
        for para in element.get("paragraph", {}).get("elements", []):
            run = para.get("textRun", {})
            full_text += run.get("content", "")

    # Find the first "---" separator
    separator_pos = full_text.find("\n---\n")
    if separator_pos == -1:
        separator_pos = full_text.find("\n---")
    if separator_pos == -1:
        return  # Can't find separator, skip header update

    new_header = build_monthly_header(month_key, entry_count, total_duration_sec)

    # Replace from index 1 to separator_pos + 1
    requests_body = [
        {"deleteContentRange": {
            "range": {"startIndex": 1, "endIndex": separator_pos + 1}
        }},
        {"insertText": {
            "location": {"index": 1},
            "text": new_header,
        }},
    ]

    try:
        docs.documents().batchUpdate(
            documentId=doc_id,
            body={"requests": requests_body},
        ).execute()
    except Exception:
        pass  # Non-critical, header just won't have updated counts


# ---------------------------------------------------------------------------
# SCAN
# ---------------------------------------------------------------------------
def scan_photos():
    """Use osxphotos to find front-facing camera videos."""
    try:
        import osxphotos
    except ImportError:
        print("ERROR: osxphotos not installed. Run: pip install osxphotos")
        print("NOTE: osxphotos only works on macOS.")
        sys.exit(1)

    print("Opening Photos library...")
    photosdb = osxphotos.PhotosDB()

    all_videos = photosdb.photos(media_type=["video"])
    print(f"Found {len(all_videos)} total videos.")

    candidates = []
    skipped = {"not_selfie": 0, "too_short": 0, "too_long": 0, "no_path": 0}

    for photo in all_videos:
        if not photo.selfie:
            skipped["not_selfie"] += 1
            continue
        duration = photo.duration or 0
        if duration < CONFIG["min_duration_seconds"]:
            skipped["too_short"] += 1
            continue
        if duration > CONFIG["max_duration_seconds"]:
            skipped["too_long"] += 1
            continue
        path = photo.path or photo.path_edited
        if not path or not os.path.exists(path):
            skipped["no_path"] += 1
            continue

        candidates.append({
            "uuid": photo.uuid,
            "date": photo.date.isoformat() if photo.date else "unknown",
            "duration_sec": round(duration, 1),
            "duration_human": str(timedelta(seconds=int(duration))),
            "filename": photo.original_filename,
            "path": path,
            "title": f"Thought — {photo.date.strftime('%Y-%m-%d %H:%M') if photo.date else 'unknown'}",
            "has_location": bool(photo.location),
            "latitude": photo.location[0] if photo.location else None,
            "longitude": photo.location[1] if photo.location else None,
            "process": "yes",
        })

    candidates.sort(key=lambda x: x["date"])

    if candidates:
        fieldnames = [
            "process", "date", "duration_human", "duration_sec",
            "title", "filename", "uuid", "path",
            "has_location", "latitude", "longitude",
        ]
        with open(CONFIG["candidates_csv"], "w", newline="") as f:
            writer = csv.DictWriter(f, fieldnames=fieldnames)
            writer.writeheader()
            writer.writerows(candidates)

    # Monthly breakdown
    monthly = defaultdict(list)
    for c in candidates:
        mk = get_month_key(c["date"])
        monthly[mk].append(c)

    total_duration = sum(c["duration_sec"] for c in candidates)
    hours = total_duration / 3600

    print(f"\n{'=' * 60}")
    print(f"SCAN RESULTS")
    print(f"{'=' * 60}")
    print(f"Selfie videos found:      {len(candidates)}")
    print(f"Total duration:           {timedelta(seconds=int(total_duration))} ({hours:.1f} hrs)")
    print(f"Est. Gemini cost:         ${hours * 0.25:.2f}")
    print(f"")
    print(f"Filtered out:")
    print(f"  Not selfie:             {skipped['not_selfie']}")
    print(f"  Too short (<{CONFIG['min_duration_seconds']}s):       {skipped['too_short']}")
    print(f"  Too long (>{CONFIG['max_duration_seconds']}s):     {skipped['too_long']}")
    print(f"  File not accessible:    {skipped['no_path']}")
    print(f"")

    if candidates:
        print(f"Monthly breakdown:")
        for mk in sorted(monthly.keys()):
            vids = monthly[mk]
            dur = sum(float(v["duration_sec"]) for v in vids)
            print(f"  {get_month_display(mk):20s}  {len(vids):3d} videos  {timedelta(seconds=int(dur))}")
        print(f"")
        print(f"Each month → one Google Doc in NotebookLM")
        print(f"Total docs needed: {len(monthly)} (well within 300-source limit)")
        print(f"")
        print(f"Saved to: {CONFIG['candidates_csv']}")
        print(f"Next: review CSV, then run: python {sys.argv[0]} process")


# ---------------------------------------------------------------------------
# PROCESS — Transcribe + batch into monthly docs
# ---------------------------------------------------------------------------
def process_videos(limit):
    """Transcribe videos and batch into monthly Google Docs."""
    if not os.path.exists(CONFIG["candidates_csv"]):
        print(f"ERROR: Run 'scan' first.")
        sys.exit(1)

    # Load state
    process_log = {}
    if os.path.exists(CONFIG["process_log"]):
        with open(CONFIG["process_log"]) as f:
            process_log = json.load(f)

    monthly_docs = load_monthly_docs_log()

    with open(CONFIG["candidates_csv"]) as f:
        candidates = list(csv.DictReader(f))

    to_process = [
        c for c in candidates
        if c["process"].strip().lower() == "yes"
        and c["uuid"] not in process_log
    ]

    if not to_process:
        already = len([c for c in candidates if c["uuid"] in process_log])
        print(f"Nothing to process. Already done: {already}")
        return

    batch = to_process[:limit]
    remaining = len(to_process) - len(batch)

    # Group batch by month for efficient doc updates
    batch_by_month = defaultdict(list)
    for v in batch:
        batch_by_month[get_month_key(v["date"])].append(v)

    print(f"{'=' * 60}")
    print(f"THOUGHTSTREAM PROCESSOR (Monthly Batching)")
    print(f"{'=' * 60}")
    print(f"Videos this run:    {len(batch)}")
    print(f"Months touched:     {len(batch_by_month)} ({', '.join(sorted(batch_by_month.keys()))})")
    print(f"Remaining after:    {remaining}")
    print()

    # Initialize clients
    print("Authenticating...")
    drive, docs_service = get_google_services()
    gemini = get_gemini_client()
    folder_id = get_or_create_folder(drive, CONFIG["drive_folder_name"])
    print(f"  Drive folder: {CONFIG['drive_folder_name']}")
    print()

    success_count = 0
    error_count = 0
    total = len(batch)
    idx = 0

    # Process in chronological order
    for video in sorted(batch, key=lambda v: v["date"]):
        idx += 1
        month_key = get_month_key(video["date"])
        title = video["title"]
        date_str = video["date"][:10] if video["date"] != "unknown" else "unknown"

        print(f"[{idx}/{total}] {title}")
        print(f"    File: {video['filename']} ({video['duration_human']})")
        print(f"    Month: {get_month_display(month_key)}")

        filepath = video["path"]
        if not os.path.exists(filepath):
            print(f"    ERROR: File not found, skipping")
            error_count += 1
            continue

        try:
            # Step 1: Transcribe
            transcript, summary_data = transcribe_video(
                gemini, filepath, video["filename"]
            )

            # Step 2: Format the entry
            entry_block = format_entry_block(video, transcript, summary_data)

            # Step 3: Get or create the monthly doc
            if month_key in monthly_docs and monthly_docs[month_key].get("doc_id"):
                doc_id = monthly_docs[month_key]["doc_id"]
                doc_url = monthly_docs[month_key]["doc_url"]
                print(f"    Appending to existing doc...", end="", flush=True)
                append_to_monthly_doc(docs_service, doc_id, entry_block)

                # Update counts in the log
                monthly_docs[month_key]["entry_count"] += 1
                monthly_docs[month_key]["total_duration_sec"] += float(video["duration_sec"])
                monthly_docs[month_key]["videos"].append(video["uuid"])
                print(" done")
            else:
                # Create new monthly doc
                print(f"    Creating new monthly doc...", end="", flush=True)
                header = build_monthly_header(month_key, 1, float(video["duration_sec"]))
                doc_id, doc_url = create_monthly_doc(
                    drive, docs_service, folder_id, month_key, header
                )
                # Append the first entry
                append_to_monthly_doc(docs_service, doc_id, entry_block)

                monthly_docs[month_key] = {
                    "doc_id": doc_id,
                    "doc_url": doc_url,
                    "month_display": get_month_display(month_key),
                    "entry_count": 1,
                    "total_duration_sec": float(video["duration_sec"]),
                    "videos": [video["uuid"]],
                    "created_at": datetime.now().isoformat(),
                }
                print(" done")

            # Step 4 (optional): Upload video to Drive
            drive_video_id = None
            if CONFIG["upload_videos_to_drive"]:
                from googleapiclient.http import MediaFileUpload
                video_folder_id = get_or_create_folder(
                    drive, CONFIG["drive_video_folder_name"]
                )
                print("    Uploading video to Drive...", end="", flush=True)
                media = MediaFileUpload(filepath, mimetype="video/mp4", resumable=True)
                file_meta = {
                    "name": f"{date_str} - {video['filename']}",
                    "parents": [video_folder_id],
                }
                request = drive.files().create(
                    body=file_meta, media_body=media, fields="id"
                )
                response = None
                while response is None:
                    status, response = request.next_chunk()
                    if status:
                        pct = int(status.progress() * 100)
                        print(f"\r    Uploading video to Drive... {pct}%", end="", flush=True)
                drive_video_id = response["id"]
                print(" done")

            # Log per-video processing
            tags = summary_data.get("tags", [])
            process_log[video["uuid"]] = {
                "month_key": month_key,
                "doc_id": monthly_docs[month_key]["doc_id"],
                "doc_url": monthly_docs[month_key]["doc_url"],
                "drive_video_id": drive_video_id,
                "title": title,
                "date": date_str,
                "duration": video["duration_human"],
                "tags": tags,
                "summary": summary_data.get("summary", ""),
                "processed_at": datetime.now().isoformat(),
            }

            # Save state after each (crash-safe)
            with open(CONFIG["process_log"], "w") as f:
                json.dump(process_log, f, indent=2)
            save_monthly_docs_log(monthly_docs)

            print(f"    ✓ Doc: {doc_url}")
            print(f"    ✓ Tags: {', '.join(tags)}")
            success_count += 1

        except Exception as e:
            print(f"    ERROR: {e}")
            import traceback
            traceback.print_exc()
            error_count += 1

        if idx < total:
            time.sleep(CONFIG["delay_between_videos"])

    # Generate monthly summaries for newly completed months
    # (A month is "complete" if all its candidates are processed)
    print()
    generate_monthly_summaries(gemini, docs_service, monthly_docs, process_log, candidates)

    # Final summary
    print(f"\n{'=' * 60}")
    print(f"PROCESSING COMPLETE")
    print(f"{'=' * 60}")
    print(f"Succeeded:          {success_count}")
    print(f"Errors:             {error_count}")
    print(f"Total processed:    {len(process_log)}")
    print(f"Monthly docs:       {len(monthly_docs)}")
    if remaining > 0:
        print(f"Remaining:          {remaining}")
    print()
    print(f"Your transcripts: Google Drive → '{CONFIG['drive_folder_name']}'")
    print()
    print(f"NOTEBOOKLM SETUP:")
    print(f"  1. https://notebooklm.google.com → New notebook")
    print(f"  2. Add sources → Google Drive → '{CONFIG['drive_folder_name']}'")
    print(f"  3. Select monthly docs ({len(monthly_docs)} docs)")
    print(f"  4. Start chatting with your past thinking")


def generate_monthly_summaries(gemini, docs_service, monthly_docs, process_log, candidates):
    """For each month where all videos are processed, generate a monthly overview."""
    # Group candidates by month
    candidates_by_month = defaultdict(list)
    for c in candidates:
        if c["process"].strip().lower() == "yes":
            candidates_by_month[get_month_key(c["date"])].append(c)

    for month_key, month_candidates in candidates_by_month.items():
        if month_key not in monthly_docs:
            continue

        # Check if all videos for this month are processed
        all_done = all(c["uuid"] in process_log for c in month_candidates)
        already_summarized = monthly_docs[month_key].get("monthly_summary_done", False)

        if all_done and not already_summarized:
            print(f"  Generating monthly overview for {get_month_display(month_key)}...",
                  end="", flush=True)

            # Collect all summaries for this month
            month_summaries = []
            for c in sorted(month_candidates, key=lambda x: x["date"]):
                entry = process_log.get(c["uuid"], {})
                if entry.get("summary"):
                    month_summaries.append(
                        f"- {entry['date']}: {entry['summary']} "
                        f"[Tags: {', '.join(entry.get('tags', []))}]"
                    )

            if month_summaries:
                try:
                    overview_response = gemini.models.generate_content(
                        model=CONFIG["gemini_model"],
                        contents=[
                            f"Here are summaries of voice recordings from "
                            f"{get_month_display(month_key)}:\n\n"
                            + "\n".join(month_summaries)
                            + f"\n\n{MONTHLY_SUMMARY_PROMPT}"
                        ],
                    )
                    overview_text = (
                        f"\n\n{'=' * 60}\n\n"
                        f"# Monthly Overview — {get_month_display(month_key)}\n\n"
                        f"{overview_response.text}\n"
                    )

                    # Append to the monthly doc
                    doc_id = monthly_docs[month_key]["doc_id"]
                    append_to_monthly_doc(docs_service, doc_id, overview_text)

                    monthly_docs[month_key]["monthly_summary_done"] = True
                    save_monthly_docs_log(monthly_docs)
                    print(" done")
                except Exception as e:
                    print(f" error: {e}")


# ---------------------------------------------------------------------------
# STATS
# ---------------------------------------------------------------------------
def show_stats():
    """Progress and topic landscape."""
    print(f"{'=' * 60}")
    print(f"THOUGHTSTREAM STATUS")
    print(f"{'=' * 60}")

    if not os.path.exists(CONFIG["candidates_csv"]):
        print(f"\nNo scan yet. Run: python {sys.argv[0]} scan")
        return

    with open(CONFIG["candidates_csv"]) as f:
        candidates = list(csv.DictReader(f))

    yes = len([c for c in candidates if c["process"].strip().lower() == "yes"])
    total_dur = sum(float(c["duration_sec"]) for c in candidates)
    hours = total_dur / 3600
    print(f"\nScanned: {len(candidates)} selfie videos ({hours:.1f} hrs)")
    print(f"Marked for processing: {yes}")

    process_log = {}
    if os.path.exists(CONFIG["process_log"]):
        with open(CONFIG["process_log"]) as f:
            process_log = json.load(f)

    monthly_docs = load_monthly_docs_log()

    print(f"Processed: {len(process_log)}")
    print(f"Monthly docs created: {len(monthly_docs)}")

    if monthly_docs:
        print(f"\nMonthly docs:")
        for mk in sorted(monthly_docs.keys()):
            info = monthly_docs[mk]
            count = info.get("entry_count", 0)
            dur = timedelta(seconds=int(info.get("total_duration_sec", 0)))
            summarized = "✓" if info.get("monthly_summary_done") else " "
            print(f"  {info.get('month_display', mk):20s}  {count:3d} entries  {dur}  [{summarized}] summary")
        print(f"\n  [✓] = monthly overview generated")

    if process_log:
        # Tag frequency
        all_tags = defaultdict(int)
        for entry in process_log.values():
            for tag in entry.get("tags", []):
                all_tags[tag] += 1

        if all_tags:
            sorted_tags = sorted(all_tags.items(), key=lambda x: -x[1])[:20]
            print(f"\nTop topics across all recordings:")
            max_count = sorted_tags[0][1] if sorted_tags else 1
            for tag, count in sorted_tags:
                bar_len = int((count / max_count) * 30)
                bar = "█" * bar_len
                print(f"  {tag:30s} {bar} ({count})")

        # Remaining
        remaining = [
            c for c in candidates
            if c["process"].strip().lower() == "yes"
            and c["uuid"] not in process_log
        ]
        if remaining:
            remaining_dur = sum(float(r["duration_sec"]) for r in remaining)
            print(f"\nRemaining: {len(remaining)} videos ({remaining_dur / 3600:.1f} hrs)")
            print(f"Est. Gemini cost: ${remaining_dur / 3600 * 0.25:.2f}")

    print()
    print(f"Drive folder: '{CONFIG['drive_folder_name']}'")


# ---------------------------------------------------------------------------
# MAIN
# ---------------------------------------------------------------------------
def main():
    parser = argparse.ArgumentParser(
        description="ThoughtStream v3 — Selfie videos → Gemini → Monthly Google Docs → NotebookLM",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""
Workflow:
  1. scan         → Find selfie videos in Apple Photos
  2. (review CSV) → Mark any to skip
  3. process      → Transcribe → batch into monthly Google Docs
  4. stats        → Topic landscape + progress
  5. NotebookLM   → Add monthly docs from Drive folder
        """,
    )
    parser.add_argument("command", choices=["scan", "process", "stats"])
    parser.add_argument("--limit", type=int, default=CONFIG["default_batch_size"],
                        help=f"Max videos per run (default: {CONFIG['default_batch_size']})")
    parser.add_argument("--min-duration", type=int, default=CONFIG["min_duration_seconds"])
    parser.add_argument("--max-duration", type=int, default=CONFIG["max_duration_seconds"])
    parser.add_argument("--archive-videos", action="store_true",
                        help="Also upload source videos to Google Drive")

    args = parser.parse_args()
    CONFIG["min_duration_seconds"] = args.min_duration
    CONFIG["max_duration_seconds"] = args.max_duration
    CONFIG["upload_videos_to_drive"] = args.archive_videos

    if args.command == "scan":
        scan_photos()
    elif args.command == "process":
        process_videos(args.limit)
    elif args.command == "stats":
        show_stats()


if __name__ == "__main__":
    main()
