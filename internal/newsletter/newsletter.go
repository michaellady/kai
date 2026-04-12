// Package newsletter ingests a beehiiv-hosted RSS feed and exposes the
// full post HTML, plus a lightweight HTML-to-plain-text converter so
// callers can feed posts directly to Gemini for summary + tags without
// a separate cleanup pass.
package newsletter

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/html"
)

const DefaultFeedURL = "https://rss.beehiiv.com/feeds/9AbhG8CTgD.xml"

// Post is one newsletter item.
type Post struct {
	Process     string // "yes" by default; user flips to "no" to skip
	PublishDate string // YYYY-MM-DD
	Title       string
	Slug        string
	URL         string
	HTML        string // populated only by FetchFeed; not persisted in the CSV
}

// FetchFeed downloads and parses the RSS feed. The returned Posts include
// the full HTML body from content:encoded.
func FetchFeed(ctx context.Context, url string) ([]Post, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "kai/0.1 (+https://github.com/michaellady/kai)")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("feed %s: HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var doc rssDoc
	if err := xml.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("rss parse: %w", err)
	}
	var out []Post
	for _, it := range doc.Channel.Items {
		pub, _ := parseRSSDate(it.PubDate)
		out = append(out, Post{
			Process:     "yes",
			PublishDate: pub,
			Title:       strings.TrimSpace(it.Title),
			Slug:        slugFromLink(it.Link),
			URL:         strings.TrimSpace(it.Link),
			HTML:        it.ContentEncoded,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PublishDate < out[j].PublishDate })
	return out, nil
}

// ParseHTMLToText strips tags and returns readable plain text. Block-level
// elements emit blank-line boundaries so paragraphs survive; inline elements
// emit their text inline. Whitespace is collapsed.
func ParseHTMLToText(htmlStr string) string {
	z := html.NewTokenizer(strings.NewReader(htmlStr))
	var buf bytes.Buffer
	skipDepth := 0 // inside <script>/<style>/<noscript>
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			if z.Err() == io.EOF {
				break
			}
			break
		}
		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			tag, _ := z.TagName()
			name := string(tag)
			if name == "script" || name == "style" || name == "noscript" {
				skipDepth++
			}
			if tt == html.StartTagToken && isBlock(name) {
				buf.WriteString("\n\n")
			}
			if name == "br" {
				buf.WriteString("\n")
			}
		case html.EndTagToken:
			tag, _ := z.TagName()
			name := string(tag)
			if name == "script" || name == "style" || name == "noscript" {
				if skipDepth > 0 {
					skipDepth--
				}
			}
			if isBlock(name) {
				buf.WriteString("\n\n")
			}
		case html.TextToken:
			if skipDepth > 0 {
				continue
			}
			buf.Write(z.Text())
		}
	}
	return collapseWhitespace(buf.String())
}

// ---------------------------------------------------------------------------
// CSV
// ---------------------------------------------------------------------------

func WriteCSV(path string, posts []Post) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	if err := w.Write([]string{"process", "publish_date", "title", "slug", "url"}); err != nil {
		return err
	}
	for _, p := range posts {
		if err := w.Write([]string{p.Process, p.PublishDate, p.Title, p.Slug, p.URL}); err != nil {
			return err
		}
	}
	return nil
}

func ReadCSV(path string) ([]Post, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	rows, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	header := rows[0]
	idx := func(name string) int {
		for i, h := range header {
			if h == name {
				return i
			}
		}
		return -1
	}
	iProc, iDate, iTitle, iSlug, iURL := idx("process"), idx("publish_date"), idx("title"), idx("slug"), idx("url")
	var out []Post
	for _, row := range rows[1:] {
		if len(row) < len(header) {
			continue
		}
		if row[iProc] != "yes" || row[iURL] == "" {
			continue
		}
		out = append(out, Post{
			Process: row[iProc], PublishDate: row[iDate], Title: row[iTitle],
			Slug: row[iSlug], URL: row[iURL],
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// RSS types
// ---------------------------------------------------------------------------

type rssDoc struct {
	XMLName xml.Name   `xml:"rss"`
	Channel rssChannel `xml:"channel"`
}

type rssChannel struct {
	Items []rssItem `xml:"item"`
}

type rssItem struct {
	Title          string `xml:"title"`
	Link           string `xml:"link"`
	PubDate        string `xml:"pubDate"`
	GUID           string `xml:"guid"`
	ContentEncoded string `xml:"http://purl.org/rss/1.0/modules/content/ encoded"`
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func parseRSSDate(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	for _, layout := range []string{
		time.RFC1123Z,
		time.RFC1123,
		"Mon, 2 Jan 2006 15:04:05 -0700",
		"Mon, 2 Jan 2006 15:04:05 MST",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format("2006-01-02"), nil
		}
	}
	return s, fmt.Errorf("unknown rss date format: %q", s)
}

func slugFromLink(link string) string {
	i := strings.LastIndex(link, "/p/")
	if i < 0 {
		i = strings.LastIndex(link, "/")
		if i < 0 {
			return link
		}
		return link[i+1:]
	}
	return link[i+len("/p/"):]
}

func isBlock(tag string) bool {
	switch tag {
	case "p", "div", "section", "article", "header", "footer", "main",
		"h1", "h2", "h3", "h4", "h5", "h6",
		"ul", "ol", "li", "blockquote", "pre", "table", "tr", "hr":
		return true
	}
	return false
}

func collapseWhitespace(s string) string {
	// Normalize Windows/Mac line endings.
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	// Decode common HTML entities that the tokenizer leaves untouched when
	// they appear inside attributes or unparsed chunks.
	for _, r := range [][2]string{
		{"&nbsp;", " "}, {"&amp;", "&"}, {"&lt;", "<"}, {"&gt;", ">"},
		{"&quot;", "\""}, {"&#39;", "'"}, {"&apos;", "'"},
		{"&mdash;", "—"}, {"&ndash;", "–"}, {"&hellip;", "…"},
	} {
		s = strings.ReplaceAll(s, r[0], r[1])
	}
	// Collapse runs of whitespace on each line, and collapse blank-line runs.
	var out strings.Builder
	blankCount := 0
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(collapseSpaces(line))
		if trimmed == "" {
			blankCount++
			if blankCount <= 1 {
				out.WriteByte('\n')
			}
			continue
		}
		blankCount = 0
		out.WriteString(trimmed)
		out.WriteByte('\n')
	}
	return strings.TrimSpace(out.String())
}

func collapseSpaces(s string) string {
	var b strings.Builder
	prevSpace := false
	for _, r := range s {
		if r == ' ' || r == '\t' {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		prevSpace = false
		b.WriteRune(r)
	}
	return b.String()
}

