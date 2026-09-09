// Command acpgen generates the schema package from a pinned ACP JSON Schema
// release. The generated types, method registry, and parity fixtures are
// committed so ordinary builds never invoke this tool.
//
// Typical use, from the repository root:
//
//	go run ./cmd/acpgen -update 1.21.0   # download a release and pin it
//	go run ./cmd/acpgen                  # regenerate schema/*_gen*.go
//
// The schema releases live at
// https://github.com/agentclientprotocol/agent-client-protocol/releases,
// tagged schema-v<version>, with schema.json and meta.json attached.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	update := flag.String("update", "", "download and pin schema-v<version> before generating (e.g. 1.21.0)")
	schemaPath := flag.String("schema", "schema/schema.json", "path to the pinned schema.json")
	metaPath := flag.String("meta", "schema/meta.json", "path to the pinned meta.json")
	outDir := flag.String("out", "schema", "directory receiving generated files")
	flag.Parse()

	if *update != "" {
		version := *update
		if !strings.HasPrefix(version, "schema-v") {
			version = "schema-v" + version
		}
		base := "https://github.com/agentclientprotocol/agent-client-protocol/releases/download/" + version + "/"
		fetch(base+"meta.json", *metaPath)
		fetch(base+"schema.json", *schemaPath)
		if err := os.WriteFile(filepath.Join(filepath.Dir(*schemaPath), "VERSION"), []byte(version+"\n"), 0o644); err != nil {
			fatal(err)
		}
	}

	schema, err := loadSchema(*schemaPath)
	if err != nil {
		fatal(fmt.Errorf("load %s: %w", *schemaPath, err))
	}
	meta, err := loadMeta(*metaPath)
	if err != nil {
		fatal(fmt.Errorf("load %s: %w", *metaPath, err))
	}
	pin := strings.TrimSpace(readPin(filepath.Join(filepath.Dir(*schemaPath), "VERSION")))
	if pin == "" || pin == "unknown" {
		fatal(fmt.Errorf("schema/VERSION is missing; run with -update <version> first"))
	}

	ir, err := buildIR(schema, meta, pin)
	if err != nil {
		fatal(fmt.Errorf("build IR: %w", err))
	}
	for _, f := range emit(ir, pin) {
		path := filepath.Join(*outDir, f.name)
		if err := os.WriteFile(path, f.source, 0o644); err != nil {
			fatal(err)
		}
		fmt.Printf("wrote %s\n", path)
	}
}

func readPin(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "unknown"
	}
	return string(b)
}

func fetch(url, dest string) {
	resp, err := http.Get(url)
	if err != nil {
		fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fatal(fmt.Errorf("GET %s: %s", url, resp.Status))
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		fatal(err)
	}
	if err := os.WriteFile(dest, b, 0o644); err != nil {
		fatal(err)
	}
	var probe map[string]json.RawMessage
	if json.Unmarshal(b, &probe) != nil {
		fatal(fmt.Errorf("%s is not JSON", dest))
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "acpgen:", err)
	os.Exit(1)
}
