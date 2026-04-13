// Package gemini wraps the google.golang.org/genai SDK for Kai's three
// generation calls: per-video transcription, per-video summary+tags,
// and per-month overview.
package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/mikelady/kai/internal/prompts"
	"github.com/mikelady/kai/internal/retry"
	"google.golang.org/genai"
)

type Client struct {
	inner *genai.Client
	model string
}

func NewClient(ctx context.Context, apiKey, model string) (*Client, error) {
	c, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, err
	}
	if model == "" {
		model = "gemini-2.5-flash"
	}
	return &Client{inner: c, model: model}, nil
}

// TranscribeResult is the composite output of transcribing + summarizing one video.
type TranscribeResult struct {
	Transcript  string
	Summary     string
	Tags        []string
	UploadedURI string
}

// Transcribe uploads path to Gemini File API, waits for ACTIVE state,
// then runs two generations: verbatim transcript and summary+tags JSON.
// The uploaded file is deleted before return regardless of outcome.
func (c *Client) Transcribe(ctx context.Context, path string) (*TranscribeResult, error) {
	mime := "video/mp4"
	switch strings.ToLower(filepathExt(path)) {
	case ".mov":
		mime = "video/quicktime"
	case ".m4a", ".mp4a":
		mime = "audio/mp4"
	case ".mp3":
		mime = "audio/mpeg"
	case ".aac":
		mime = "audio/aac"
	case ".wav":
		mime = "audio/wav"
	}
	file, err := retry.Do(ctx, func(ctx context.Context) (*genai.File, error) {
		return c.inner.Files.UploadFromPath(ctx, path, &genai.UploadFileConfig{MIMEType: mime})
	})
	if err != nil {
		return nil, fmt.Errorf("upload: %w", err)
	}
	// Best-effort delete of the remote file when we're done.
	defer func() {
		if file == nil || file.Name == "" {
			return
		}
		delCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = c.inner.Files.Delete(delCtx, file.Name, nil)
	}()

	// Poll until the file becomes ACTIVE.
	active, err := c.waitActive(ctx, file.Name)
	if err != nil {
		return nil, fmt.Errorf("wait active: %w", err)
	}

	// Transcription.
	transcriptResp, err := retry.Do(ctx, func(ctx context.Context) (*genai.GenerateContentResponse, error) {
		parts := []*genai.Part{
			{Text: prompts.Transcription},
			{FileData: &genai.FileData{FileURI: active.URI, MIMEType: active.MIMEType}},
		}
		contents := []*genai.Content{{Parts: parts, Role: genai.RoleUser}}
		return c.inner.Models.GenerateContent(ctx, c.model, contents, nil)
	})
	if err != nil {
		return nil, fmt.Errorf("transcribe generate: %w", err)
	}
	transcript := transcriptResp.Text()

	// Summary + tags.
	summaryResp, err := retry.Do(ctx, func(ctx context.Context) (*genai.GenerateContentResponse, error) {
		parts := []*genai.Part{
			{Text: prompts.Summary + "\n\nTranscript:\n" + transcript},
		}
		contents := []*genai.Content{{Parts: parts, Role: genai.RoleUser}}
		return c.inner.Models.GenerateContent(ctx, c.model, contents, nil)
	})
	if err != nil {
		return nil, fmt.Errorf("summary generate: %w", err)
	}
	summary, tags, err := parseSummaryJSON(summaryResp.Text())
	if err != nil {
		return nil, fmt.Errorf("summary parse: %w", err)
	}

	return &TranscribeResult{
		Transcript:  transcript,
		Summary:     summary,
		Tags:        tags,
		UploadedURI: active.URI,
	}, nil
}

// CleanCaption takes raw auto-caption text and produces a clean Markdown
// transcript plus summary + tags — the same shape as Transcribe, but without
// uploading any media. Used by the YouTube ingestion path.
func (c *Client) CleanCaption(ctx context.Context, raw string) (*TranscribeResult, error) {
	transcriptResp, err := retry.Do(ctx, func(ctx context.Context) (*genai.GenerateContentResponse, error) {
		parts := []*genai.Part{
			{Text: prompts.CaptionClean + "\n\nRaw captions:\n" + raw},
		}
		contents := []*genai.Content{{Parts: parts, Role: genai.RoleUser}}
		return c.inner.Models.GenerateContent(ctx, c.model, contents, nil)
	})
	if err != nil {
		return nil, fmt.Errorf("caption clean generate: %w", err)
	}
	transcript := transcriptResp.Text()

	summaryResp, err := retry.Do(ctx, func(ctx context.Context) (*genai.GenerateContentResponse, error) {
		parts := []*genai.Part{
			{Text: prompts.Summary + "\n\nTranscript:\n" + transcript},
		}
		contents := []*genai.Content{{Parts: parts, Role: genai.RoleUser}}
		return c.inner.Models.GenerateContent(ctx, c.model, contents, nil)
	})
	if err != nil {
		return nil, fmt.Errorf("summary generate: %w", err)
	}
	summary, tags, err := parseSummaryJSON(summaryResp.Text())
	if err != nil {
		return nil, fmt.Errorf("summary parse: %w", err)
	}
	return &TranscribeResult{Transcript: transcript, Summary: summary, Tags: tags}, nil
}

