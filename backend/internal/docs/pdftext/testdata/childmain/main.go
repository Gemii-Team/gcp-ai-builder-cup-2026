// Command childmain is the smallest program that links package pdftext: all it can do is
// be the child of the sandbox, through the init hook of the package, which the tests
// build it to run under the limits a production binary gets.
package main

import (
	"fmt"
	"os"

	_ "kiwi/internal/docs/pdftext"
)

func main() {
	fmt.Fprintln(os.Stderr, "childmain only runs as the child of the pdftext sandbox")
	os.Exit(1)
}
