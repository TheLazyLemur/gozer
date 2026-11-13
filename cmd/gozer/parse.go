package main

import (
	"fmt"
	"strconv"
	"strings"
)

// goSrc is Go code taken verbatim from the template; pos is its byte offset there.
type goSrc struct {
	code string
	pos  int
}

// Template AST.
type (
	node  any
	textN struct{ s string }
	rawN  struct{ s string } // <script>/<style> body
	exprN struct{ goSrc }
	codeN struct{ goSrc }
	ifN   struct {
		conds  []goSrc // pos < 0 is the else branch
		bodies [][]node
	}
	forN struct {
		clause goSrc
		body   []node
	}
	elemN struct {
		tag         string
		tagPos      int
		start, head int // offsets of '<' and just past the start tag
		attrs       []attrN
		kids        []node
		line        int
	}
	attrN struct {
		name    string
		namePos int
		raw     string
		rawPos  int
		has     bool
	}
	part struct {
		text string
		expr goSrc
	}
	// mark is template syntax for highlighting: '@', block keywords, @* comments *@
	mark struct {
		pos, n  int
		comment bool
	}
)

type file struct {
	name     string
	pages    []string
	layout   goSrc
	inherits goSrc
	imports  []goSrc
	code     []goSrc
	injects  []goSrc // "Name Type"
	params   []goSrc
	body     []node
	marks    []mark
}

type parseError struct {
	file      string
	pos       int
	line, col int
	msg       string
}

func (e *parseError) Error() string { return fmt.Sprintf("%s:%d:%d: %s", e.file, e.line, e.col, e.msg) }

type tparser struct {
	src, path string
	pos       int
	f         *file
	// editor mode: recover from half-typed markup instead of stopping
	tolerant bool
	errs     []*parseError
	dotAt    int // offset of a '.' that ends an implicit expression (completion after "@c.")
}

var voidTags = map[string]bool{"area": true, "base": true, "br": true, "col": true, "embed": true,
	"hr": true, "img": true, "input": true, "link": true, "meta": true, "source": true, "track": true, "wbr": true}

func parse(path, name, src string) (*file, error) {
	f, errs := parseWith(&tparser{src: src, path: path, f: &file{name: name}, dotAt: -1})
	if len(errs) > 0 {
		return nil, errs[0]
	}
	return f, nil
}

// parseTolerant parses for the editor. f is nil when recovery was impossible.
func parseTolerant(path, name, src string, dotAt int) (*file, []*parseError) {
	return parseWith(&tparser{src: src, path: path, f: &file{name: name}, tolerant: true, dotAt: dotAt})
}

func parseWith(p *tparser) (f *file, errs []*parseError) {
	defer func() {
		if r := recover(); r != nil {
			pe, ok := r.(*parseError)
			if !ok {
				panic(r)
			}
			f, errs = nil, append(p.errs, pe)
		}
	}()
	p.f.body = p.nodes("", false, true)
	return p.f, p.errs
}

func (p *tparser) errAt(pos int, format string, args ...any) *parseError {
	pos = min(pos, len(p.src))
	line := 1 + strings.Count(p.src[:pos], "\n")
	col := pos - strings.LastIndex(p.src[:pos], "\n")
	return &parseError{p.path, pos, line, col, fmt.Sprintf(format, args...)}
}

func (p *tparser) failAt(pos int, format string, args ...any) { panic(p.errAt(pos, format, args...)) }

func (p *tparser) fail(format string, args ...any) { p.failAt(p.pos, format, args...) }

// soft reports an error the editor can recover from; outside the editor it stops.
func (p *tparser) soft(pos int, format string, args ...any) {
	e := p.errAt(pos, format, args...)
	if !p.tolerant {
		panic(e)
	}
	p.errs = append(p.errs, e)
}

func (p *tparser) rest() string { return p.src[p.pos:] }
func (p *tparser) eof() bool    { return p.pos >= len(p.src) }

func (p *tparser) byteAt(i int) byte {
	if i < 0 || i >= len(p.src) {
		return 0
	}
	return p.src[i]
}

