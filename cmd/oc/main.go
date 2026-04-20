package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/opensandbox/opensandbox/cmd/oc/internal/commands"
)

func main() {
	if err := commands.Execute(); err != nil {
		// An ExitError means the command already rendered its output and
		// just wants a specific exit code (e.g. 3 for user-fixable upstream
		// errors, 5 for transient). Don't reprint the message.
		var exit *commands.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.Code)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
