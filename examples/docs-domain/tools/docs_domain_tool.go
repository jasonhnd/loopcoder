package main

import (
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: docs_domain_tool <render|mcp>")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "render":
		if err := render(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "mcp":
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
}

func render() error {
	if err := os.MkdirAll("out", 0o755); err != nil {
		return err
	}
	content := "Rendered IR report\n\nReporting period: FY2026 Q4\nDisclosure review: required\n"
	return os.WriteFile(filepath.Join("out", "report.txt"), []byte(content), 0o644)
}
