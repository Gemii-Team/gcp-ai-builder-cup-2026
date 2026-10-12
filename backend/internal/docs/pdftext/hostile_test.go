package pdftext

import (
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// These tests feed the extraction what a hostile uploader would: files a few kilobytes
// long that make an unbounded reader spin, grow or lie. Each runs under a deadline, so
// an extraction that hangs fails the test instead of the suite. extractWithin runs a
// file both ways: in this process, which is what the child of the sandbox does and where
// the limits of the extraction itself are what is tested, and through the sandbox, where
// the process that reads the file may die of it and the test process must not.

// within runs f and fails the test if it takes longer than d. A hung f keeps running
// in the background until the test binary exits.
func within(t *testing.T, d time.Duration, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("did not finish within %v", d)
	}
}

// boxed is the time every hostile fixture must be handled in.
const boxed = 5 * time.Second

// rawPDF writes a PDF of the given object bodies, numbered from 1 in order, with the
// catalog as object 1. A body that has a stream carries it itself.
func rawPDF(objs ...string) []byte { return rawPDFWith(false, objs...) }

// rawPDFWith is rawPDF; prevSelf makes the trailer name its own cross-reference section
// as the previous one.
func rawPDFWith(prevSelf bool, objs ...string) []byte {
	var b bytes.Buffer
	b.WriteString("%PDF-1.5\n")
	offsets := make([]int, len(objs))
	for i, body := range objs {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, body)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	prev := ""
	if prevSelf {
		prev = fmt.Sprintf(" /Prev %d", xref)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R%s >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, prev, xref)
	return b.Bytes()
}

func stream(dict, data string) string {
	return fmt.Sprintf("<< %s /Length %d >>\nstream\n%s\nendstream", dict, len(data), data)
}

// deflate compresses data as a zlib stream.
func deflate(data []byte) []byte {
	var b bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&b, zlib.BestSpeed)
	_, _ = zw.Write(data)
	_ = zw.Close()
	return b.Bytes()
}

// deflateRepeated compresses n bytes made of unit repeated, without holding them.
func deflateRepeated(unit string, n int) []byte {
	var b bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&b, zlib.BestSpeed)
	chunk := []byte(strings.Repeat(unit, 32<<10/len(unit)))
	for written := 0; written < n; written += len(chunk) {
		_, _ = zw.Write(chunk)
	}
	_ = zw.Close()
	return b.Bytes()
}

const helveticaObj = "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"

// fontRes is a /Resources entry with Helvetica as /F1, which is object n.
func fontRes(n int) string { return fmt.Sprintf("/Resources << /Font << /F1 %d 0 R >> >>", n) }

// treePDF is a document with the catalog as object 1, the page tree root pagesDict as
// object 2, one real page as 3 whose content stream is 4 and Helvetica 5; extra objects
// follow from 6.
func treePDF(pagesDict, content string, extra ...string) []byte {
	return rawPDF(append([]string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		pagesDict,
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R " + fontRes(5) + " >>",
		stream("", content),
		helveticaObj,
	}, extra...)...)
}

const onePage = "<< /Type /Pages /Kids [3 0 R] /Count 1 >>"

func show(text string) string { return "BT /F1 12 Tf 72 720 Td (" + text + ") Tj ET" }

// local runs the extraction in this process, as the child of the sandbox does.
func local(pdf []byte) ([]string, error) {
	return extract(context.Background(), bytes.NewReader(pdf), defaultLimits)
}

func localCount(pdf []byte) (int, error) {
	return pageCount(context.Background(), bytes.NewReader(pdf), defaultLimits)
}

// childBoxed is the time a file may take through the sandbox: a process to start, too.
const childBoxed = 15 * time.Second

// quietLogs keeps what the sandbox logs about a child that died out of the test output
// and returns it.
func quietLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	logs := new(bytes.Buffer)
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return logs
}

// same reports whether two outcomes of the extraction of one file are the same.
func same(pages1 []string, err1 error, pages2 []string, err2 error) bool {
	return slices.Equal(pages1, pages2) && (err1 == nil) == (err2 == nil) &&
		errors.Is(err1, ErrUnreadable) == errors.Is(err2, ErrUnreadable)
}

func extractWithin(t *testing.T, pdf []byte) (pages []string, err error) {
	t.Helper()
	quietLogs(t)
	within(t, boxed, func() { pages, err = local(pdf) })
	var viaChild []string
	var childErr error
	within(t, childBoxed, func() { viaChild, childErr = Extract(bytes.NewReader(pdf)) })
	if !same(pages, err, viaChild, childErr) {
		t.Fatalf("in this process: %q, %v; through the sandbox: %q, %v; want the same", pages, err, viaChild, childErr)
	}
	return pages, err
}

