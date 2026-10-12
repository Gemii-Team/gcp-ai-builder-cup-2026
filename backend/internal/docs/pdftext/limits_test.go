package pdftext

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ledongthuc/pdf"
)

// extractLimited runs the extraction with the limits changed by mod.
func extractLimited(t *testing.T, data []byte, mod func(*limits)) ([]string, error) {
	t.Helper()
	lim := defaultLimits
	mod(&lim)
	var pages []string
	var err error
	within(t, boxed, func() { pages, err = extract(context.Background(), bytes.NewReader(data), lim) })
	return pages, err
}

func TestRefIdentityIsAvailable(t *testing.T) {
	// The page tree walk dedupes by object identity, which the PDF library keeps in an
	// unexported field. If an update of the library moves it, this fails and the walk
	// is down to its node budget alone.
	data := minimalPDF("a")
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	root := r.Trailer().Key("Root")
	pages := root.Key("Pages")
	a, ok := refOf(root)
	b, ok2 := refOf(pages)
	again, ok3 := refOf(root.Key("Pages"))
	if !ok || !ok2 || !ok3 || a == b || b != again {
		t.Fatalf("refOf: catalog %v %v, pages %v %v, pages again %v %v; want distinct, stable identities", a, ok, b, ok2, again, ok3)
	}
}

func TestExtract_CallerContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pages, err := ExtractContext(ctx, bytes.NewReader(minimalPDF("a", "b")))
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrUnreadable) || pages != nil {
		t.Fatalf("ExtractContext(cancelled) = %q, %v, want the context's error and not ErrUnreadable", pages, err)
	}
	if n, err := PageCountContext(ctx, bytes.NewReader(minimalPDF("a", "b"))); !errors.Is(err, context.Canceled) || n != 0 {
		t.Fatalf("PageCountContext(cancelled) = %d, %v, want the context's error", n, err)
	}
}

func TestExtract_Limits(t *testing.T) {
	two := minimalPDF("abc", "def")
	cases := []struct {
		name  string
		data  []byte
		mod   func(*limits)
		want  []string // nil when the extraction must fail
		fails bool
	}{
		{"the time budget has run out", two, func(l *limits) { l.duration = -1 }, nil, true},
		{"the file is over the size limit", two, func(l *limits) { l.input = 100 }, nil, true},
		{"a page decodes to more than the limit", two, func(l *limits) { l.pageStream = 10 }, nil, true},
		{"the document decodes to more than the limit", two, func(l *limits) { l.decoded = 60 }, nil, true},
		{"the document holds more text than the limit", two, func(l *limits) { l.totalText = 5 }, nil, true},
		{"too many pages", two, func(l *limits) { l.pages = 1 }, nil, true},
		{"too many tree nodes", two, func(l *limits) { l.treeNodes = 2 }, nil, true},
		{"a page that has yielded all it may is cut off",
			helveticaPage("BT /F1 12 Tf 14 TL 72 720 Td (abc) Tj T* (defgh) Tj ET"),
			func(l *limits) { l.pageText = 5 }, []string{"abc"}, false},
		{"q nesting over the limit cuts the page off",
			helveticaPage("BT /F1 12 Tf 72 720 Td (a) Tj ET q q q BT /F1 12 Tf 72 700 Td (b) Tj ET"),
			func(l *limits) { l.qDepth = 2 }, []string{"a"}, false},
		{"nesting within the limit is read",
			helveticaPage("q q BT /F1 12 Tf 72 720 Td (a) Tj ET Q Q"),
			func(l *limits) { l.qDepth = 2 }, []string{"a"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pages, err := extractLimited(t, c.data, c.mod)
			if c.fails {
				if !errors.Is(err, ErrUnreadable) || pages != nil {
					t.Fatalf("extract = %q, %v, want ErrUnreadable", pages, err)
				}
				return
			}
			if err != nil || !slices.Equal(pages, c.want) {
				t.Fatalf("extract = %q, %v, want %q", pages, err, c.want)
			}
		})
	}
}

func TestExtract_DecodedBudgetSpreadOverPages(t *testing.T) {
	// Every page decodes to a megabyte, under the cap of a page; together they are over
	// the budget of the document, however small the file.
	const pages = 8
	content := stream("/Filter /FlateDecode", string(deflateRepeated("% filler\n", 1<<20)))
	objs := []string{"<< /Type /Catalog /Pages 2 0 R >>", ""}
	var kids strings.Builder
	for i := range pages {
		fmt.Fprintf(&kids, "%d 0 R ", 3+2*i)
		objs = append(objs, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents %d 0 R >>", 4+2*i), content)
	}
	objs[1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", kids.String(), pages)
	data := rawPDF(objs...)

	pages1, err := extractLimited(t, data, func(l *limits) { l.decoded = 4 << 20 })
	if !errors.Is(err, ErrUnreadable) || pages1 != nil {
		t.Fatalf("extract = %d pages, %v, want ErrUnreadable", len(pages1), err)
	}
	// Within the budget the same file reads, all of it blank.
	pages1, err = extractLimited(t, data, func(l *limits) { l.decoded = 16 << 20 })
	if err != nil || len(pages1) != pages {
		t.Fatalf("extract = %d pages, %v, want %d", len(pages1), err, pages)
	}
}
