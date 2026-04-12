// Package gdocs handles Google OAuth, Drive folder/file lookup, and
// monthly Google Docs creation + appends with marker-delimited headers.
//
// Scopes: drive.file + documents. drive.file limits access to files this
// app created, which is fine for the Kai folder workflow.
package gdocs

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/mikelady/kai/internal/retry"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	docs "google.golang.org/api/docs/v1"
	drive "google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

const (
	HeaderStart = "<!-- kai:header:start -->"
	HeaderEnd   = "<!-- kai:header:end -->"
)

type Service struct {
	drive *drive.Service
	docs  *docs.Service
}

// New authorizes via OAuth (installed-app flow, cached in tokenFile) and
// constructs Drive + Docs services.
func New(ctx context.Context, clientSecretsPath, tokenFile string) (*Service, error) {
	secrets, err := os.ReadFile(clientSecretsPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", clientSecretsPath, err)
	}
	cfg, err := google.ConfigFromJSON(secrets, drive.DriveFileScope, docs.DocumentsScope)
	if err != nil {
		return nil, fmt.Errorf("parse client_secrets: %w", err)
	}
	tok, err := loadOrFetchToken(ctx, cfg, tokenFile)
	if err != nil {
		return nil, err
	}
	ts := cfg.TokenSource(ctx, tok)
	driveSvc, err := drive.NewService(ctx, option.WithTokenSource(ts))
	if err != nil {
		return nil, err
	}
	docsSvc, err := docs.NewService(ctx, option.WithTokenSource(ts))
	if err != nil {
		return nil, err
	}
	return &Service{drive: driveSvc, docs: docsSvc}, nil
}

// ---------------------------------------------------------------------------
// Drive: folder + doc lookup/create
// ---------------------------------------------------------------------------

func (s *Service) GetOrCreateFolder(ctx context.Context, name string) (string, error) {
	q := fmt.Sprintf("name = %q and mimeType = 'application/vnd.google-apps.folder' and trashed = false", name)
	res, err := retry.Do(ctx, func(ctx context.Context) (*drive.FileList, error) {
		return s.drive.Files.List().Q(q).Spaces("drive").Fields("files(id,name)").Context(ctx).Do()
	})
	if err != nil {
		return "", err
	}
	if len(res.Files) > 0 {
		return res.Files[0].Id, nil
	}
	f, err := retry.Do(ctx, func(ctx context.Context) (*drive.File, error) {
		return s.drive.Files.Create(&drive.File{
			Name:     name,
			MimeType: "application/vnd.google-apps.folder",
		}).Fields("id").Context(ctx).Do()
	})
	if err != nil {
		return "", err
	}
	return f.Id, nil
}

func (s *Service) FindDocInFolder(ctx context.Context, folderID, title string) (string, error) {
	q := fmt.Sprintf("name = %q and mimeType = 'application/vnd.google-apps.document' and %q in parents and trashed = false", title, folderID)
	res, err := retry.Do(ctx, func(ctx context.Context) (*drive.FileList, error) {
		return s.drive.Files.List().Q(q).Spaces("drive").Fields("files(id,name)").Context(ctx).Do()
	})
	if err != nil {
		return "", err
	}
	if len(res.Files) == 0 {
		return "", nil
	}
	return res.Files[0].Id, nil
}

// CreateDoc makes a new Google Doc in folderID with the given title,
// seeds the marker-delimited header block, and returns the doc ID + URL.
func (s *Service) CreateDoc(ctx context.Context, folderID, title, monthLabel string) (docID, docURL string, err error) {
	f, err := retry.Do(ctx, func(ctx context.Context) (*drive.File, error) {
		return s.drive.Files.Create(&drive.File{
			Name:     title,
			MimeType: "application/vnd.google-apps.document",
			Parents:  []string{folderID},
		}).Fields("id,webViewLink").Context(ctx).Do()
	})
	if err != nil {
		return "", "", err
	}
	// Seed with marker-delimited header and a visible separator.
	header := fmt.Sprintf("%s\n# Thoughts — %s\n**Recordings:** 0  **Total Duration:** 0:00:00\n%s\n\n---\n\n",
		HeaderStart, monthLabel, HeaderEnd)
	_, err = retry.Do(ctx, func(ctx context.Context) (*docs.BatchUpdateDocumentResponse, error) {
		return s.docs.Documents.BatchUpdate(f.Id, &docs.BatchUpdateDocumentRequest{
			Requests: []*docs.Request{
				{InsertText: &docs.InsertTextRequest{Location: &docs.Location{Index: 1}, Text: header}},
			},
		}).Context(ctx).Do()
	})
	if err != nil {
		return "", "", err
	}
	return f.Id, f.WebViewLink, nil
}