func countWithin(t *testing.T, pdf []byte) (n int, err error) {
	t.Helper()
	quietLogs(t)
	within(t, boxed, func() { n, err = localCount(pdf) })
	var viaChild int
	var childErr error
	within(t, childBoxed, func() { viaChild, childErr = PageCount(bytes.NewReader(pdf)) })
	if n != viaChild || (err == nil) != (childErr == nil) || errors.Is(err, ErrUnreadable) != errors.Is(childErr, ErrUnreadable) {
		t.Fatalf("in this process: %d, %v; through the sandbox: %d, %v; want the same", n, err, viaChild, childErr)
	}
	return n, err
}

func TestHostile_PageTreeThatListsItself(t *testing.T) {
	// The Pages node is its own first kid: walking it by /Count and index never ends.
	pdf := treePDF("<< /Type /Pages /Kids [2 0 R 3 0 R] /Count 1 >>", show("survivor"))
	pages, err := extractWithin(t, pdf)
	if err != nil || !slices.Equal(pages, []string{"survivor"}) {
		t.Fatalf("Extract = %q, %v, want the page that is there, once", pages, err)
	}
	if n, err := countWithin(t, pdf); err != nil || n != 1 {
		t.Fatalf("PageCount = %d, %v, want 1", n, err)
	}

	// With no page in it at all the document has no pages.
	pdf = treePDF("<< /Type /Pages /Kids [2 0 R] /Count 1 >>", show("x"))
	if pages, err := extractWithin(t, pdf); !errors.Is(err, ErrUnreadable) || pages != nil {
		t.Fatalf("Extract of a tree with no page = %q, %v, want ErrUnreadable", pages, err)
	}
}

func TestHostile_PageTreeCycleAcrossNodes(t *testing.T) {
	// Pages 2 -> 6 -> 7 -> 6 ... with the page listed in the loop: every pass would
	// find the page again.
	pdf := treePDF("<< /Type /Pages /Kids [6 0 R] /Count 1 >>", show("once"),
		"<< /Type /Pages /Parent 2 0 R /Kids [3 0 R 7 0 R] /Count 1 >>",
		"<< /Type /Pages /Parent 6 0 R /Kids [6 0 R 3 0 R] /Count 1 >>")
	pages, err := extractWithin(t, pdf)
	if err != nil || !slices.Equal(pages, []string{"once"}) {
		t.Fatalf("Extract = %q, %v, want the page once", pages, err)
	}
}

func TestHostile_ParentLoopWithoutResources(t *testing.T) {
	// The page and its Pages node are each other's parent and neither has /Resources:
	// looking for the inherited resources of a font never ends.
	pdf := rawPDF(
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 /Parent 3 0 R >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R >>",
		stream("", show("loop")),
	)
	pages, err := extractWithin(t, pdf)
	if err != nil || !slices.Equal(pages, []string{"loop"}) {
		t.Fatalf("Extract = %q, %v, want the text, read with the default font", pages, err)
	}
}

func TestHostile_DecompressionBomb(t *testing.T) {
	// 24 MiB of "q " that deflate squeezes into a few tens of kilobytes. Every q pushes
	// graphics state; decoded in full it is gigabytes of memory.
	bomb := deflateRepeated("q ", 24<<20)
	if len(bomb) > 256<<10 {
		t.Fatalf("the bomb is %d bytes, want a small file", len(bomb))
	}
	pdf := rawPDF(
		"<< /Type /Catalog /Pages 2 0 R >>",
		onePage,
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R "+fontRes(5)+" >>",
		stream("/Filter /FlateDecode", string(bomb)),
		helveticaObj,
	)
	pages, err := extractWithin(t, pdf)
	if !errors.Is(err, ErrUnreadable) || pages != nil {
		t.Fatalf("Extract = %d pages, %v, want ErrUnreadable", len(pages), err)
	}
}

func TestHostile_DeepQNestingKeepsTheTextBeforeIt(t *testing.T) {
	content := show("before") + " " + strings.Repeat("q ", 100_000) + show("after")
	pages, err := extractWithin(t, treePDF(onePage, content))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !slices.Equal(pages, []string{"before"}) {
		t.Fatalf("pages = %q, want the text before the nesting only", pages)
	}
}

