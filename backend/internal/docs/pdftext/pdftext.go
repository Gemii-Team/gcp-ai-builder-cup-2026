// Package pdftext reads the text layer of a PDF page by page, in pure Go. A scanned
// PDF has no text layer; Extract returns an empty string for each of its pages and
// the caller decides what that means.
package pdftext

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
)

// ErrUnreadable means the bytes are not a PDF this package can open: malformed,
// truncated, password-protected or without pages.
var ErrUnreadable = errors.New("pdftext: unreadable PDF")

// maxPages bounds a page count the file declares, which it can make up.
const maxPages = 10_000

// Extract returns the text of every page, in page order: page n is pages[n-1]. Lines
// are separated by "\n" and words by one space; a page without a text layer is "".
// Text inside form XObjects is not read.
func Extract(r io.Reader) (pages []string, err error) {
	doc, err := open(r)
	if err != nil {
		return nil, err
	}
	defer func() {
		if p := recover(); p != nil {
			pages, err = nil, fmt.Errorf("%w: %v", ErrUnreadable, p)
		}
	}()
	n, err := count(doc)
	if err != nil {
		return nil, err
	}
	pages = make([]string, n)
	for i := range pages {
		pages[i] = pageText(doc.Page(i + 1))
	}
	return pages, nil
}

// PageCount returns the number of pages the document declares.
func PageCount(r io.Reader) (n int, err error) {
	doc, err := open(r)
	if err != nil {
		return 0, err
	}
	defer func() {
		if p := recover(); p != nil {
			n, err = 0, fmt.Errorf("%w: %v", ErrUnreadable, p)
		}
	}()
	return count(doc)
}

func open(r io.Reader) (doc *pdf.Reader, err error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("pdftext: read: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			doc, err = nil, fmt.Errorf("%w: %v", ErrUnreadable, p)
		}
	}()
	doc, err = pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreadable, err)
	}
	return doc, nil
}

func count(doc *pdf.Reader) (int, error) {
	n := doc.NumPage()
	if n <= 0 || n > maxPages {
		return 0, fmt.Errorf("%w: %d pages", ErrUnreadable, n)
	}
	return n, nil
}

// matrix is the affine transform [a b 0; c d 0; e f 1] of a PDF content stream.
type matrix [6]float64

var identity = matrix{1, 0, 0, 1, 0, 0}

// mul returns m followed by n, as the PDF specification multiplies.
func (m matrix) mul(n matrix) matrix {
	return matrix{
		m[0]*n[0] + m[1]*n[2], m[0]*n[1] + m[1]*n[3],
		m[2]*n[0] + m[3]*n[2], m[2]*n[1] + m[3]*n[3],
		m[4]*n[0] + m[5]*n[2] + n[4], m[4]*n[1] + m[5]*n[3] + n[5],
	}
}

func translate(tx, ty float64) matrix { return matrix{1, 0, 0, 1, tx, ty} }

// textState is the part of the graphics state that q and Q save and restore and that
// text placement reads.
type textState struct {
	ctm        matrix
	font       *fontInfo
	size       float64
	tc, tw, th float64 // character spacing, word spacing, horizontal scale
	tl         float64 // leading
}

// extractor interprets one page's content stream and collects the text it shows.
// PDF has no words or lines, only strings placed at positions, so a gap between
// where one string ended and the next begins becomes a space and a jump across the
// baseline a line break.
type extractor struct {
	page  pdf.Page
	fonts map[string]*fontInfo
	g     textState
	stack []textState
	tm    matrix // text matrix
	tlm   matrix // text line matrix

	out   strings.Builder
	shown bool
	last  rune
	// penX and penY are where the next glyph would start had nothing moved the text
	// position since the last glyph, in device space.
	penX, penY float64
}

// pageText returns the tidy text of a page. A content stream that breaks off
// halfway, which this parser reports by panicking, keeps what was read before it.
func pageText(p pdf.Page) (text string) {
	x := &extractor{
		page: p, fonts: map[string]*fontInfo{},
		g:  textState{ctm: identity, th: 1},
		tm: identity, tlm: identity,
	}
	defer func() {
		_ = recover()
		text = tidy(x.out.String())
	}()
	if p.V.IsNull() || p.V.Key("Contents").Kind() == pdf.Null {
		return ""
	}
	pdf.Interpret(p.V.Key("Contents"), x.op)
	return ""
}

