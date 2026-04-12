// Package prompts holds the Gemini system/user prompts for Kai.
// Text is ported verbatim from thoughtstream_v3.py so quality comparisons
// between the Python reference and this port are apples-to-apples.
package prompts

const Transcription = `Transcribe this video's audio verbatim. This is a personal voice recording
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

Output the transcript as clean Markdown.`

const Summary = `Based on this transcript of a personal voice recording, generate:

1. A 2-3 sentence summary of what was discussed
2. A list of 3-8 topic tags (lowercase, hyphenated, e.g., "career-strategy", "bjj-coaching")

Return as JSON only, no markdown fences:
{"summary": "...", "tags": ["tag1", "tag2"]}`

const CaptionClean = `You are given raw auto-generated captions from a recorded livestream. The speaker talks through the stream extemporaneously; multiple topics are covered in one session. Transform this into a clean, readable transcript that matches the format of our driving-video transcripts.

Rules:
- Preserve the speaker's actual words; do NOT paraphrase or compress.
- Add paragraph breaks at natural topic shifts.
- Keep the existing [HH:MM:SS] markers where present and add more at topic-shift paragraph breaks (use the nearest timestamp from the raw input).
- Remove filler (um, uh) but keep discourse markers (like, you know, so, right) when they carry meaning.
- At the end, add a "## Key Topics" section with a bullet list of main subjects discussed.
- At the end, add a "## Open Questions" section listing any questions the speaker raised but did not resolve.

Output as clean Markdown.`

const MonthlySummary = `You are given the complete set of personal voice recording transcripts
from one month. Generate a monthly overview:

1. A 3-5 sentence summary of the major themes and evolution of thinking across the month
2. A list of all unique topic tags mentioned, with counts
3. A list of recurring open questions that span multiple recordings
4. Any notable contradictions or shifts in thinking across the month

Return as Markdown with these sections:
## Monthly Overview
## Topic Frequency
## Recurring Questions
## Shifts in Thinking`