// ---------------------------------------------------------------------------
// Docs: append + header update
// ---------------------------------------------------------------------------

// AppendEntry writes block at the end of the doc body.
func (s *Service) AppendEntry(ctx context.Context, docID, block string) error {
	doc, err := retry.Do(ctx, func(ctx context.Context) (*docs.Document, error) {
		return s.docs.Documents.Get(docID).Context(ctx).Do()
	})
	if err != nil {
		return err
	}
	end := bodyEndIndex(doc)
	_, err = retry.Do(ctx, func(ctx context.Context) (*docs.BatchUpdateDocumentResponse, error) {
		return s.docs.Documents.BatchUpdate(docID, &docs.BatchUpdateDocumentRequest{
			Requests: []*docs.Request{
				{InsertText: &docs.InsertTextRequest{Location: &docs.Location{Index: end}, Text: block}},
			},
		}).Context(ctx).Do()
	})
	return err
}

// UpdateHeader replaces the content between the kai:header markers with
// a fresh header block that encodes the current count/duration. If markers
// are missing (legacy doc), it inserts them at the top first.
func (s *Service) UpdateHeader(ctx context.Context, docID, monthLabel string, count int, totalDurationHuman string) error {
	doc, err := retry.Do(ctx, func(ctx context.Context) (*docs.Document, error) {
		return s.docs.Documents.Get(docID).Context(ctx).Do()
	})
	if err != nil {
		return err
	}
	start, end, found := findMarkerRange(doc)
	headerBody := fmt.Sprintf("\n# Thoughts — %s\n**Recordings:** %d  **Total Duration:** %s\n",
		monthLabel, count, totalDurationHuman)

	var reqs []*docs.Request
	if !found {
		// Legacy upgrade: insert full marker block at index 1.
		full := fmt.Sprintf("%s%s%s\n", HeaderStart, headerBody, HeaderEnd)
		reqs = []*docs.Request{
			{InsertText: &docs.InsertTextRequest{Location: &docs.Location{Index: 1}, Text: full}},
		}
	} else {
		// Replace content strictly between the markers.
		// Delete (start, end) exclusive of the marker lines themselves.
		reqs = []*docs.Request{
			{DeleteContentRange: &docs.DeleteContentRangeRequest{Range: &docs.Range{StartIndex: start, EndIndex: end}}},
			{InsertText: &docs.InsertTextRequest{Location: &docs.Location{Index: start}, Text: headerBody}},
		}
	}
	_, err = retry.Do(ctx, func(ctx context.Context) (*docs.BatchUpdateDocumentResponse, error) {
		return s.docs.Documents.BatchUpdate(docID, &docs.BatchUpdateDocumentRequest{Requests: reqs}).Context(ctx).Do()
	})
	return err
}

