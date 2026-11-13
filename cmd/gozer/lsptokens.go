package main

import (
	"encoding/json"
	"slices"
	"sort"
)

type semTok struct{ off, n, typ, mods int }

// semanticTokens colours a template: gopls's tokens for the Go it contains,
// mapped back from the generated file, plus the template's own syntax.
func (p *proxy) semanticTokens(path, text string, gs *genState, src *lineIndex) any {
	p.mu.Lock()
	enc, legend := p.enc, p.legend
	p.mu.Unlock()
	var toks []semTok
	raw, err := p.call("textDocument/semanticTokens/full", map[string]any{
		"textDocument": map[string]any{"uri": pathToURI(path + ".go")}})
	var res struct{ Data []int }
	if err == nil && json.Unmarshal(raw, &res) == nil {
		line, char := 0, 0
		for i := 0; i+4 < len(res.Data); i += 5 {
			d := res.Data[i : i+5]
			if d[0] > 0 {
				line, char = line+d[0], d[1]
			} else {
				char += d[1]
			}
			start := gs.gen.offset(position{line, char}, enc)
			end := gs.gen.offset(position{line, char + d[2]}, enc)
			s, ok1 := gs.toSrc(text, start)
			e, ok2 := gs.toSrc(text, end)
			if ok1 && ok2 && e-s == end-start && e > s { // only tokens inside copied template code
				toks = append(toks, semTok{s, e - s, d[3], d[4]})
			}
		}
	}
	keyword, comment := slices.Index(legend, "keyword"), slices.Index(legend, "comment")
	for _, m := range gs.out.marks {
		typ := keyword
		if m.comment {
			typ = comment
		}
		if s, ok := shift(gs.src, text, m.pos); ok && typ >= 0 {
			toks = append(toks, semTok{s, m.n, typ, 0})
		}
	}
	sort.Slice(toks, func(i, j int) bool { return toks[i].off < toks[j].off })
	data := []int{}
	prevLine, prevChar := 0, 0
	for _, t := range toks {
		sp, ep := src.position(t.off, enc), src.position(t.off+t.n, enc)
		if ep.Line != sp.Line {
			continue // shortcut: multi-line tokens (block comments, raw strings) are not coloured
		}
		dc := sp.Character
		if sp.Line == prevLine {
			dc -= prevChar
		}
		data = append(data, sp.Line-prevLine, dc, ep.Character-sp.Character, t.typ, t.mods)
		prevLine, prevChar = sp.Line, sp.Character
	}
	return map[string]any{"data": data}
}
