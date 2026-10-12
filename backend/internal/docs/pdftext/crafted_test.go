package pdftext

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"strings"
)

// The files below are a few hundred bytes each and make the PDF library fail in ways
// nothing in this process can recover from, inside pdf.NewReader or the first lookup of
// an object. They are why the file is read in a child process (see sandbox.go), and
// the tests of the sandbox feed them to it. Do not call extract, or anything that
// reaches the library, on one of them in a process you want to keep.

// crafted is a file that kills or stalls the PDF library, with what it does to it.
type crafted struct {
	name string
	pdf  func() []byte
	// does is what happens to a process that opens it.
	does string
}

var craftedFiles = []crafted{
	{"two object streams that hold each other", objectStreamCyclePDF, "stack overflow"},
	{"an object stream that extends itself", objectStreamExtendsLoopPDF, "endless loop"},
	{"a cross-reference index that starts at four thousand million", xrefIndexPDF, "out of memory"},
	{"a cross-reference section that precedes itself", prevLoopPDF, "endless loop"},
}

// xrefStreamPDF is a one-page PDF whose cross-reference is a stream. Objects 1 to 4 are
// the catalog, the page tree, the page, which takes its /Resources from object 6, and an
// empty content stream. extraObjStm, when set, is the dictionary entries of an object
// stream written as object 7. entries gives the cross-reference entry of each object
// as type, second field and third field; offset(n) is where object n was written.
func xrefStreamPDF(index, extraObjStm string, entries func(offset func(n int) int) [][3]int) []byte {
	var b bytes.Buffer
	offsets := map[int]int{}
	define := func(n int, body string) {
		offsets[n] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", n, body)
	}
	b.WriteString("%PDF-1.5\n")
	define(1, "<< /Type /Catalog /Pages 2 0 R >>")
	define(2, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	define(3, "<< /Type /Page /Parent 2 0 R /Contents 4 0 R /Resources 6 0 R >>")
	const content = "BT /F1 12 Tf 72 720 Td (x) Tj ET"
	define(4, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content))
	if extraObjStm != "" {
		data := "99 0 <<>>"
		define(7, fmt.Sprintf("<< /Type /ObjStm /N 1 /First 5 %s /Length %d >>\nstream\n%s\nendstream", extraObjStm, len(data), data))
	}
	all := entries(func(n int) int { return offsets[n] })
	var table bytes.Buffer
	for _, e := range all {
		table.WriteByte(byte(e[0]))
		table.Write(binary.BigEndian.AppendUint32(nil, uint32(e[1])))
		table.Write(binary.BigEndian.AppendUint16(nil, uint16(e[2])))
	}
	if index == "" {
		index = fmt.Sprintf("[0 %d]", len(all))
	}
	xref := b.Len()
	fmt.Fprintf(&b, "5 0 obj\n<< /Type /XRef /Size %d /Index %s /W [1 4 2] /Root 1 0 R /Length %d >>\nstream\n",
		len(all), index, table.Len())
	b.Write(table.Bytes())
	fmt.Fprintf(&b, "\nendstream\nendobj\nstartxref\n%d\n%%%%EOF\n", xref)
	return b.Bytes()
}

// objectStreamCyclePDF: /Resources is object 6, which the table says lives in object
// stream 7, which it says lives in object stream 6. Loading either asks for the other.
func objectStreamCyclePDF() []byte {
	return xrefStreamPDF("", "", func(at func(int) int) [][3]int {
		return [][3]int{
			{0, 0, 65535}, {1, at(1), 0}, {1, at(2), 0}, {1, at(3), 0}, {1, at(4), 0}, {1, 0, 0},
			{2, 7, 0}, {2, 6, 0},
		}
	})
}

// objectStreamExtendsLoopPDF: object 6 lives in object stream 7, which does not hold it
// and extends itself, so looking for it goes round for ever.
func objectStreamExtendsLoopPDF() []byte {
	return xrefStreamPDF("", "/Extends 7 0 R", func(at func(int) int) [][3]int {
		return [][3]int{
			{0, 0, 65535}, {1, at(1), 0}, {1, at(2), 0}, {1, at(3), 0}, {1, at(4), 0}, {1, 0, 0},
			{2, 7, 0}, {1, at(7), 0},
		}
	})
}

// xrefIndexPDF: the cross-reference stream starts its entries at object 4000000000,
// and the library makes its table that long.
func xrefIndexPDF() []byte {
	return xrefStreamPDF("[4000000000 1 0 7]", "", func(at func(int) int) [][3]int {
		return [][3]int{
			{1, 0, 0},
			{0, 0, 65535}, {1, at(1), 0}, {1, at(2), 0}, {1, at(3), 0}, {1, at(4), 0}, {1, 0, 0}, {2, 7, 0},
		}
	})
}

// prevLoopPDF: the trailer names its own cross-reference section as the previous one.
func prevLoopPDF() []byte {
	return rawPDFWith(true,
		"<< /Type /Catalog /Pages 2 0 R >>",
		onePage,
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R "+fontRes(5)+" >>",
		stream("", show("never read")),
		helveticaObj,
	)
}

// overcommitsEverything reports whether the host hands out any amount of memory it is
// asked for, in which case the file that asks for a hundred gigabytes would be given
// them and the test of it must not run in a process that has no memory limit.
func overcommitsEverything() bool {
	b, err := os.ReadFile("/proc/sys/vm/overcommit_memory")
	return err == nil && strings.TrimSpace(string(b)) == "1"
}