// ReadBody returns the plain-text content of the doc (useful for monthly
// overview input).
func (s *Service) ReadBody(ctx context.Context, docID string) (string, error) {
	doc, err := retry.Do(ctx, func(ctx context.Context) (*docs.Document, error) {
		return s.docs.Documents.Get(docID).Context(ctx).Do()
	})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if doc.Body != nil {
		for _, el := range doc.Body.Content {
			if el.Paragraph == nil {
				continue
			}
			for _, pe := range el.Paragraph.Elements {
				if pe.TextRun != nil {
					b.WriteString(pe.TextRun.Content)
				}
			}
		}
	}
	return b.String(), nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func bodyEndIndex(doc *docs.Document) int64 {
	if doc.Body == nil || len(doc.Body.Content) == 0 {
		return 1
	}
	last := doc.Body.Content[len(doc.Body.Content)-1]
	if last.EndIndex > 1 {
		// Insert *before* the trailing newline.
		return last.EndIndex - 1
	}
	return 1
}

// findMarkerRange locates the text strictly between HeaderStart and HeaderEnd
// in the body. Returns (startExclusive, endExclusive) — i.e. the range of
// content to delete when replacing the header. The markers themselves are
// preserved.
func findMarkerRange(doc *docs.Document) (int64, int64, bool) {
	if doc.Body == nil {
		return 0, 0, false
	}
	// Collect every text run with its absolute start index.
	type runRef struct {
		start   int64
		end     int64
		content string
	}
	var runs []runRef
	for _, el := range doc.Body.Content {
		if el.Paragraph == nil {
			continue
		}
		for _, pe := range el.Paragraph.Elements {
			if pe.TextRun != nil {
				runs = append(runs, runRef{start: pe.StartIndex, end: pe.EndIndex, content: pe.TextRun.Content})
			}
		}
	}
	findIn := func(needle string) (int64, int64, bool) {
		for _, r := range runs {
			if i := strings.Index(r.content, needle); i >= 0 {
				startAbs := r.start + int64(utf16Offset(r.content, i))
				endAbs := startAbs + int64(utf16Len(needle))
				return startAbs, endAbs, true
			}
		}
		return 0, 0, false
	}
	_, sEnd, ok1 := findIn(HeaderStart)
	eStart, _, ok2 := findIn(HeaderEnd)
	if !ok1 || !ok2 || sEnd >= eStart {
		return 0, 0, false
	}
	return sEnd, eStart, true
}

// utf16Offset / utf16Len convert between byte indices in a Go string and
// UTF-16 code units, which is what the Docs API measures indices in.
func utf16Offset(s string, byteIdx int) int {
	// Count UTF-16 code units up to byteIdx.
	n := 0
	for i, r := range s {
		if i >= byteIdx {
			break
		}
		if r <= 0xFFFF {
			n++
		} else {
			n += 2
		}
	}
	return n
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r <= 0xFFFF {
			n++
		} else {
			n += 2
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// OAuth installed-app flow
// ---------------------------------------------------------------------------

func loadOrFetchToken(ctx context.Context, cfg *oauth2.Config, path string) (*oauth2.Token, error) {
	if tok, err := readToken(path); err == nil {
		// If expired and we have a refresh token, TokenSource will refresh automatically.
		return tok, nil
	}
	tok, err := runInstalledFlow(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := writeToken(path, tok); err != nil {
		return nil, err
	}
	return tok, nil
}

func readToken(path string) (*oauth2.Token, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tok oauth2.Token
	if err := json.Unmarshal(b, &tok); err != nil {
		return nil, err
	}
	return &tok, nil
}

func writeToken(path string, tok *oauth2.Token) error {
	b, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// runInstalledFlow implements Google's installed-app flow with a localhost
// redirect listener.
func runInstalledFlow(ctx context.Context, cfg *oauth2.Config) (*oauth2.Token, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	redirect := fmt.Sprintf("http://%s/", ln.Addr().String())
	cfg = withRedirect(cfg, redirect)

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			errCh <- err
			return
		}
		if e := q.Get("error"); e != "" {
			errCh <- fmt.Errorf("oauth error: %s", e)
			http.Error(w, "oauth error: "+e, http.StatusBadRequest)
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			return
		}
		fmt.Fprintln(w, "Kai: auth complete. You can close this tab.")
		codeCh <- code
	})}

	go func() { _ = srv.Serve(ln) }()
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	authURL := cfg.AuthCodeURL("kai-state", oauth2.AccessTypeOffline, oauth2.ApprovalForce)
	fmt.Println("Opening browser for Google OAuth…")
	fmt.Println("If nothing opens, visit:")
	fmt.Println(authURL)
	_ = openBrowser(authURL)

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case err := <-errCh:
		return nil, err
	case code := <-codeCh:
		return cfg.Exchange(ctx, code)
	case <-time.After(5 * time.Minute):
		return nil, fmt.Errorf("oauth flow timed out after 5 minutes")
	}
}

func withRedirect(cfg *oauth2.Config, redirect string) *oauth2.Config {
	cp := *cfg
	cp.RedirectURL = redirect
	return &cp
}

func openBrowser(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "linux":
		cmd = exec.Command("xdg-open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		return fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
	return cmd.Start()
}
