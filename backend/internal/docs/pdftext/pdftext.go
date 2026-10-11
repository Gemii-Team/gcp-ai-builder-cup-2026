// Package pdftext reads the text layer of a PDF page by page, in pure Go. A scanned
// PDF has no text layer; Extract returns an empty string for each of its pages and
// the caller decides what that means.
//
// The input is hostile: any workspace member can upload a PDF, and the program that
// reads it is the one that also serves the API. Two layers keep a crafted file from
// taking it down.
//
// The PDF is read in a child process (see sandbox.go): the binary runs itself again
// with an environment marker, which an init function of this package answers, under
// CPU and memory limits and a wall-clock deadline, with an empty environment, and the
// parent keeps only the text that comes back. The PDF library can recurse without end,
// loop or allocate without bound inside pdf.NewReader and a lookup, which no check
// from outside can stop and a panic handler cannot recover from; a child that dies of
// it costs the file, as ErrUnreadable, and nothing else.
//
// Inside the child, what the extraction reads is bounded as well (see limits): the page
// tree is walked iteratively with a visited set, a depth cap and a node budget instead
// of trusting /Count and /Parent; the decoded content of a page, of all pages together
// and of a font's ToUnicode map is capped, so a few kilobytes of Deflate cannot expand
// without limit; the content stream is read by a tokenizer of its own with a
// fixed-size operand stack and a cap on q nesting and on the text a page may yield; and
// the whole extraction has a wall-clock budget and obeys its context. Whatever is over
// a limit is ErrUnreadable.
package pdftext

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
)

// ErrUnreadable means the bytes are not a PDF this package can read: malformed,
// truncated, password-protected, without pages, or over one of its limits.
var ErrUnreadable = errors.New("pdftext: unreadable PDF")

// limits bound what one extraction may read, hold and take time for.
type limits struct {
	input      int64 // bytes of the PDF itself
	pages      int   // pages of one document
	treeNodes  int   // page tree nodes visited, and the kids one node may list
	treeDepth  int   // levels of the page tree
	parents    int   // levels of /Parent followed to find /Resources
	pageStream int64 // decoded bytes of the content of one page
	toUnicode  int64 // decoded bytes of one ToUnicode map
	decoded    int64 // decoded bytes of every stream read, in all
	qDepth     int   // nesting of q
	pageText   int   // bytes of text of one page
	totalText  int   // bytes of text of the document
	duration   time.Duration
}

var defaultLimits = limits{
	input:      64 << 20,
	pages:      10_000,
	treeNodes:  40_000,
	treeDepth:  32,
	parents:    32,
	pageStream: 16 << 20,
	toUnicode:  4 << 20,
	decoded:    128 << 20,
	qDepth:     64,
	pageText:   2 << 20,
	totalText:  16 << 20,
	duration:   20 * time.Second,
}

const (
	// maxOperands is the operand stack of the content tokenizer; real operators take
	// six at most.
	maxOperands = 64
	// maxArrayItems caps the elements kept of one array operand.
	maxArrayItems = 1 << 14
	// maxDecodeChunk is the longest string handed to a font in one piece, so a huge
	// string cannot hold the extraction past its deadline inside one decode.
	maxDecodeChunk = 4096
	// maxWidthWork bounds the work spent reading one font's width table.
	maxWidthWork = 1 << 20
	// checkEvery is how many operators pass between looks at the context.
	checkEvery = 256
)

// extract is the extraction itself, in this process: the child process of the sandbox
// runs it, and the tests of its limits call it directly. Nothing else may, because
// the PDF library it relies on can crash or hang the process on a crafted file.
func extract(parent context.Context, r io.Reader, lim limits) (texts []string, err error) {
	d, finish, err := open(parent, r, lim)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			texts = nil
		}
	}()
	defer finish(&err)
	pages := d.pages()
	texts = make([]string, len(pages))
	for i, p := range pages {
		d.alive()
		texts[i] = d.pageText(p)
		if d.text += len(texts[i]); d.text > lim.totalText {
			d.stop(limitError("the document holds more text than the limit"))
		}
	}
	return texts, nil
}

