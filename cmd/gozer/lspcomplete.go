package main

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

const (
	kindField   = 5
	kindClass   = 7
	kindIface   = 8
	kindModule  = 9
	kindKeyword = 14
	kindStruct  = 22
)

var htmlTags = strings.Fields(`a abbr article aside audio b blockquote br button canvas caption code col
	colgroup datalist dd details dialog div dl dt em fieldset figcaption figure footer form h1 h2 h3 h4 h5 h6
	header hr i iframe img input label legend li main mark meter nav ol optgroup option output p picture pre
	progress q section select small source span strong style sub summary sup svg table tbody td template
	textarea tfoot th thead time tr u ul video`)

var htmlAttrs = strings.Fields(`id class style title href src alt type name value placeholder disabled checked
	readonly required for role aria-label target rel width height tabindex hidden min max step pattern
	autocomplete autofocus multiple selected rows cols colspan rowspan action method`)

var directiveAttrs = strings.Fields(`@onclick @ondblclick @oninput @onchange @onsubmit @onkeydown @onkeyup
	@onfocus @onblur @onmouseover @onmouseout @onmousedown @onmouseup @onwheel @onscroll
	@bind @bind:event @bind:format @key @ref @attributes`)

var blockDirectives = strings.Fields(`if for code page layout import inject param inherits`)

func isTagChar(c byte) bool {
	return isIdent(c) || c == '.' || c == '-' || c == '[' || c == ']' || c == ':'
}
func isAttrChar(c byte) bool { return isIdent(c) || c == '@' || c == '-' || c == ':' }

type completeCtx int

const (
	inOther completeCtx = iota
	inTagName
	inAttrName
)

// classify says whether off is in a tag name, an attribute name (with its tag), or elsewhere.
func classify(text string, gs *genState, off int) (completeCtx, *tagInfo, int) {
	i := off
	for i > 0 && isTagChar(text[i-1]) {
		i--
	}
	if i > 0 && text[i-1] == '<' {
		return inTagName, nil, i
	}
	j := off
	for j > 0 && isAttrChar(text[j-1]) {
		j--
	}
	if j == 0 || !isSpace(text[j-1]) {
		return inOther, nil, off
	}
	var hit *tagInfo
	for k := range gs.out.tags {
		t := &gs.out.tags[k]
		start, ok1 := shift(gs.src, text, t.start)
		head, ok2 := shift(gs.src, text, t.head)
		if ok1 && ok2 && start < j && off <= head && strings.Count(text[start:j], `"`)%2 == 0 {
			hit = t
		}
	}
	if hit == nil {
		return inOther, nil, off
	}
	return inAttrName, hit, j
}

func (p *proxy) complete(d *doc, text string, gs *genState, src *lineIndex, off int, params map[string]any) any {
	ctx, tag, start := classify(text, gs, off)
	if g, ok := gs.toGen(text, off); ok {
		return p.goComplete(d, text, gs, src, g, off, params, ctx)
	}
	// "@c." does not parse as an expression until something follows the dot
	if off > 0 && text[off-1] == '.' {
		if tmp, _ := p.regen(d.path, text, off-1); tmp != nil {
			if g, ok := tmp.toGen(text, off); ok {
				p.sendVirtual(d, tmp.out.gen)
				res := p.goComplete(d, text, tmp, src, g, off, params, ctx)
				p.sendVirtual(d, gs.out.gen)
				return res
			}
		}
	}
	edit := func(label string, kind int, insert string) map[string]any {
		return map[string]any{"label": label, "kind": kind, "textEdit": map[string]any{
			"range":   lspRange{src.position(start, p.enc), src.position(off, p.enc)},
			"newText": insert,
		}}
	}
	var items []any
	switch ctx {
	case inTagName:
		for _, t := range htmlTags {
			items = append(items, edit(t, kindKeyword, t))
		}
		for name := range localComponents(filepath.Dir(d.path)) {
			items = append(items, edit(name, kindStruct, name))
		}
		for name := range builtins {
			items = append(items, edit(name, kindStruct, name))
		}
	case inAttrName:
		if tag.probe >= 0 {
			for _, f := range p.componentFields(d, gs, tag.probe) {
				items = append(items, edit(f.name, kindField, f.name+"="))
				if strings.Contains(f.detail, "Binding[") {
					items = append(items, edit("@bind-"+f.name, kindField, "@bind-"+f.name+"="))
				}
			}
			items = append(items, edit("@key", kindKeyword, "@key="), edit("@ref", kindKeyword, "@ref="))
			break
		}
		for _, a := range htmlAttrs {
			items = append(items, edit(a, kindField, a+"="))
		}
		for _, a := range directiveAttrs {
			items = append(items, edit(a, kindKeyword, a+"="))
		}
	}
	return map[string]any{"isIncomplete": false, "items": items}
}