// Summarize runs just the Summary+Tags prompt on already-clean plain text
// (e.g. newsletter posts). Returns a TranscribeResult whose Transcript is
// the input unchanged, so callers can funnel newsletter content through
// the same monthly-doc append path as livestreams and driving videos.
func (c *Client) Summarize(ctx context.Context, plainText string) (*TranscribeResult, error) {
	resp, err := retry.Do(ctx, func(ctx context.Context) (*genai.GenerateContentResponse, error) {
		parts := []*genai.Part{
			{Text: prompts.Summary + "\n\nTranscript:\n" + plainText},
		}
		contents := []*genai.Content{{Parts: parts, Role: genai.RoleUser}}
		return c.inner.Models.GenerateContent(ctx, c.model, contents, nil)
	})
	if err != nil {
		return nil, fmt.Errorf("summary generate: %w", err)
	}
	summary, tags, err := parseSummaryJSON(resp.Text())
	if err != nil {
		return nil, fmt.Errorf("summary parse: %w", err)
	}
	return &TranscribeResult{Transcript: plainText, Summary: summary, Tags: tags}, nil
}

// MonthlyOverview asks Gemini to synthesize a monthly overview from all of
// the month's per-entry summaries + tags + transcripts. For partial months,
// the caller prepends a disclaimer line outside this call.
func (c *Client) MonthlyOverview(ctx context.Context, docBody string) (string, error) {
	resp, err := retry.Do(ctx, func(ctx context.Context) (*genai.GenerateContentResponse, error) {
		parts := []*genai.Part{
			{Text: prompts.MonthlySummary + "\n\nEntries:\n" + docBody},
		}
		contents := []*genai.Content{{Parts: parts, Role: genai.RoleUser}}
		return c.inner.Models.GenerateContent(ctx, c.model, contents, nil)
	})
	if err != nil {
		return "", err
	}
	return resp.Text(), nil
}

func (c *Client) waitActive(ctx context.Context, name string) (*genai.File, error) {
	deadline := time.Now().Add(10 * time.Minute)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("file %s did not become ACTIVE within 10m", name)
		}
		f, err := c.inner.Files.Get(ctx, name, nil)
		if err != nil {
			return nil, err
		}
		switch f.State {
		case genai.FileStateActive:
			return f, nil
		case genai.FileStateFailed:
			if f.Error != nil {
				return nil, fmt.Errorf("file processing failed: %v", f.Error)
			}
			return nil, fmt.Errorf("file processing failed")
		default:
			select {
			case <-time.After(3 * time.Second):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
}

// parseSummaryJSON is tolerant to the model wrapping JSON in markdown fences
// even though the prompt forbids it.
var fenceRE = regexp.MustCompile("(?s)```(?:json)?\\s*(.*?)\\s*```")

var summaryStrRE = regexp.MustCompile(`(?s)"summary"\s*:\s*"((?:\\.|[^"\\])*)`)

func parseSummaryJSON(raw string) (string, []string, error) {
	s := strings.TrimSpace(raw)
	if m := fenceRE.FindStringSubmatch(s); m != nil {
		s = strings.TrimSpace(m[1])
	}
	var payload struct {
		Summary string   `json:"summary"`
		Tags    []string `json:"tags"`
	}
	if err := json.Unmarshal([]byte(s), &payload); err == nil {
		return payload.Summary, payload.Tags, nil
	}
	// Salvage path: Gemini occasionally runs over the output token cap and
	// returns a truncated {"summary": "...", "tags": …} JSON object. Rather
	// than fail the entry, extract the partial summary string via regex and
	// accept empty tags.
	if m := summaryStrRE.FindStringSubmatch(s); m != nil {
		raw := m[1]
		// Unescape common JSON escapes.
		raw = strings.ReplaceAll(raw, `\"`, `"`)
		raw = strings.ReplaceAll(raw, `\n`, "\n")
		raw = strings.ReplaceAll(raw, `\\`, `\`)
		return strings.TrimSpace(raw), nil, nil
	}
	return "", nil, fmt.Errorf("decode summary: %w (raw first 200: %q)", errMalformed, truncate(s, 200))
}

var errMalformed = fmt.Errorf("malformed summary response")

func filepathExt(p string) string { return filepath.Ext(p) }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// LoadAPIKey reads GEMINI_API_KEY env or falls back to gemini_api_key.txt in cwd.
func LoadAPIKey() (string, error) {
	if k := strings.TrimSpace(os.Getenv("GEMINI_API_KEY")); k != "" {
		return k, nil
	}
	b, err := os.ReadFile("gemini_api_key.txt")
	if err != nil {
		return "", fmt.Errorf("no GEMINI_API_KEY env var and gemini_api_key.txt not found")
	}
	return strings.TrimSpace(string(b)), nil
}
