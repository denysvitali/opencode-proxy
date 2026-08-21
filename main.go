package main

import (
	"os"

	"github.com/denysvitali/opencode-proxy/cmd"
)

func main() {
	os.Exit(cmd.Execute())
}