func pageCount(parent context.Context, r io.Reader, lim limits) (n int, err error) {
	d, finish, err := open(parent, r, lim)
	if err != nil {
		return 0, err
	}
	defer finish(&err)
	return len(d.pages()), nil
}

// fatal is the panic that ends an extraction; the deferred finish of extract and
// pageCount turns it into the error it carries.
type fatal struct{ err error }

// truncate is the panic that ends the interpretation of one page, which then keeps
// the text read so far.
type truncate struct{}

func limitError(what string) error { return fmt.Errorf("%w: %s", ErrUnreadable, what) }

// doc is one extraction in progress.
type doc struct {
	ctx    context.Context // with the time budget
	parent context.Context // the caller's
	lim    limits
	r      *pdf.Reader

	decoded int64 // bytes decoded so far
	text    int   // bytes of text so far
	fonts   map[ref]*fontInfo
}

// open reads the PDF and returns the extraction and the function that ends it: finish
// is deferred with the address of the named error result, cancels the time budget and
// turns what the extraction panicked with into that error. A panic that is not ours
// comes from the PDF parser on malformed input and is ErrUnreadable.
func open(parent context.Context, r io.Reader, lim limits) (d *doc, finish func(*error), err error) {
	ctx, cancel := context.WithTimeout(parent, lim.duration)
	fail := func(err error) (*doc, func(*error), error) {
		cancel()
		return nil, nil, err
	}
	data, err := io.ReadAll(io.LimitReader(r, lim.input+1))
	if err != nil {
		return fail(fmt.Errorf("pdftext: read: %w", err))
	}
	if int64(len(data)) > lim.input {
		return fail(limitError("the file is over the size limit"))
	}
	d = &doc{ctx: ctx, parent: parent, lim: lim, fonts: map[ref]*fontInfo{}}
	if err := d.parse(data); err != nil {
		cancel()
		return nil, nil, err
	}
	return d, func(err *error) {
		cancel()
		switch p := recover().(type) {
		case nil:
		case fatal:
			*err = p.err
		default:
			*err = fmt.Errorf("%w: %v", ErrUnreadable, p)
		}
	}, nil
}

// parse opens the PDF. The parser panics on malformed input.
func (d *doc) parse(data []byte) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("%w: %v", ErrUnreadable, p)
		}
	}()
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnreadable, err)
	}
	d.r = r
	return nil
}

// stop ends the extraction with err.
func (d *doc) stop(err error) { panic(fatal{err}) }

// alive ends the extraction when its context is done: with the caller's error when the
// caller gave up, which is not the file's fault, and as unreadable when the time budget
// ran out.
func (d *doc) alive() {
	if d.ctx.Err() == nil {
		return
	}
	if err := d.parent.Err(); err != nil {
		d.stop(fmt.Errorf("pdftext: %w", err))
	}
	d.stop(limitError("reading it took too long"))
}

// ref identifies an indirect object.
type ref struct{ id, gen uint64 }

// refOf returns the indirect object v was loaded from. The PDF library keeps it in an
// unexported field, which reflect can read; a library without that field has no
// identity to offer, and the callers then rely on their other limits alone
// (TestRefIdentityIsAvailable fails in that case).
func refOf(v pdf.Value) (ref, bool) {
	f := reflect.ValueOf(v).FieldByName("ptr")
	if !f.IsValid() || f.Kind() != reflect.Struct || f.NumField() != 2 {
		return ref{}, false
	}
	id, gen := f.Field(0), f.Field(1)
	if !id.CanUint() || !gen.CanUint() {
		return ref{}, false
	}
	return ref{id.Uint(), gen.Uint()}, true
}

