package main

import (
	"os"

	"github.com/jasonhnd/loopcoder/internal/cli"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	os.Exit(cli.RunWithBuildInfo(os.Args[1:], os.Stdout, os.Stderr, cli.BuildInfo{
		Version: version,
		Commit:  commit,
		Date:    date,
	}))
}
