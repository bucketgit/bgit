package cli

import (
	"fmt"
	"io"
	"os"
)

type Application func(executable string, args []string, stdin io.Reader, stdout, stderr io.Writer) error

func Main(run Application) {
	if err := run(os.Args[0], os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}
