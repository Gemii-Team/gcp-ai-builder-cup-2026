package pdftext

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// ---- fixtures ---------------------------------------------------------------

const helvetica = "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"

// buildPDF writes a PDF with the given font and image objects, numbered from 3, and
// one page per content stream; res is the /Resources dictionary every page gets. The
// Pages node declares count pages when count is not zero, whatever the page count.
func buildPDF(res string, objs []string, count int, contents ...string) []byte {
	var b bytes.Buffer
	var offsets []int
	obj := func(body string) {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
	}
	b.WriteString("%PDF-1.4\n")
	first := 3 + len(objs)
	var kids strings.Builder
	for i := range contents {
		fmt.Fprintf(&kids, "%d 0 R ", first+2*i)
	}
	if count == 0 {
		count = len(contents)
	}
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	obj(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", kids.String(), count))
	for _, o := range objs {
		obj(o)
	}
	for i, c := range contents {
		obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources << %s >> >>", first+2*i+1, res))
		obj(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(c), c))
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xref)
	return b.Bytes()
}

// minimalPDF is a valid PDF with a text layer: one Helvetica line per page.
func minimalPDF(pages ...string) []byte {
	contents := make([]string, len(pages))
	for i, text := range pages {
		contents[i] = fmt.Sprintf("BT /F1 12 Tf 72 720 Td (%s) Tj ET", strings.NewReplacer(`\`, `\\`, "(", `\(`, ")", `\)`).Replace(text))
	}
	return buildPDF("/Font << /F1 3 0 R >>", []string{helvetica}, 0, contents...)
}

// helveticaPage is a one-page PDF whose content stream is stream, in Helvetica /F1.
func helveticaPage(stream string) []byte {
	return buildPDF("/Font << /F1 3 0 R >>", []string{helvetica}, 0, stream)
}

// imageOnlyPDF is a valid one-page PDF with a picture and no text, like a scan.
func imageOnlyPDF() []byte {
	return buildPDF("/XObject << /Im0 3 0 R >>",
		[]string{"<< /Type /XObject /Subtype /Image /Width 1 /Height 1 /ColorSpace /DeviceGray /BitsPerComponent 8 /Length 1 >>\nstream\n\x80\nendstream"},
		0, "q 400 0 0 400 100 200 cm /Im0 Do Q")
}

// thaiToUnicode maps the two-byte codes the Thai test font uses to Thai letters.
const thaiToUnicode = `/CIDInit /ProcSet findresource begin
12 dict begin
begincmap
/CIDSystemInfo << /Registry (Adobe) /Ordering (UCS) /Supplement 0 >> def
/CMapName /Adobe-Identity-UCS def
/CMapType 2 def
1 begincodespacerange
<0000> <FFFF>
endcodespacerange
8 beginbfchar
<0001> <0E2A>
<0002> <0E27>
<0003> <0E31>
<0004> <0E14>
<0005> <0E35>
<0006> <0E04>
<0007> <0E23>
<0008> <0E1A>
endbfchar
endcmap
CMapName currentdict /CMap defineresource pop
end
end`

// thaiPDF is a one-page PDF in Helvetica (/F1) and a composite Thai font (/F2) that
// has no glyphs, only the ToUnicode map a text extractor needs.
func thaiPDF(stream string) []byte {
	return buildPDF("/Font << /F1 3 0 R /F2 4 0 R >>", []string{
		helvetica,
		"<< /Type /Font /Subtype /Type0 /BaseFont /Thai /Encoding /Identity-H /DescendantFonts [5 0 R] /ToUnicode 6 0 R >>",
		"<< /Type /Font /Subtype /CIDFontType2 /BaseFont /Thai /CIDSystemInfo << /Registry (Adobe) /Ordering (Identity) /Supplement 0 >> /DW 500 >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(thaiToUnicode), thaiToUnicode),
	}, 0, stream)
}

// ---- tests ------------------------------------------------------------------

func TestPDFText_ExtractsPerPage(t *testing.T) {
	want := []string{"First page.", "Second page, with (parens) and \\ a slash.", "Third page"}
	pages, err := local(minimalPDF(want...))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !slices.Equal(pages, want) {
		t.Fatalf("pages = %q, want %q", pages, want)
	}
}

func TestPDFText_PageCount(t *testing.T) {
	for _, c := range []struct {
		name string
		pdf  []byte
		want int
	}{
		{"one page", minimalPDF("a"), 1},
		{"three pages", minimalPDF("a", "b", "c"), 3},
		{"a scan", imageOnlyPDF(), 1},
	} {
		n, err := localCount(c.pdf)
		if err != nil || n != c.want {
			t.Errorf("PageCount(%s) = %d, %v, want %d", c.name, n, err, c.want)
		}
	}
}

func TestPDFText_SeparatesWordsAndLines(t *testing.T) {
	cases := []struct {
		name   string
		stream string
		want   string
	}{
		{"one string", "BT /F1 12 Tf 72 720 Td (Hello world) Tj ET", "Hello world"},
		{"strings that follow each other join", "BT /F1 12 Tf 72 720 Td (Hel) Tj (lo) Tj ET", "Hello"},
		{"a move along the line is a space", "BT /F1 12 Tf 72 720 Td (Hello) Tj 40 0 Td (world) Tj ET", "Hello world"},
		{"a small move is not", "BT /F1 12 Tf 72 720 Td (Hello) Tj 31 0 Td (world) Tj ET", "Helloworld"},
		{"a wide TJ gap is a space and a kern is not", "BT /F1 12 Tf 72 720 Td [(Hel) 20 (lo) -300 (world)] TJ ET", "Hello world"},
		{"a move down is a line break", "BT /F1 12 Tf 72 720 Td (line one) Tj 0 -14 Td (line two) Tj ET", "line one\nline two"},
		{"T* moves to the next line", "BT /F1 12 Tf 14 TL 72 720 Td (a) Tj T* (b) Tj ET", "a\nb"},
		{"the quote operator moves to the next line", "BT /F1 12 Tf 14 TL 72 720 Td (a) Tj (b) ' ET", "a\nb"},
		{"separate text objects on one line", "BT /F1 12 Tf 72 720 Td (Hello) Tj ET BT /F1 12 Tf 112 720 Td (world) Tj ET", "Hello world"},
		{"separate text objects on two lines", "BT /F1 12 Tf 72 720 Td (top) Tj ET BT /F1 12 Tf 72 700 Td (below) Tj ET", "top\nbelow"},
		{"Tm positions text", "BT /F1 12 Tf 1 0 0 1 72 720 Tm (up) Tj 1 0 0 1 72 690 Tm (down) Tj ET", "up\ndown"},
		{"a flipped page matrix changes nothing", "q 1 0 0 -1 0 792 cm BT /F1 12 Tf 72 72 Td (Hello) Tj 40 0 Td (world) Tj 0 14 Td (next) Tj ET Q", "Hello world\nnext"},
		{"a font size set by the text matrix", "BT /F1 1 Tf 12 0 0 12 72 720 Tm (Hello) Tj 12 0 0 12 112 720 Tm (world) Tj ET", "Hello world"},
		{"blank lines and runs of blanks collapse", "BT /F1 12 Tf 72 720 Td (a   b) Tj 0 -14 Td () Tj 0 -14 Td (  c ) Tj ET", "a b\nc"},
		{"escapes", `BT /F1 12 Tf 72 720 Td (a \(b\) \\ c) Tj ET`, `a (b) \ c`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pages, err := local(helveticaPage(c.stream))
			if err != nil || len(pages) != 1 {
				t.Fatalf("Extract = %q, %v", pages, err)
			}
			if pages[0] != c.want {
				t.Fatalf("text = %q, want %q", pages[0], c.want)
			}
		})
	}
}

func TestPDFText_ThaiThroughToUnicode(t *testing.T) {
	cases := []struct {
		name   string
		stream string
		want   string
	}{
		{"a word", "BT /F2 14 Tf 72 700 Td <000100020003000100040005> Tj ET", "สวัสดี"},
		// A generator that places each run of a Thai sentence itself leaves gaps that are
		// not spaces: Thai has none between words.
		{"runs placed apart stay joined", "BT /F2 14 Tf 72 700 Td <000100020003000100040005> Tj 100 0 Td <0006000700030008> Tj ET", "สวัสดีครับ"},
		{"a kern between marks stays joined", "BT /F2 14 Tf 72 700 Td [<00010002> 15 <0003> 20 <0001> 10 <00040005>] TJ ET", "สวัสดี"},
		{"a Latin word beside Thai is set off", "BT /F2 14 Tf 72 700 Td <0006000700030008> Tj /F1 14 Tf 60 0 Td (ok) Tj ET", "ครับ ok"},
		{"Thai then a new line", "BT /F2 14 Tf 72 700 Td <00010002> Tj 0 -20 Td <00060007> Tj ET", "สว\nคร"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pages, err := local(thaiPDF(c.stream))
			if err != nil || len(pages) != 1 {
				t.Fatalf("Extract = %q, %v", pages, err)
			}
			if pages[0] != c.want {
				t.Fatalf("text = %q, want %q", pages[0], c.want)
			}
		})
	}
}

func TestPDFText_ScannedPDFHasNoText(t *testing.T) {
	pages, err := local(imageOnlyPDF())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !slices.Equal(pages, []string{""}) {
		t.Fatalf("pages = %q, want one empty page", pages)
	}
}

func TestPDFText_UnreadableInput(t *testing.T) {
	good := minimalPDF("one", "two")
	cases := []struct {
		name string
		data []byte
	}{
		{"nothing", nil},
		{"text", []byte("this is not a pdf")},
		{"a header only", []byte("%PDF-1.4\n")},
		{"cut off", good[:len(good)/2]},
		{"a catalog without pages", rawPDF("<< /Type /Catalog >>")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if pages, err := local(c.data); !errors.Is(err, ErrUnreadable) || pages != nil {
				t.Fatalf("Extract = %q, %v, want ErrUnreadable", pages, err)
			}
			if n, err := localCount(c.data); !errors.Is(err, ErrUnreadable) || n != 0 {
				t.Fatalf("PageCount = %d, %v, want ErrUnreadable", n, err)
			}
		})
	}
}

func TestPDFText_BrokenContentKeepsWhatWasRead(t *testing.T) {
	for name, content := range map[string]string{
		"an unterminated string":      "BT /F1 12 Tf 72 720 Td (kept) Tj ET (never ends",
		"an unterminated array":       "BT /F1 12 Tf 72 720 Td (kept) Tj ET [(a) 5",
		"an unterminated hex":         "BT /F1 12 Tf 72 720 Td (kept) Tj ET <4865",
		"an unterminated dict":        "BT /F1 12 Tf 72 720 Td (kept) Tj ET << /A (b)",
		"a stray delimiter run":       "BT /F1 12 Tf 72 720 Td (kept) Tj ET ) > ] } >> ]]]",
		"an inline image with no end": "BT /F1 12 Tf 72 720 Td (kept) Tj ET BI /W 1 ID \x00\x01\x02",
	} {
		t.Run(name, func(t *testing.T) {
			pages, err := local(helveticaPage(content))
			if err != nil || !slices.Equal(pages, []string{"kept"}) {
				t.Fatalf("Extract = %q, %v, want the text before the break", pages, err)
			}
		})
	}
}

func TestPDFText_PageWithAFilterItCannotDecodeIsBlank(t *testing.T) {
	// LZW is not supported: the page is blank and the others are still read.
	pdf := rawPDF(
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R 5 0 R] /Count 2 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R "+fontRes(7)+" >>",
		stream("/Filter /LZWDecode", "garbage"),
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 6 0 R "+fontRes(7)+" >>",
		stream("", show("second")),
		helveticaObj,
	)
	pages, err := extractWithin(t, pdf)
	if err != nil || !slices.Equal(pages, []string{"", "second"}) {
		t.Fatalf("Extract = %q, %v, want a blank first page and the second", pages, err)
	}
}

func TestPDFText_ContentSyntax(t *testing.T) {
	cases := []struct {
		name, content, want string
	}{
		{"octal and escaped characters", `(a\101\\\)b) Tj`, `aA\)b`},
		{"nested parentheses", `(a (b) c) Tj`, `a (b) c`},
		{"a line continuation", "(ab\\\ncd) Tj", "abcd"},
		{"a hex string", `<48656C6C6F> Tj`, "Hello"},
		{"a hex string with blanks and an odd digit", `<4865 6C6C 6F7> Tj`, "Hellop"},
		{"signed and fractional numbers", "+72 .5 Td (a) Tj -.5 -14.5 Td (b) Tj", "a\nb"},
		{"an operator without its operands does nothing", "Tj Td (ok) Tj", "ok"},
		{"a number is not a string", "5 Tj (ok) Tj", "ok"},
		{"names in an array are skipped", "[(a) /X (b) << /Y (no) >> [(nested)]] TJ", "ab"},
		{"a comment", "% (hidden) Tj\n(shown) Tj", "shown"},
		{"a marked-content dictionary", "/Span << /MCID 0 /Alt (hidden) >> BDC (shown) Tj EMC", "shown"},
		{"an inline image is skipped", "BI /W 2 /H 1 /CS /G /BPC 8 ID ab(Tj)(oops) Tj\nEI (after) Tj", "after"},
		{"more operands than any operator takes", strings.Repeat("1 ", 200) + "(x) Tj", "x"},
		{"a name with an escape", "/F#31 12 Tf (ok) Tj", "ok"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pages, err := local(helveticaPage("BT /F1 12 Tf 72 720 Td " + c.content + " ET"))
			if err != nil || !slices.Equal(pages, []string{c.want}) {
				t.Fatalf("Extract = %q, %v, want %q", pages, err, c.want)
			}
		})
	}
}