type fieldInfo struct{ name, detail string }

// componentFields asks gopls what can follow "_pN." for a component tag.
func (p *proxy) componentFields(d *doc, gs *genState, probe int) []fieldInfo {
	raw, err := p.call("textDocument/completion", map[string]any{
		"textDocument": map[string]any{"uri": pathToURI(d.path + ".go")},
		"position":     gs.gen.position(probe, p.enc),
	})
	if err != nil {
		return nil
	}
	v, _ := decode(raw)
	var out []fieldInfo
	for _, it := range completionItems(v) {
		item := it.(map[string]any)
		label, _ := item["label"].(string)
		detail, _ := item["detail"].(string)
		if asInt(item["kind"]) == kindField && isExported(label) && label != "Base" && label != "LayoutBase" {
			out = append(out, fieldInfo{label, detail})
		}
	}
	return out
}

func isExported(s string) bool { return s != "" && unicode.IsUpper(rune(s[0])) }

func completionItems(v any) []any {
	switch t := v.(type) {
	case []any:
		return t
	case map[string]any:
		items, _ := t["items"].([]any)
		return items
	}
	return nil
}

var quotedPath = regexp.MustCompile(`"([^"]+)"`)

// goComplete asks gopls at generated offset g and maps the answer back to the template.
func (p *proxy) goComplete(d *doc, text string, gs *genState, src *lineIndex, g, off int, params map[string]any, ctx completeCtx) any {
	params["textDocument"] = map[string]any{"uri": pathToURI(d.path + ".go")}
	setPos(params["position"], gs.gen.position(g, p.enc))
	raw, err := p.call("textDocument/completion", params)
	if err != nil {
		return nil
	}
	v, err := decode(raw)
	if err != nil {
		return nil
	}
	mp := &mapper{cur: text, gs: gs, src: src, enc: p.enc}
	var items []any
	for _, it := range completionItems(v) {
		item := it.(map[string]any)
		kind := asInt(item["kind"])
		label, _ := item["label"].(string)
		if ctx == inTagName && !slices.Contains([]int{kindClass, kindStruct, kindIface, kindModule}, kind) {
			continue // after '<' only types and packages make sense
		}
		if ctx == inAttrName && (kind != kindField || !isExported(label)) {
			continue
		}
		if te, ok := item["textEdit"].(map[string]any); ok {
			okEdit := true
			for _, k := range []string{"range", "insert", "replace"} {
				if te[k] != nil && !mp.rangeInPlace(te[k], false) {
					okEdit = false
				}
			}
			if !okEdit {
				delete(item, "textEdit")
				item["insertText"] = te["newText"]
			}
		}
		if extra, ok := item["additionalTextEdits"].([]any); ok {
			var kept []any
			for _, e := range extra {
				e := e.(map[string]any)
				newText, _ := e["newText"].(string)
				switch {
				case mp.rangeInPlace(e["range"], false):
					kept = append(kept, e)
				case quotedPath.MatchString(newText): // auto-import: becomes an @import line
					kept = append(kept, map[string]any{
						"range":   lspRange{},
						"newText": "@import " + quotedPath.FindString(newText) + "\n",
					})
				}
			}
			item["additionalTextEdits"] = kept
		}
		items = append(items, item)
	}
	// right after '@' the template keywords are valid too
	i := off
	for i > 0 && isIdent(text[i-1]) {
		i--
	}
	if ctx == inOther && i > 0 && text[i-1] == '@' {
		for _, kw := range blockDirectives {
			items = append(items, map[string]any{"label": kw, "kind": kindKeyword, "textEdit": map[string]any{
				"range": lspRange{src.position(i, p.enc), src.position(off, p.enc)}, "newText": kw}})
		}
	}
	if ctx == inTagName {
		for name := range builtins {
			items = append(items, map[string]any{"label": name, "kind": kindStruct})
		}
	}
	return map[string]any{"isIncomplete": true, "items": items}
}
