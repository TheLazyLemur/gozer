package main

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// doc is one .gozer file the proxy mirrors to gopls as an unsaved Go file.
type doc struct {
	path       string // the .gozer file
	text       string // current template text
	open       bool   // the editor has it open
	version    int    // version of the virtual file sent to gopls
	gs         *genState
	parseDiags []any
	goDiags    []any
}

// genState is one successful generation. gs.src may lag behind doc.text while
// the template does not parse; shift bridges the difference.
type genState struct {
	src string
	out *output
	gen *lineIndex
}

func pathToURI(path string) string { return (&url.URL{Scheme: "file", Path: path}).String() }

func uriToPath(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return ""
	}
	return filepath.Clean(u.Path)
}

func readFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	return string(b), err
}

// shift maps an offset in from to the same place in to, outside the edited region.
func shift(from, to string, off int) (int, bool) {
	if from == to {
		return off, true
	}
	pre := 0
	for pre < len(from) && pre < len(to) && from[pre] == to[pre] {
		pre++
	}
	suf := 0
	for suf < len(from)-pre && suf < len(to)-pre && from[len(from)-1-suf] == to[len(to)-1-suf] {
		suf++
	}
	switch {
	case off <= pre:
		return off, true
	case off >= len(from)-suf:
		return off + len(to) - len(from), true
	}
	return 0, false
}

// toGen maps an offset in the current template text into the virtual Go file.
func (gs *genState) toGen(cur string, off int) (int, bool) {
	o, ok := shift(cur, gs.src, off)
	if !ok {
		return 0, false
	}
	for _, s := range gs.out.spans {
		if s.src <= o && o <= s.src+s.n {
			return s.gen + o - s.src, true
		}
	}
	return 0, false
}

// toSrc maps an offset in the virtual Go file back to the current template text.
func (gs *genState) toSrc(cur string, g int) (int, bool) {
	for _, s := range gs.out.spans {
		if s.gen <= g && g <= s.gen+s.n {
			return shift(gs.src, cur, s.src+g-s.gen)
		}
	}
	return 0, false
}

// nearestSrc is the template offset of the last span starting at or before g;
// it anchors errors from generated-only code.
func (gs *genState) nearestSrc(cur string, g int) int {
	var best *span
	for i := range gs.out.spans {
		if s := &gs.out.spans[i]; s.gen <= g && (best == nil || s.gen > best.gen) {
			best = s
		}
	}
	if best == nil {
		return 0
	}
	off, _ := shift(gs.src, cur, best.src)
	return off
}

// lineIndex converts between byte offsets and LSP positions.
type lineIndex struct {
	text   string
	starts []int
}

func newLineIndex(text string) *lineIndex {
	x := &lineIndex{text: text, starts: []int{0}}
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			x.starts = append(x.starts, i+1)
		}
	}
	return x
}

type position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type lspRange struct {
	Start position `json:"start"`
	End   position `json:"end"`
}

func (x *lineIndex) lineEnd(l int) int {
	if l+1 < len(x.starts) {
		return x.starts[l+1] - 1
	}
	return len(x.text)
}

func (x *lineIndex) offset(p position, enc string) int {
	if p.Line >= len(x.starts) {
		return len(x.text)
	}
	off, end := x.starts[p.Line], x.lineEnd(p.Line)
	for units := 0; off < end && units < p.Character; {
		r, size := utf8.DecodeRuneInString(x.text[off:end])
		switch enc {
		case "utf-8":
			units += size
		case "utf-32":
			units++
		default:
			units += utf16.RuneLen(r)
		}
		off += size
	}
	return off
}

func (x *lineIndex) position(off int, enc string) position {
	off = max(0, min(off, len(x.text)))
	l := 0
	for l+1 < len(x.starts) && x.starts[l+1] <= off {
		l++
	}
	seg := x.text[x.starts[l]:off]
	switch enc {
	case "utf-8":
		return position{l, len(seg)}
	case "utf-32":
		return position{l, utf8.RuneCountInString(seg)}
	}
	n := 0
	for _, r := range seg {
		n += utf16.RuneLen(r)
	}
	return position{l, n}
}

// JSON helpers for walking decoded messages (decoded with UseNumber).

func asInt(v any) int {
	n, _ := v.(json.Number).Int64()
	return int(n)
}

func posOf(v any) (position, bool) {
	m, ok := v.(map[string]any)
	if !ok || m["line"] == nil || m["character"] == nil {
		return position{}, false
	}
	return position{asInt(m["line"]), asInt(m["character"])}, true
}

func setPos(v any, p position) {
	m := v.(map[string]any)
	m["line"], m["character"] = json.Number(itoa(p.Line)), json.Number(itoa(p.Character))
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func decode(raw []byte) (any, error) {
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.UseNumber()
	var v any
	err := d.Decode(&v)
	return v, err
}

var errUnmappable = errors.New("edit touches generated code")

// mapper rewrites positions in the virtual Go file of one doc to the template.
type mapper struct {
	cur string
	gs  *genState
	src *lineIndex
	enc string
}

func (m *mapper) offset(p position) int { return m.gs.gen.offset(p, m.enc) }

// rangeInPlace rewrites a {start,end} object; fallback anchors unmappable
// ranges at the nearest template code instead of failing.
func (m *mapper) rangeInPlace(v any, fallback bool) bool {
	r, ok := v.(map[string]any)
	if !ok {
		return false
	}
	sp, ok1 := posOf(r["start"])
	ep, ok2 := posOf(r["end"])
	if !ok1 || !ok2 {
		return false
	}
	gs, ge := m.offset(sp), m.offset(ep)
	s, okS := m.gs.toSrc(m.cur, gs)
	e, okE := m.gs.toSrc(m.cur, ge)
	if !okS || !okE || e < s {
		if !fallback {
			return false
		}
		s = m.gs.nearestSrc(m.cur, gs)
		e = s
	}
	setPos(r["start"], m.src.position(s, m.enc))
	setPos(r["end"], m.src.position(e, m.enc))
	return true
}