func TestHostile_PageCountThatLies(t *testing.T) {
	// A real two-page document that says it has ten thousand pages.
	pdf := rawPDF(
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R 5 0 R] /Count 10000 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R "+fontRes(7)+" >>",
		stream("", show("one")),
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 6 0 R "+fontRes(7)+" >>",
		stream("", show("two")),
		helveticaObj,
	)
	pages, err := extractWithin(t, pdf)
	if err != nil || !slices.Equal(pages, []string{"one", "two"}) {
		t.Fatalf("Extract = %d pages %q, %v, want the two pages that are there", len(pages), pages, err)
	}
	if n, err := countWithin(t, pdf); err != nil || n != 2 {
		t.Fatalf("PageCount = %d, %v, want 2", n, err)
	}
}

func TestHostile_PageTreeTooDeep(t *testing.T) {
	// Forty Pages nodes in a chain, the page at the bottom.
	const levels = 40
	objs := []string{"<< /Type /Catalog /Pages 2 0 R >>"}
	for i := range levels {
		objs = append(objs, fmt.Sprintf("<< /Type /Pages /Kids [%d 0 R] /Count 1 >>", 3+i))
	}
	objs = append(objs,
		fmt.Sprintf("<< /Type /Page /Parent %d 0 R /MediaBox [0 0 612 792] /Contents %d 0 R >>", 1+levels, 3+levels),
		stream("", show("deep")))
	pages, err := extractWithin(t, rawPDF(objs...))
	if !errors.Is(err, ErrUnreadable) || pages != nil {
		t.Fatalf("Extract = %q, %v, want ErrUnreadable for a tree forty levels deep", pages, err)
	}
}

func TestHostile_PageTreeNodeListingTooMuch(t *testing.T) {
	kids := strings.Repeat("3 0 R ", 50_000)
	pages, err := extractWithin(t, treePDF("<< /Type /Pages /Kids ["+kids+"] /Count 1 >>", show("x")))
	if !errors.Is(err, ErrUnreadable) || pages != nil {
		t.Fatalf("Extract = %q, %v, want ErrUnreadable for a node of 50 000 kids", pages, err)
	}
}

func TestHostile_WidthTableThatNamesEverythingManyTimes(t *testing.T) {
	// A CID font whose /W lists the whole code space a hundred thousand times.
	w := "[" + strings.Repeat("0 65535 500 ", 100_000) + "]"
	pdf := cidFontPDF(w, "", "BT /F2 14 Tf 72 700 Td <000100020003000100040005> Tj ET")
	pages, err := extractWithin(t, pdf)
	if err != nil || !slices.Equal(pages, []string{"สวัสดี"}) {
		t.Fatalf("Extract = %q, %v, want the Thai word", pages, err)
	}
}

func TestHostile_ToUnicodeBomb(t *testing.T) {
	// A ToUnicode map of 8 MiB of blanks.
	bomb := deflateRepeated(" ", 8<<20)
	pdf := cidFontPDF("[]", stream("/Filter /FlateDecode", string(bomb)), "BT /F2 14 Tf 72 700 Td <00010002> Tj ET")
	pages, err := extractWithin(t, pdf)
	if !errors.Is(err, ErrUnreadable) || pages != nil {
		t.Fatalf("Extract = %q, %v, want ErrUnreadable", pages, err)
	}
}

// cidFontPDF is a one-page PDF with Helvetica as /F1 and, as /F2, a composite font
// whose descendant has the width array w; toUnicode is the object body of its map
// (the Thai map of the other tests when empty).
func cidFontPDF(w, toUnicode, content string) []byte {
	if toUnicode == "" {
		toUnicode = stream("", thaiToUnicode)
	}
	return rawPDF(
		"<< /Type /Catalog /Pages 2 0 R >>",
		onePage,
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R /F2 6 0 R >> >> >>",
		stream("", content),
		helveticaObj,
		"<< /Type /Font /Subtype /Type0 /BaseFont /Thai /Encoding /Identity-H /DescendantFonts [7 0 R] /ToUnicode 8 0 R >>",
		"<< /Type /Font /Subtype /CIDFontType2 /BaseFont /Thai /CIDSystemInfo << /Registry (Adobe) /Ordering (Identity) /Supplement 0 >> /DW 500 /W "+w+" >>",
		toUnicode,
	)
}

// ---- a compressed document --------------------------------------------------

