// cmd/inspect is a one-off doc-body dumper. It reuses the gdocs OAuth
// stack so it can refresh the token cache on its own.
//
//	go run ./cmd/inspect <docID>
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/mikelady/kai/internal/gdocs"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: inspect <docID>")
		os.Exit(2)
	}
	ctx := context.Background()
	svc, err := gdocs.New(ctx, "client_secrets.json", "google_token.json")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	body, err := svc.ReadBody(ctx, os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Print(body)
}
