package pdftext

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"
)

// init makes the binary its own child: a process started with childEnv in its
// environment is the reader of one PDF, not the program it was built as. The hook is
// here so that every binary that links this package, the server and the test binaries
// alike, can be run again as a child without anyone calling it from main.
func init() {
	if mode := os.Getenv(childEnv); mode != "" {
		os.Exit(runChild(mode, os.Stdin, os.Stdout))
	}
}

// runChild is the child: it confines itself, says so, reads the PDF from in and prints
// the result to out. It returns the exit status. What kills it before it has printed
// the result (a stack overflow, an out-of-memory error, the CPU limit, the parent's
// kill) is for the parent to find out from the exit status.
func runChild(mode string, in io.Reader, out io.Writer) int {
	if err := confine(); err != nil {
		fmt.Fprintln(os.Stderr, "pdftext: cannot confine the reader process:", err)
		return 2
	}
	if _, err := out.Write([]byte{frameStarted}); err != nil {
		return 2
	}
	var result []byte
	switch mode {
	case modeExtract:
		pages, err := extract(context.Background(), in, defaultLimits)
		result = pagesFrame(pages, err)
	case modeCount:
		n, err := pageCount(context.Background(), in, defaultLimits)
		result = countFrame(n, err)
	default:
		fmt.Fprintf(os.Stderr, "pdftext: unknown mode %q\n", mode)
		return 2
	}
	if _, err := out.Write(result); err != nil {
		return 2
	}
	return 0
}

func errorFrame(err error) []byte {
	return append([]byte{frameError}, strings.TrimPrefix(err.Error(), ErrUnreadable.Error()+": ")...)
}

func pagesFrame(pages []string, err error) []byte {
	if err != nil {
		return errorFrame(err)
	}
	frame := binary.AppendUvarint([]byte{framePages}, uint64(len(pages)))
	for _, p := range pages {
		frame = binary.AppendUvarint(frame, uint64(len(p)))
		frame = append(frame, p...)
	}
	return frame
}

func countFrame(n int, err error) []byte {
	if err != nil {
		return errorFrame(err)
	}
	return binary.AppendUvarint([]byte{frameCount}, uint64(n))
}
