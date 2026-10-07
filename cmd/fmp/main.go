package main

import (
	"os"

	"github.com/tobiash/gitops-preview-toolkit/internal/cli"
)

var version = "dev"

func main() { os.Exit(cli.Run("fmp", version)) }
