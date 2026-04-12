// cmd/preview pulls the Photos-library cached thumbnail for every
// process=yes row in selfie_videos.csv so the user can eyeball whether
// each candidate is actually a driving-selfie video before Gemini
// transcribes it.
//
// Uses osxphotos' --preview export (no iCloud round-trip, no Gemini).
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mikelady/kai/internal/scan"
)

const (
	csvPath = "selfie_videos.csv"
	destDir = "tmp/previews"
)

func main() {
	ctx := context.Background()

	rows, err := scan.AllRows(csvPath)
	if err != nil {
		die(err)
	}
	var yes []scan.Candidate
	for _, r := range rows {
		if r.Process == "yes" {
			yes = append(yes, r)
		}
	}
	if len(yes) == 0 {
		fmt.Println("No yes-rows in selfie_videos.csv.")
		return
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		die(err)
	}

	fmt.Printf("Exporting previews for %d candidates → %s\n\n", len(yes), destDir)
	for i, c := range yes {
		fmt.Printf("[%d/%d] %s  %s  %s\n", i+1, len(yes), c.Date[:10], c.DurationHuman, c.Filename)
		if err := exportPreview(ctx, c.UUID, destDir); err != nil {
			fmt.Printf("  ERR: %v\n", err)
			continue
		}
		if p := findPreview(destDir, c.UUID); p != "" {
			fmt.Printf("  %s\n", p)
		}
	}

	// Open the folder in Finder so the user can scan the grid.
	_ = exec.Command("open", destDir).Start()
	fmt.Printf("\nOpened %s in Finder. Flip `process` to `no` in %s for any non-driving row.\n", destDir, csvPath)
}

func exportPreview(ctx context.Context, uuid, dest string) error {
	// Each UUID gets its own subdirectory so osxphotos' per-dir export db
	// doesn't collide between runs and we don't need --update semantics.
	sub := filepath.Join(dest, uuid)
	if err := os.RemoveAll(sub); err != nil {
		return err
	}
	if err := os.MkdirAll(sub, 0o755); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "osxphotos", "export",
		"--uuid", uuid,
		"--preview",
		"--preview-if-missing",
		sub,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("osxphotos: %w (%s)", err, shorten(string(out), 300))
	}
	return nil
}

// findPreview scans the per-UUID subdir for the *_preview.jpeg file osxphotos
// emits. Deletes any non-preview stragglers (e.g. a stale full-size export)
// so the Finder view is just the thumbnails the user wants to eyeball.
func findPreview(destDir, uuid string) string {
	sub := filepath.Join(destDir, uuid)
	entries, err := os.ReadDir(sub)
	if err != nil {
		return ""
	}
	var preview string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		full := filepath.Join(sub, name)
		if name[0] == '.' {
			continue
		}
		if isPreviewName(name) {
			preview = full
			continue
		}
		// Anything else in here is junk for our purposes — remove it.
		_ = os.Remove(full)
	}
	return preview
}

func isPreviewName(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, "_preview.jpeg") || strings.HasSuffix(lower, "_preview.jpg")
}

func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