func isIdentStart(c byte) bool { return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isIdent(c byte) bool      { return isIdentStart(c) || c >= '0' && c <= '9' }
func isSpace(c byte) bool      { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func (p *tparser) skipWS() {
	for !p.eof() && isSpace(p.src[p.pos]) {
		p.pos++
	}
}

func (p *tparser) expect(s string) {
	if p.eof() {
		p.soft(p.pos, "expected %q", s)
		return
	}
	if !strings.HasPrefix(p.rest(), s) {
		p.fail("expected %q", s)
	}
	p.pos += len(s)
}

func (p *tparser) ident() string {
	start := p.pos
	for !p.eof() && isIdent(p.src[p.pos]) {
		p.pos++
	}
	return p.src[start:p.pos]
}

// trimmed is src[start:end] without surrounding whitespace, keeping its offset.
func (p *tparser) trimmed(start, end int) goSrc {
	s := p.src[start:end]
	t := strings.TrimLeft(s, " \t\r\n")
	return goSrc{strings.TrimRight(t, " \t\r\n"), start + len(s) - len(t)}
}

// skipGoLiteral steps over a Go string, rune or comment at pos.
func (p *tparser) skipGoLiteral() bool {
	s := p.rest()
	switch {
	case strings.HasPrefix(s, "//"):
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			p.pos += i
		} else {
			p.pos = len(p.src)
		}
	case strings.HasPrefix(s, "/*"):
		i := strings.Index(s[2:], "*/")
		if i < 0 {
			p.fail("unterminated comment")
		}
		p.pos += i + 4
	case s[0] == '`':
		i := strings.IndexByte(s[1:], '`')
		if i < 0 {
			p.fail("unterminated raw string")
		}
		p.pos += i + 2
	case s[0] == '"' || s[0] == '\'':
		q, i := s[0], 1
		for ; i < len(s) && s[i] != q; i++ {
			if s[i] == '\\' {
				i++
			} else if s[i] == '\n' {
				p.fail("newline in string literal")
			}
		}
		if i >= len(s) {
			p.fail("unterminated string literal")
		}
		p.pos += i + 1
	default:
		return false
	}
	return true
}

// balanced reads from the bracket at pos to its match and returns what is inside.
func (p *tparser) balanced() goSrc {
	start, depth := p.pos, 0
	for !p.eof() {
		if p.skipGoLiteral() {
			continue
		}
		switch p.src[p.pos] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
			if depth == 0 {
				p.pos++
				return goSrc{p.src[start+1 : p.pos-1], start + 1}
			}
		}
		p.pos++
	}
	p.soft(start, "unbalanced %q", p.src[start])
	return goSrc{p.src[start+1:], start + 1}
}

// header reads Go code up to the '{' that opens a block, and consumes the '{'.
func (p *tparser) header() goSrc {
	start, depth := p.pos, 0
	for !p.eof() {
		if p.skipGoLiteral() {
			continue
		}
		switch p.src[p.pos] {
		case '(', '[':
			depth++
		case ')', ']', '}':
			depth--
		case '{':
			if depth == 0 {
				h := p.trimmed(start, p.pos)
				p.pos++
				return h
			}
			depth++
		}
		p.pos++
	}
	p.failAt(start, "missing '{'")
	return goSrc{}
}

// implicit reads an implicit expression: a.b.c(x)[i].d
func (p *tparser) implicit() goSrc {
	start := p.pos
	if p.ident() == "" {
		p.soft(p.pos, "expected an expression after '@'")
		return goSrc{"", start}
	}
	for !p.eof() {
		switch c := p.src[p.pos]; {
		case c == '.' && isIdentStart(p.byteAt(p.pos+1)):
			p.pos++
			p.ident()
		case c == '.' && p.pos == p.dotAt:
			p.pos++
			return goSrc{p.src[start:p.pos], start}
		case c == '(' || c == '[':
			p.balanced()
		default:
			return goSrc{p.src[start:p.pos], start}
		}
	}
	return goSrc{p.src[start:p.pos], start}
}

func (p *tparser) nodes(endTag string, inBlock, top bool) []node {
	var out []node
	for !p.eof() {
		c := p.src[p.pos]
		if inBlock && c == '}' {
			return out
		}
		switch r := p.rest(); {
		case strings.HasPrefix(r, "</"):
			if endTag != "" {
				return out
			}
			p.soft(p.pos, "unexpected closing tag")
			p.pos += max(2, strings.IndexByte(r, '>')+1)
		case strings.HasPrefix(r, "<!--"):
			i := strings.Index(r, "-->")
			if i < 0 {
				p.fail("unterminated comment")
			}
			p.pos += i + 3
		case c == '<' && (isIdentStart(p.byteAt(p.pos+1)) || p.byteAt(p.pos+1) == ':'):
			out = append(out, p.element())
		case c == '@':
			if n := p.at(top); n != nil {
				out = append(out, n)
			}
		default:
			out = append(out, p.text(inBlock))
		}
	}
	if inBlock {
		p.soft(p.pos, "missing '}'")
	}
	if endTag != "" {
		p.soft(p.pos, "missing </%s>", endTag)
	}
	return out
}