func (x *extractor) op(stk *pdf.Stack, op string) {
	args := make([]pdf.Value, stk.Len())
	for i := len(args) - 1; i >= 0; i-- {
		args[i] = stk.Pop()
	}
	switch op {
	case "q":
		x.stack = append(x.stack, x.g)
	case "Q":
		if n := len(x.stack); n > 0 {
			x.g, x.stack = x.stack[n-1], x.stack[:n-1]
		}
	case "cm":
		if m, ok := matrixOf(args); ok {
			x.g.ctm = m.mul(x.g.ctm)
		}
	case "BT":
		x.tm, x.tlm = identity, identity
	case "Tf":
		if len(args) == 2 {
			x.g.font, x.g.size = x.font(args[0].Name()), args[1].Float64()
		}
	case "Tc":
		x.g.tc = number(args, 0)
	case "Tw":
		x.g.tw = number(args, 0)
	case "Tz":
		x.g.th = number(args, 0) / 100
	case "TL":
		x.g.tl = number(args, 0)
	case "Tm":
		if m, ok := matrixOf(args); ok {
			x.tm, x.tlm = m, m
		}
	case "Td":
		x.move(number(args, 0), number(args, 1))
	case "TD":
		x.g.tl = -number(args, 1)
		x.move(number(args, 0), number(args, 1))
	case "T*":
		x.move(0, -x.g.tl)
	case "Tj":
		if len(args) == 1 {
			x.show(args[0].RawString())
		}
	case "'":
		if len(args) == 1 {
			x.move(0, -x.g.tl)
			x.show(args[0].RawString())
		}
	case `"`:
		if len(args) == 3 {
			x.g.tw, x.g.tc = args[0].Float64(), args[1].Float64()
			x.move(0, -x.g.tl)
			x.show(args[2].RawString())
		}
	case "TJ":
		if len(args) == 1 {
			x.showArray(args[0])
		}
	}
}

func number(args []pdf.Value, i int) float64 {
	if i >= len(args) {
		return 0
	}
	return args[i].Float64()
}

func matrixOf(args []pdf.Value) (matrix, bool) {
	if len(args) != 6 {
		return matrix{}, false
	}
	var m matrix
	for i, a := range args {
		m[i] = a.Float64()
	}
	return m, true
}

// move starts the next line at (tx, ty) from the start of the current one.
func (x *extractor) move(tx, ty float64) {
	x.tlm = translate(tx, ty).mul(x.tlm)
	x.tm = x.tlm
}

// showArray shows the strings of a TJ array; a number moves the text position by
// that many thousandths of the font size to the left, which is how generators put
// spaces between words.
func (x *extractor) showArray(arr pdf.Value) {
	for i := range arr.Len() {
		switch el := arr.Index(i); el.Kind() {
		case pdf.String:
			x.show(el.RawString())
		case pdf.Integer, pdf.Real:
			x.tm = translate(-el.Float64()/1000*x.g.size*x.g.th, 0).mul(x.tm)
		}
	}
}

// show writes the text of raw, a string of character codes of the current font, and
// moves the pen past it.
func (x *extractor) show(raw string) {
	f := x.g.font
	if f == nil {
		f = defaultFont
	}
	if text := f.decode(raw); text != "" {
		x.separate(text)
		x.write(text)
	}
	step := 1
	if f.composite {
		step = 2
	}
	for i := 0; i+step <= len(raw); i += step {
		code := int(raw[i])
		if step == 2 {
			code = code<<8 | int(raw[i+1])
		}
		tx := f.width(code)/1000*x.g.size + x.g.tc
		if step == 1 && code == ' ' {
			tx += x.g.tw
		}
		x.tm = translate(tx*x.g.th, 0).mul(x.tm)
	}
	m := x.tm.mul(x.g.ctm)
	x.penX, x.penY = m[4], m[5]
}

// separate puts the break between the previous text and text, which is about to be
// written, if the text position says there is one: a line break when it has moved
// across the baseline, a space when it has moved a fifth of the font size along it.
func (x *extractor) separate(text string) {
	if !x.shown {
		return
	}
	m := x.tm.mul(x.g.ctm)
	size := x.g.size * math.Hypot(m[2], m[3])
	along := math.Hypot(m[0], m[1])
	if size <= 0 || along == 0 {
		return
	}
	ux, uy := m[0]/along, m[1]/along
	dx, dy := m[4]-x.penX, m[5]-x.penY
	switch {
	case math.Abs(-dx*uy+dy*ux) > 0.4*size:
		x.breakLine()
	case dx*ux+dy*uy > 0.2*size:
		first, _ := utf8.DecodeRuneInString(text)
		// Thai runs words together, so a gap between two Thai letters is a glyph
		// placed by the generator, not a space the author typed.
		if !unicode.IsSpace(x.last) && !unicode.IsSpace(first) && (!isThai(x.last) || !isThai(first)) {
			x.out.WriteByte(' ')
			x.last = ' '
		}
	}
}