// pages returns the page objects in order, found by walking the page tree from the
// catalog. The walk is iterative; it visits an object once, goes no deeper than the
// limit and gives up after the node budget, so a tree that refers to itself, or one
// that claims a thousand million pages, ends it. /Count is not consulted.
func (d *doc) pages() []pdf.Value {
	type node struct {
		v     pdf.Value
		depth int
	}
	stack := []node{{d.r.Trailer().Key("Root").Key("Pages"), 0}}
	seen := map[ref]bool{}
	var pages []pdf.Value
	for visits := 0; len(stack) > 0; visits++ {
		if visits >= d.lim.treeNodes {
			d.stop(limitError("the page tree has too many nodes"))
		}
		if visits%checkEvery == 0 {
			d.alive()
		}
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		// An object is read once, whichever way it is reached. A kid written inside its
		// parent instead of being an indirect reference, which the specification forbids,
		// carries its parent's identity and is skipped with it.
		if key, ok := refOf(n.v); ok {
			if seen[key] {
				continue
			}
			seen[key] = true
		}

		if kids := n.v.Key("Kids"); kids.Kind() == pdf.Array {
			if n.depth >= d.lim.treeDepth {
				d.stop(limitError("the page tree is too deep"))
			}
			count := kids.Len()
			if count > d.lim.treeNodes {
				d.stop(limitError("a page tree node lists too many pages"))
			}
			for i := count - 1; i >= 0; i-- {
				stack = append(stack, node{kids.Index(i), n.depth + 1})
			}
			continue
		}
		if n.v.Key("Type").Name() == "Page" {
			if len(pages) >= d.lim.pages {
				d.stop(limitError("the document has too many pages"))
			}
			pages = append(pages, n.v)
		}
	}
	if len(pages) == 0 {
		d.stop(limitError("the document has no pages"))
	}
	return pages
}

// resources returns the /Resources of a page, which it may inherit from its ancestors.
// A /Parent chain that loops or runs on is cut off.
func (d *doc) resources(page pdf.Value) pdf.Value {
	for v, i := page, 0; !v.IsNull() && i < d.lim.parents; i++ {
		if r := v.Key("Resources"); !r.IsNull() {
			return r
		}
		v = v.Key("Parent")
	}
	return pdf.Value{}
}

// ctxReader fails the extraction when its context is done, between two reads.
type ctxReader struct {
	d *doc
	r io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	c.d.alive()
	return c.r.Read(p)
}

// read returns the decoded bytes of stream v. It reads at most limit bytes and ends the
// extraction if the stream has more, or if everything decoded so far has passed the
// budget: the compressed size says nothing about the decoded one. A stream that is
// damaged yields the part that decoded.
func (d *doc) read(v pdf.Value, limit int64, what string) []byte {
	rc := v.Reader()
	defer func() { _ = rc.Close() }()
	data, _ := io.ReadAll(io.LimitReader(ctxReader{d, rc}, limit+1))
	if int64(len(data)) > limit {
		d.stop(limitError(what + " is larger than the limit"))
	}
	if d.decoded += int64(len(data)); d.decoded > d.lim.decoded {
		d.stop(limitError("the document decodes to more than the limit"))
	}
	return data
}

// content returns the decoded content of a page; /Contents is a stream or an array of
// streams, which read as one.
func (d *doc) content(page pdf.Value) []byte {
	c := page.Key("Contents")
	switch c.Kind() {
	case pdf.Stream:
		return d.read(c, d.lim.pageStream, "the content of a page")
	case pdf.Array:
		var all []byte
		for i := range c.Len() {
			s := c.Index(i)
			if s.Kind() != pdf.Stream {
				continue
			}
			all = append(all, d.read(s, d.lim.pageStream-int64(len(all)), "the content of a page")...)
			all = append(all, '\n')
		}
		return all
	}
	return nil
}

// pageText returns the tidy text of a page. A page that cannot be read to the end, which
// the parser reports by panicking, keeps what was read before it.
func (d *doc) pageText(page pdf.Value) (text string) {
	x := &extractor{d: d, fonts: map[string]*fontInfo{}, g: textState{ctm: identity, th: 1}, tm: identity, tlm: identity}
	defer func() {
		if p := recover(); p != nil {
			if f, ok := p.(fatal); ok {
				panic(f)
			}
			text = tidy(x.out.String())
		}
	}()
	x.res = d.resources(page)
	x.run(d.content(page))
	return tidy(x.out.String())
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
	d     *doc
	res   pdf.Value
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

	ops int
}

