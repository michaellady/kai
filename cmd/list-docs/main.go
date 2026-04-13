// cmd/list-docs dumps every Google Doc in the Kai Transcripts folder so
// we can spot duplicates (same title → two different doc IDs).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	drive "google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

const folderName = "Kai Transcripts"

func main() {
	ctx := context.Background()
	driveSvc, err := authDrive(ctx)
	if err != nil {
		die(err)
	}

	q := fmt.Sprintf("name = %q and mimeType = 'application/vnd.google-apps.folder' and trashed = false", folderName)
	res, err := driveSvc.Files.List().Q(q).Fields("files(id,name)").Do()
	if err != nil {
		die(err)
	}
	if len(res.Files) == 0 {
		die(fmt.Errorf("folder %q not found", folderName))
	}
	folderID := res.Files[0].Id

	q2 := fmt.Sprintf("%q in parents and mimeType = 'application/vnd.google-apps.document' and trashed = false", folderID)
	docs, err := driveSvc.Files.List().Q(q2).Fields("files(id,name,createdTime,modifiedTime)").OrderBy("name,createdTime").PageSize(1000).Do()
	if err != nil {
		die(err)
	}
	fmt.Printf("%-35s  %-25s  %-25s  id\n", "name", "created", "modified")
	for _, f := range docs.Files {
		fmt.Printf("%-35s  %-25s  %-25s  %s\n", f.Name, f.CreatedTime, f.ModifiedTime, f.Id)
	}
	// Count dupes.
	counts := map[string]int{}
	for _, f := range docs.Files {
		counts[f.Name]++
	}
	var dupeNames []string
	for n, c := range counts {
		if c > 1 {
			dupeNames = append(dupeNames, fmt.Sprintf("  %s × %d", n, c))
		}
	}
	sort.Strings(dupeNames)
	fmt.Printf("\nTotal docs: %d\n", len(docs.Files))
	if len(dupeNames) > 0 {
		fmt.Println("Duplicates:")
		for _, d := range dupeNames {
			fmt.Println(d)
		}
	} else {
		fmt.Println("No duplicates.")
	}
}

func authDrive(ctx context.Context) (*drive.Service, error) {
	secrets, err := os.ReadFile("client_secrets.json")
	if err != nil {
		return nil, err
	}
	cfg, err := google.ConfigFromJSON(secrets, drive.DriveFileScope)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile("google_token.json")
	if err != nil {
		return nil, err
	}
	var tok oauth2.Token
	if err := json.Unmarshal(b, &tok); err != nil {
		return nil, err
	}
	return drive.NewService(ctx, option.WithTokenSource(cfg.TokenSource(ctx, &tok)))
}

func die(err error) { fmt.Fprintln(os.Stderr, "error:", err); os.Exit(1) }
