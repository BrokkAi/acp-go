// Command acp-trace-viewer serves one ACP JSONL transcript as an ordered
// sequence diagram in a browser.
//
//	acp-trace-viewer -addr 127.0.0.1:8787 session.jsonl
//
// The transcript is re-read on every poll, so a file that a live session keeps
// appending to updates in place.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"

	"github.com/BrokkAi/acp-go/traceviewer"
)

func main() {
	address := flag.String("addr", "127.0.0.1:8787", "address to serve the viewer on")
	openBrowser := flag.Bool("open", false, "open the viewer in the platform browser")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: acp-trace-viewer [flags] <transcript.jsonl>")
		flag.PrintDefaults()
		os.Exit(2)
	}
	transcript := flag.Arg(0)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	viewer := &traceviewer.Viewer{Source: traceviewer.FileSource{Path: transcript}}
	url := "http://" + *address + "/"
	log.Printf("ACP trace viewer: %s at %s", transcript, url)
	if *openBrowser {
		if err := traceviewer.Open(url); err != nil {
			log.Printf("could not open a browser: %v", err)
		}
	}
	if err := viewer.Run(ctx, *address); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}