func (p *tparser) isEmailAt(i int) bool { return isIdent(p.byteAt(i-1)) && isIdent(p.byteAt(i+1)) }

func (p *tparser) text(inBlock bool) node {
	start := p.pos
	p.pos++
	for !p.eof() {
		c := p.src[p.pos]
		next := p.byteAt(p.pos + 1)
		if c == '<' && (isIdentStart(next) || next == '/' || next == '!' || next == ':') ||
			c == '@' && !p.isEmailAt(p.pos) || inBlock && c == '}' {
			break
		}
		p.pos++
	}
	return textN{p.src[start:p.pos]}
}

func (p *tparser) at(top bool) node {
	start := p.pos
	if p.isEmailAt(p.pos) {
		p.pos++
		return textN{"@"}
	}
	switch p.byteAt(p.pos + 1) {
	case '@':
		p.pos += 2
		return textN{"@"}
	case '*':
		i := strings.Index(p.src[p.pos:], "*@")
		if i < 0 {
			p.fail("unterminated @* comment")
		}
		p.pos += i + 2
		p.mark(start, p.pos-start, true)
		return nil
	case '(':
		p.mark(start, 1, false)
		p.pos++
		return exprN{p.balanced()}
	case '{':
		p.mark(start, 1, false)
		p.pos++
		return codeN{p.balanced()}
	}
	p.pos++
	word := p.ident()
	switch word {
	case "if":
		p.mark(start, 3, false)
		return p.ifBlock()
	case "for":
		p.mark(start, 4, false)
		clause := p.header()
		body := p.nodes("", true, false)
		p.expect("}")
		return forN{clause, body}
	}
	if top {
		switch word {
		case "code":
			p.mark(start, 5, false)
			p.skipWS()
			if p.byteAt(p.pos) != '{' {
				p.fail("expected '{' after @code")
			}
			p.f.code = append(p.f.code, p.balanced())
			return nil
		case "page", "layout", "inherits", "import", "inject", "param":
			end := strings.IndexByte(p.rest(), '\n')
			if end < 0 {
				end = len(p.rest())
			}
			p.mark(start, 1+len(word), false)
			arg := p.trimmed(p.pos, p.pos+end)
			p.pos += end
			p.directive(start, word, arg)
			return nil
		}
	}
	p.mark(start, 1, false)
	p.pos = start + 1
	return exprN{p.implicit()}
}

func (p *tparser) mark(pos, n int, comment bool) {
	p.f.marks = append(p.f.marks, mark{pos, n, comment})
}

func (p *tparser) directive(at int, word string, arg goSrc) {
	switch word {
	case "page":
		route, err := strconv.Unquote(arg.code)
		if err != nil {
			p.failAt(at, "@page needs a quoted route")
		}
		p.f.pages = append(p.f.pages, route)
	case "layout":
		p.f.layout = arg
	case "inherits":
		p.f.inherits = arg
	case "import":
		p.f.imports = append(p.f.imports, arg)
	case "inject", "param":
		if !strings.ContainsAny(arg.code, " \t") {
			p.failAt(at, "@%s needs a name and a type", word)
		}
		if word == "inject" {
			p.f.injects = append(p.f.injects, arg)
		} else {
			p.f.params = append(p.f.params, arg)
		}
	}
}

func (p *tparser) ifBlock() node {
	var n ifN
	cond := p.header()
	for {
		body := p.nodes("", true, false)
		p.expect("}")
		n.conds, n.bodies = append(n.conds, cond), append(n.bodies, body)
		if cond.pos < 0 {
			return n
		}
		save := p.pos
		p.skipWS()
		if !strings.HasPrefix(p.rest(), "else") || isIdent(p.byteAt(p.pos+4)) {
			p.pos = save
			return n
		}
		p.mark(p.pos, 4, false)
		p.pos += 4
		p.skipWS()
		if strings.HasPrefix(p.rest(), "if") && !isIdent(p.byteAt(p.pos+2)) {
			p.mark(p.pos, 2, false)
			p.pos += 2
			cond = p.header()
			if cond.code == "" {
				p.fail("else if needs a condition")
			}
		} else {
			p.expect("{")
			cond = goSrc{pos: -1}
		}
	}
}