// run interprets a content stream: it reads operands into a stack of fixed size and
// runs each operator on them.
func (x *extractor) run(content []byte) {
	sc := &scanner{b: content}
	args := make([]operand, 0, maxOperands)
	for {
		t := sc.next()
		switch t.kind {
		case tokEOF:
			return
		case tokNumber:
			args = push(args, operand{kind: kindNumber, num: t.num})
		case tokName:
			args = push(args, operand{kind: kindName, s: t.s})
		case tokString:
			args = push(args, operand{kind: kindString, s: t.s})
		case tokArrayOpen:
			args = push(args, operand{kind: kindArray, items: sc.array()})
		case tokDictOpen:
			sc.skipDict()
			args = push(args, operand{kind: kindOther})
		case tokOp:
			if x.ops++; x.ops%checkEvery == 0 {
				x.d.alive()
			}
			x.exec(t.s, args)
			args = args[:0]
			if t.s == "ID" {
				sc.skipInlineImage()
			}
		}
	}
}

// push adds an operand; a stream that piles up more than any operator takes starts
// over, which only a hostile one does.
func push(args []operand, o operand) []operand {
	if len(args) == maxOperands {
		args = args[:0]
	}
	return append(args, o)
}

// last returns the final n operands, or nil when there are fewer.
func last(args []operand, n int) []operand {
	if len(args) < n {
		return nil
	}
	return args[len(args)-n:]
}

func (x *extractor) exec(op string, args []operand) {
	switch op {
	case "q":
		if len(x.stack) >= x.d.lim.qDepth {
			panic(truncate{})
		}
		x.stack = append(x.stack, x.g)
	case "Q":
		if n := len(x.stack); n > 0 {
			x.g, x.stack = x.stack[n-1], x.stack[:n-1]
		}
	case "cm":
		if a := last(args, 6); a != nil {
			x.g.ctm = matrixOf(a).mul(x.g.ctm)
		}
	case "BT":
		x.tm, x.tlm = identity, identity
	case "Tf":
		if a := last(args, 2); a != nil {
			x.g.font, x.g.size = x.font(a[0].s), a[1].num
		}
	case "Tc":
		if a := last(args, 1); a != nil {
			x.g.tc = a[0].num
		}
	case "Tw":
		if a := last(args, 1); a != nil {
			x.g.tw = a[0].num
		}
	case "Tz":
		if a := last(args, 1); a != nil {
			x.g.th = a[0].num / 100
		}
	case "TL":
		if a := last(args, 1); a != nil {
			x.g.tl = a[0].num
		}
	case "Tm":
		if a := last(args, 6); a != nil {
			x.tm = matrixOf(a)
			x.tlm = x.tm
		}
	case "Td":
		if a := last(args, 2); a != nil {
			x.move(a[0].num, a[1].num)
		}
	case "TD":
		if a := last(args, 2); a != nil {
			x.g.tl = -a[1].num
			x.move(a[0].num, a[1].num)
		}
	case "T*":
		x.move(0, -x.g.tl)
	case "Tj":
		if a := last(args, 1); a != nil && a[0].kind == kindString {
			x.show(a[0].s)
		}
	case "'":
		if a := last(args, 1); a != nil && a[0].kind == kindString {
			x.move(0, -x.g.tl)
			x.show(a[0].s)
		}
	case `"`:
		if a := last(args, 3); a != nil && a[2].kind == kindString {
			x.g.tw, x.g.tc = a[0].num, a[1].num
			x.move(0, -x.g.tl)
			x.show(a[2].s)
		}
	case "TJ":
		if a := last(args, 1); a != nil && a[0].kind == kindArray {
			x.showArray(a[0].items)
		}
	}
}

func matrixOf(a []operand) matrix {
	var m matrix
	for i := range m {
		m[i] = a[i].num
	}
	return m
}

// move starts the next line at (tx, ty) from the start of the current one.
func (x *extractor) move(tx, ty float64) {
	x.tlm = translate(tx, ty).mul(x.tlm)
	x.tm = x.tlm
}