// arialWidths are the advance widths, in thousandths of an em, of the printable ASCII
// characters from the space (32) to the tilde (126) in Helvetica and Arial.
var arialWidths = []int{
	278, 278, 355, 556, 556, 889, 667, 191, 333, 333, 389, 584, 278, 333, 278, 278, // space ! " # $ % & ' ( ) * + , - . /
	556, 556, 556, 556, 556, 556, 556, 556, 556, 556, // 0-9
	278, 278, 584, 584, 584, 556, 1015, // : ; < = > ? @
	667, 667, 722, 722, 667, 611, 778, 722, 278, 500, 667, 556, 833, 722, 778, 667, 778, 722, 667, 611, 722, 667, 944, 667, 667, 611, // A-Z
	278, 278, 278, 469, 556, 333, // [ \ ] ^ _ `
	556, 556, 500, 556, 556, 278, 556, 556, 222, 222, 500, 222, 833, 556, 556, 556, 556, 333, 500, 278, 556, 500, 722, 500, 500, 500, // a-z
	334, 260, 334, 584, // { | } ~
}

// advance is the width of text in points at the given font size.
func advance(text string, size float64) float64 {
	w := 0
	for _, c := range []byte(text) {
		w += arialWidths[c-32]
	}
	return float64(w) / 1000 * size
}

// flateFixture is a two-page PDF as a word processor writes one: Arial as a TrueType
// font with a width table, content compressed with Flate, the words of the first line
// placed one by one with nothing but the gap of a space between them, the second line a
// TJ array with kerns and gaps, and the second page's content split over two streams.
func flateFixture() []byte {
	const size = 12.0
	space := advance(" ", size)
	var p1 strings.Builder
	p1.WriteString("BT /F1 12 Tf 72 720 Td ")
	words := []string{"Quarterly", "report", "for", "Q4"}
	for i, word := range words {
		if i > 0 {
			// Td moves from the start of the line, which is where the last word began.
			fmt.Fprintf(&p1, "%.3f 0 Td ", advance(words[i-1], size)+space)
		}
		fmt.Fprintf(&p1, "(%s) Tj ", word)
	}
	p1.WriteString("ET BT /F1 12 Tf 14 TL 72 706 Td [(Rev)15(enue)-278(grew)-278(by)-278(12%)] TJ T* (Costs fell 3%.) Tj ET")

	res := "/Resources << /Font << /F1 3 0 R >> >>"
	return rawPDF(
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [4 0 R 6 0 R] /Count 2 >>",
		"<< /Type /Font /Subtype /TrueType /BaseFont /ArialMT /Encoding /WinAnsiEncoding /FirstChar 32 /LastChar 126 /Widths ["+joinInts(arialWidths)+"] >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 5 0 R "+res+" >>",
		stream("/Filter /FlateDecode", string(deflate([]byte(p1.String())))),
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents [7 0 R 8 0 R] "+res+" >>",
		stream("/Filter /FlateDecode", string(deflate([]byte("BT /F1 12 Tf 72 700 Td (First half) Tj")))),
		stream("/Filter /FlateDecode", string(deflate([]byte("0 -14 Td (second half) Tj ET")))),
	)
}

func joinInts(v []int) string {
	s := make([]string, len(v))
	for i, n := range v {
		s[i] = fmt.Sprint(n)
	}
	return strings.Join(s, " ")
}

const flateFixturePath = "testdata/flate-truetype.pdf"

func TestFlateFixture_CompressedTrueTypeDocument(t *testing.T) {
	// PDFTEXT_UPDATE_FIXTURE=1 rewrites the committed file from flateFixture.
	if os.Getenv("PDFTEXT_UPDATE_FIXTURE") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(flateFixturePath, flateFixture(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(flateFixturePath)
	if err != nil {
		t.Fatalf("read the committed fixture (regenerate it with PDFTEXT_UPDATE_FIXTURE=1): %v", err)
	}
	if !bytes.Contains(data, []byte("/FlateDecode")) || !bytes.Contains(data, []byte("/TrueType")) {
		t.Fatal("the fixture is not a compressed TrueType document")
	}
	pages, err := extractWithin(t, data)
	want := []string{"Quarterly report for Q4\nRevenue grew by 12%\nCosts fell 3%.", "First half\nsecond half"}
	if err != nil || !slices.Equal(pages, want) {
		t.Fatalf("Extract = %q, %v, want %q", pages, err, want)
	}
	if n, err := countWithin(t, data); err != nil || n != 2 {
		t.Fatalf("PageCount = %d, %v, want 2", n, err)
	}
}