func (p *tparser) element() node {
	start := p.pos
	line := 1 + strings.Count(p.src[:p.pos], "\n")
	p.pos++
	tagStart := p.pos
	for !p.eof() {
		c := p.src[p.pos]
		if c == '[' {
			p.balanced()
			continue
		}
		if !isIdent(c) && c != '-' && c != '.' && c != ':' {
			break
		}
		p.pos++
	}
	tag := p.src[tagStart:p.pos]
	el := elemN{tag: tag, tagPos: tagStart, start: start, line: line}
	selfClose := false
	for {
		p.skipWS()
		if p.eof() || p.src[p.pos] == '<' {
			// half-typed tag: treat it as self-closing so the rest of the file still parses
			p.soft(start, "unterminated <%s>", tag)
			el.head = p.pos
			return el
		}
		if strings.HasPrefix(p.rest(), "/>") {
			p.pos += 2
			selfClose = true
			break
		}
		if p.src[p.pos] == '>' {
			p.pos++
			break
		}
		nameStart := p.pos
		for !p.eof() && !isSpace(p.src[p.pos]) && !strings.ContainsRune("=><", rune(p.src[p.pos])) && !strings.HasPrefix(p.rest(), "/>") {
			p.pos++
		}
		a := attrN{name: p.src[nameStart:p.pos], namePos: nameStart}
		if a.name == "" {
			p.fail("bad attribute")
		}
		save := p.pos
		p.skipWS()
		if p.byteAt(p.pos) == '=' {
			p.pos++
			p.skipWS()
			a.has = true
			switch q := p.byteAt(p.pos); q {
			case '"', '\'':
				end := strings.IndexByte(p.src[p.pos+1:], q)
				if end < 0 {
					p.fail("unterminated attribute value")
				}
				a.raw, a.rawPos = p.src[p.pos+1:p.pos+1+end], p.pos+1
				p.pos += end + 2
			case '@':
				vs := p.pos
				p.pos++
				if p.byteAt(p.pos) == '(' {
					p.balanced()
				} else {
					p.implicit()
				}
				a.raw, a.rawPos = p.src[vs:p.pos], vs
			default:
				vs := p.pos
				for !p.eof() && !isSpace(p.src[p.pos]) && p.src[p.pos] != '>' && !strings.HasPrefix(p.rest(), "/>") {
					p.pos++
				}
				a.raw, a.rawPos = p.src[vs:p.pos], vs
			}
		} else {
			p.pos = save
		}
		el.attrs = append(el.attrs, a)
	}
	el.head = p.pos
	lower := strings.ToLower(tag)
	if selfClose || voidTags[lower] {
		return el
	}
	if lower == "script" || lower == "style" {
		end := strings.Index(strings.ToLower(p.rest()), "</"+lower)
		if end < 0 {
			p.failAt(start, "missing </%s>", tag)
		}
		el.kids = []node{rawN{p.src[p.pos : p.pos+end]}}
		p.pos += end
	} else {
		el.kids = p.nodes(tag, false, false)
	}
	if p.eof() {
		return el // nodes reported the missing close tag
	}
	closeAt := p.pos
	p.expect("</")
	gt := strings.IndexByte(p.rest(), '>')
	name := strings.TrimSpace(p.rest()[:max(0, gt)])
	if name != tag {
		p.pos = closeAt
		p.soft(closeAt, "expected </%s> (opened on line %d), found </%s>", tag, line, name)
		return el // leave the close tag for an enclosing element
	}
	p.pos += gt + 1
	return el
}

// interp splits an attribute value into text and @expressions. base is s's offset in the template.
func interp(path, s string, base, dotAt int) (parts []part, err error) {
	q := &tparser{src: s, path: path, f: &file{}, dotAt: dotAt - base}
	defer func() {
		if r := recover(); r != nil {
			pe, ok := r.(*parseError)
			if !ok {
				panic(r)
			}
			err = pe
		}
	}()
	var text strings.Builder
	flush := func() {
		if text.Len() > 0 {
			parts = append(parts, part{text: text.String()})
			text.Reset()
		}
	}
	shift := func(g goSrc) goSrc { g.pos += base; return g }
	for !q.eof() {
		c := q.src[q.pos]
		if c != '@' || q.isEmailAt(q.pos) {
			text.WriteByte(c)
			q.pos++
			continue
		}
		switch q.byteAt(q.pos + 1) {
		case '@':
			text.WriteByte('@')
			q.pos += 2
			continue
		case '(':
			q.pos++
			flush()
			start := q.pos
			q.balanced()
			parts = append(parts, part{expr: shift(goSrc{q.src[start:q.pos], start})})
			continue
		}
		q.pos++
		flush()
		parts = append(parts, part{expr: shift(q.implicit())})
	}
	flush()
	return parts, nil
}