// showArray shows the strings of a TJ array; a number moves the text position by
// that many thousandths of the font size to the left, which is how generators put
// spaces between words.
func (x *extractor) showArray(items []item) {
	for _, it := range items {
		if it.isString {
			x.show(it.s)
			continue
		}
		x.tm = translate(-it.num/1000*x.g.size*x.g.th, 0).mul(x.tm)
	}
}

// show writes the text of raw, a string of character codes of the current font, and
// moves the pen past it.
func (x *extractor) show(raw string) {
	f := x.g.font
	if f == nil {
		f = defaultFont
	}
	for len(raw) > maxDecodeChunk {
		x.showChunk(f, raw[:maxDecodeChunk])
		raw = raw[maxDecodeChunk:]
		x.d.alive()
	}
	x.showChunk(f, raw)
}

func (x *extractor) showChunk(f *fontInfo, raw string) {
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

// write adds text to the page; a page that has yielded all it may is cut off there.
func (x *extractor) write(text string) {
	if x.out.Len()+len(text) > x.d.lim.pageText {
		panic(truncate{})
	}
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

// ---- content stream tokens --------------------------------------------------

type tokenKind uint8

const (
	tokEOF tokenKind = iota
	tokNumber
	tokName
	tokString
	tokArrayOpen
	tokArrayClose
	tokDictOpen
	tokDictClose
	tokOp
	tokIgnored // a delimiter that carries nothing the extraction needs
)

type token struct {
	kind tokenKind
	num  float64
	s    string
}

type operandKind uint8

const (
	kindOther operandKind = iota
	kindNumber
	kindName
	kindString
	kindArray
)

// operand is a value an operator takes. Only the kinds the text operators read are
// kept; a dictionary or a nested array is an operand without content.
type operand struct {
	kind  operandKind
	num   float64
	s     string
	items []item
}

// item is an element of a TJ array: a string or an adjustment.
type item struct {
	isString bool
	num      float64
	s        string
}

// scanner reads the tokens of a content stream from memory.
type scanner struct {
	b []byte
	i int
}

func isSpace(c byte) bool {
	switch c {
	case 0, '\t', '\n', '\f', '\r', ' ':
		return true
	}
	return false
}

func isDelim(c byte) bool {
	switch c {
	case '(', ')', '<', '>', '[', ']', '{', '}', '/', '%':
		return true
	}
	return false
}

// next returns the next token. Bytes it cannot place are skipped.
func (sc *scanner) next() token {
	for sc.i < len(sc.b) {
		c := sc.b[sc.i]
		switch {
		case isSpace(c):
			sc.i++
		case c == '%':
			for sc.i < len(sc.b) && sc.b[sc.i] != '\n' && sc.b[sc.i] != '\r' {
				sc.i++
			}
		case c == '/':
			sc.i++
			return token{kind: tokName, s: sc.name()}
		case c == '(':
			sc.i++
			return token{kind: tokString, s: sc.literal()}
		case c == '<':
			if sc.i+1 < len(sc.b) && sc.b[sc.i+1] == '<' {
				sc.i += 2
				return token{kind: tokDictOpen}
			}
			sc.i++
			return token{kind: tokString, s: sc.hex()}
		case c == '[':
			sc.i++
			return token{kind: tokArrayOpen}
		case c == ']':
			sc.i++
			return token{kind: tokArrayClose}
		case c == '>' && sc.i+1 < len(sc.b) && sc.b[sc.i+1] == '>':
			sc.i += 2
			return token{kind: tokDictClose}
		case isDelim(c): // ) > { }
			sc.i++
			return token{kind: tokIgnored}
		default:
			return sc.word()
		}
	}
	return token{kind: tokEOF}
}

// word reads a number or an operator.
func (sc *scanner) word() token {
	start := sc.i
	for sc.i < len(sc.b) && !isSpace(sc.b[sc.i]) && !isDelim(sc.b[sc.i]) {
		sc.i++
	}
	w := sc.b[start:sc.i]
	if c := w[0]; c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.' {
		if n, err := strconv.ParseFloat(string(w), 64); err == nil && !math.IsInf(n, 0) {
			return token{kind: tokNumber, num: n}
		}
	}
	return token{kind: tokOp, s: string(w)}
}

// name reads a name after its slash, with its #xx escapes.
func (sc *scanner) name() string {
	var out []byte
	for sc.i < len(sc.b) && !isSpace(sc.b[sc.i]) && !isDelim(sc.b[sc.i]) {
		c := sc.b[sc.i]
		sc.i++
		if c == '#' && sc.i+1 < len(sc.b) {
			if h, l := unhex(sc.b[sc.i]), unhex(sc.b[sc.i+1]); h >= 0 && l >= 0 {
				c = byte(h<<4 | l)
				sc.i += 2
			}
		}
		out = append(out, c)
	}
	return string(out)
}

func unhex(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// literal reads a string after its opening parenthesis: nested parentheses balance and
// a backslash escapes.
func (sc *scanner) literal() string {
	var out []byte
	for depth := 1; sc.i < len(sc.b); {
		c := sc.b[sc.i]
		sc.i++
		switch c {
		case '(':
			depth++
		case ')':
			if depth--; depth == 0 {
				return string(out)
			}
		case '\\':
			if sc.i >= len(sc.b) {
				return string(out)
			}
			e := sc.b[sc.i]
			sc.i++
			switch {
			case e == 'n':
				c = '\n'
			case e == 'r':
				c = '\r'
			case e == 't':
				c = '\t'
			case e == 'b':
				c = '\b'
			case e == 'f':
				c = '\f'
			case e == '\r': // a line continuation
				if sc.i < len(sc.b) && sc.b[sc.i] == '\n' {
					sc.i++
				}
				continue
			case e == '\n':
				continue
			case e >= '0' && e <= '7':
				v := int(e - '0')
				for k := 0; k < 2 && sc.i < len(sc.b) && sc.b[sc.i] >= '0' && sc.b[sc.i] <= '7'; k++ {
					v = v*8 + int(sc.b[sc.i]-'0')
					sc.i++
				}
				c = byte(v)
			default:
				c = e
			}
		}
		out = append(out, c)
	}
	return string(out)
}

// hex reads a hexadecimal string after its opening angle bracket.
func (sc *scanner) hex() string {
	var out []byte
	high := -1
	for sc.i < len(sc.b) {
		c := sc.b[sc.i]
		sc.i++
		if c == '>' {
			break
		}
		d := unhex(c)
		if d < 0 {
			continue
		}
		if high < 0 {
			high = d
			continue
		}
		out = append(out, byte(high<<4|d))
		high = -1
	}
	if high >= 0 {
		out = append(out, byte(high<<4))
	}
	return string(out)
}

// array reads the elements of an array after its opening bracket: the strings and
// numbers of its top level, up to a cap. Arrays and dictionaries inside it are skipped.
func (sc *scanner) array() []item {
	var items []item
	depth := 0
	for {
		t := sc.next()
		switch t.kind {
		case tokEOF:
			return items
		case tokDictOpen:
			sc.skipDict()
		case tokArrayOpen:
			depth++
		case tokArrayClose:
			if depth == 0 {
				return items
			}
			depth--
		case tokNumber:
			if depth == 0 && len(items) < maxArrayItems {
				items = append(items, item{num: t.num})
			}
		case tokString:
			if depth == 0 && len(items) < maxArrayItems {
				items = append(items, item{isString: true, s: t.s})
			}
		}
	}
}

// skipDict skips to the end of the dictionary whose opening has been read.
func (sc *scanner) skipDict() {
	for depth := 1; depth > 0; {
		t := sc.next()
		switch t.kind {
		case tokEOF:
			return
		case tokDictOpen:
			depth++
		case tokDictClose:
			depth--
		}
	}
}

// skipInlineImage skips the binary data after an ID operator, up to and including the
// EI that ends it, or the rest of the stream.
func (sc *scanner) skipInlineImage() {
	for j := sc.i; j+1 < len(sc.b); j++ {
		if sc.b[j] == 'E' && sc.b[j+1] == 'I' && (j == 0 || isSpace(sc.b[j-1])) &&
			(j+2 == len(sc.b) || isSpace(sc.b[j+2]) || isDelim(sc.b[j+2])) {
			sc.i = j + 2
			return
		}
	}
	sc.i = len(sc.b)
}

// ---- fonts ------------------------------------------------------------------

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

// font returns the page's font called name, loaded once for the page and, when the
// font is an object of its own, once for the document.
func (x *extractor) font(name string) *fontInfo {
	if f, ok := x.fonts[name]; ok {
		return f
	}
	dict := x.res.Key("Font")
	fv := dict.Key(name)
	f := defaultFont
	if !fv.IsNull() {
		key, ok := refOf(fv)
		// A font written inside the font dictionary carries the identity of the
		// dictionary, which says nothing about it.
		if container, has := refOf(dict); has && ok && container == key {
			ok = false
		}
		if ok && x.d.fonts[key] != nil {
			f = x.d.fonts[key]
		} else {
			f = x.d.loadFont(fv)
			if ok {
				x.d.fonts[key] = f
			}
		}
	}
	x.fonts[name] = f
	return f
}

// loadFont reads a font. One that cannot be read is the default font: its page is
// still worth reading.
func (d *doc) loadFont(fv pdf.Value) (f *fontInfo) {
	defer func() {
		if p := recover(); p != nil {
			if _, ok := p.(fatal); ok {
				panic(p)
			}
			f = defaultFont
		}
	}()
	// The library parses a ToUnicode map in full and looks each code up in it in turn;
	// it is measured first, so that the map cannot be a bomb.
	if tu := fv.Key("ToUnicode"); tu.Kind() == pdf.Stream {
		d.read(tu, d.lim.toUnicode, "a ToUnicode map")
	}
	pf := pdf.Font{V: fv}
	f = &fontInfo{decoder: pf.Encoder()}
	if fv.Key("Subtype").Name() == "Type0" {
		// The widths of a composite font live on its one descendant: DW is the default
		// and W lists the exceptions as "first [w1 w2 ...]" and "first last w".
		f.composite = true
		desc := fv.Key("DescendantFonts").Index(0)
		def := 1000.0
		if dw := desc.Key("DW"); dw.Kind() == pdf.Integer || dw.Kind() == pdf.Real {
			def = dw.Float64()
		}
		widths := d.cidWidths(desc.Key("W"))
		f.width = func(code int) float64 {
			if w, ok := widths[code]; ok {
				return w
			}
			return def
		}
		return f
	}
	first := int(fv.Key("FirstChar").Int64())
	wv := fv.Key("Widths")
	missing := 500.0 // the average a font without metrics, such as a standard 14 font, gets
	var widths []float64
	if wv.Kind() == pdf.Array {
		missing = fv.Key("FontDescriptor").Key("MissingWidth").Float64()
		widths = make([]float64, min(wv.Len(), 256))
		for i := range widths {
			widths[i] = wv.Index(i).Float64()
		}
	}
	f.width = func(code int) float64 {
		if i := code - first; widths != nil && i >= 0 && i < len(widths) {
			return widths[i]
		}
		return missing
	}
	return f
}

// cidWidths reads a CIDFont W array. The work is bounded: an array that names the same
// range a million times is read no further than a legitimate one would be.
func (d *doc) cidWidths(w pdf.Value) map[int]float64 {
	widths := map[int]float64{}
	work := 0
	for i := 0; i < w.Len() && work < maxWidthWork; {
		d.alive()
		first := int(w.Index(i).Float64())
		switch next := w.Index(i + 1); next.Kind() {
		case pdf.Array:
			for j := 0; j < next.Len() && work < maxWidthWork; j++ {
				work++
				if c := first + j; c >= 0 && c <= 0xFFFF {
					widths[c] = next.Index(j).Float64()
				}
			}
			i += 2
		default:
			last, width := int(next.Float64()), w.Index(i+2).Float64()
			for c := max(first, 0); c <= min(last, 0xFFFF) && work < maxWidthWork; c++ {
				work++
				widths[c] = width
			}
			i += 3
		}
	}
	return widths
}