func (x *extractor) breakLine() {
	if x.last != '\n' {
		x.out.WriteByte('\n')
		x.last = '\n'
	}
}

func (x *extractor) write(text string) {
	x.out.WriteString(text)
	x.last, _ = utf8.DecodeLastRuneInString(text)
	x.shown = true
}

func isThai(r rune) bool { return r >= 0x0E00 && r <= 0x0E7F }

// tidy drops what is not text (control characters, the replacement character a
// glyph without a Unicode mapping decodes to), puts one space between words and
// removes blank lines.
func tidy(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == utf8.RuneError || (unicode.IsControl(r) && r != '\n') {
			return -1
		}
		return r
	}, s)
	lines := strings.Split(s, "\n")
	kept := lines[:0]
	for _, l := range lines {
		if l = strings.Join(strings.Fields(l), " "); l != "" {
			kept = append(kept, l)
		}
	}
	return strings.Join(kept, "\n")
}

// fontInfo is what the extractor needs of a font: how its codes decode and how far
// each moves the pen.
type fontInfo struct {
	decoder pdf.TextEncoding
	// composite fonts (Type0) use two-byte codes.
	composite bool
	width     func(code int) float64 // thousandths of the font size
}

// defaultFont stands in for a font the page does not define: Latin-1 text of average
// width.
var defaultFont = &fontInfo{width: func(int) float64 { return 500 }}

func (f *fontInfo) decode(raw string) string {
	if f.decoder != nil {
		return f.decoder.Decode(raw)
	}
	runes := make([]rune, len(raw))
	for i := range raw {
		runes[i] = rune(raw[i])
	}
	return string(runes)
}

// font returns the page's font called name, loaded once.
func (x *extractor) font(name string) *fontInfo {
	if f, ok := x.fonts[name]; ok {
		return f
	}
	pf := x.page.Font(name)
	f := defaultFont
	if !pf.V.IsNull() {
		f = loadFont(&pf)
	}
	x.fonts[name] = f
	return f
}

func loadFont(pf *pdf.Font) *fontInfo {
	f := &fontInfo{decoder: pf.Encoder()}
	if pf.V.Key("Subtype").Name() == "Type0" {
		// The widths of a composite font live on its one descendant: DW is the default
		// and W lists the exceptions as "first [w1 w2 ...]" and "first last w".
		f.composite = true
		desc := pf.V.Key("DescendantFonts").Index(0)
		def := 1000.0
		if dw := desc.Key("DW"); dw.Kind() == pdf.Integer || dw.Kind() == pdf.Real {
			def = dw.Float64()
		}
		widths := cidWidths(desc.Key("W"))
		f.width = func(code int) float64 {
			if w, ok := widths[code]; ok {
				return w
			}
			return def
		}
		return f
	}
	first := int(pf.V.Key("FirstChar").Int64())
	widths := pf.V.Key("Widths")
	missing := 500.0 // the average a font without metrics, such as a standard 14 font, gets
	if widths.Kind() == pdf.Array {
		missing = pf.V.Key("FontDescriptor").Key("MissingWidth").Float64()
	}
	f.width = func(code int) float64 {
		if i := code - first; widths.Kind() == pdf.Array && i >= 0 && i < widths.Len() {
			return widths.Index(i).Float64()
		}
		return missing
	}
	return f
}

// cidWidths reads a CIDFont W array.
func cidWidths(w pdf.Value) map[int]float64 {
	widths := map[int]float64{}
	for i := 0; i < w.Len(); {
		first := int(w.Index(i).Float64())
		switch next := w.Index(i + 1); next.Kind() {
		case pdf.Array:
			for j := range next.Len() {
				widths[first+j] = next.Index(j).Float64()
			}
			i += 2
		default:
			last, width := int(next.Float64()), w.Index(i+2).Float64()
			for c := first; c <= last && c-first < 0x10000; c++ {
				widths[c] = width
			}
			i += 3
		}
	}
	return widths
}
