// Package forms provides EditForm, input components and struct-tag validation,
// mirroring Blazor's Microsoft.AspNetCore.Components.Forms.
package forms

import (
	"fmt"
	"net/mail"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TheLazyLemur/gozer"
)

// EditContext tracks field state and validation messages for a model.
type EditContext struct {
	Model    any
	modified map[string]bool
	msgs     map[string][]string // field path -> messages; "" is model-level
	extra    map[string][]string // messages added by AddError, kept until the field changes
}

// NewEditContext creates a context for model, a pointer to a struct.
func NewEditContext(model any) *EditContext {
	if v := reflect.ValueOf(model); v.Kind() != reflect.Pointer || v.Elem().Kind() != reflect.Struct {
		panic(fmt.Sprintf("forms: model must be a pointer to a struct, got %T", model))
	}
	return &EditContext{Model: model, modified: map[string]bool{}, msgs: map[string][]string{}, extra: map[string][]string{}}
}

type field struct {
	path  string
	label string
	tag   string
	val   reflect.Value
}

// fields lists the model's fields, recursing into nested structs.
func (e *EditContext) fields() []field {
	var out []field
	var walk func(v reflect.Value, prefix string)
	walk = func(v reflect.Value, prefix string) {
		t := v.Type()
		for i := range t.NumField() {
			sf := t.Field(i)
			if !sf.IsExported() {
				continue
			}
			fv := v.Field(i)
			label := sf.Tag.Get("label")
			if label == "" {
				label = sf.Name
			}
			out = append(out, field{prefix + sf.Name, label, sf.Tag.Get("validate"), fv})
			if fv.Kind() == reflect.Struct && fv.Type() != reflect.TypeFor[time.Time]() {
				walk(fv, prefix+sf.Name+".")
			}
		}
	}
	walk(reflect.ValueOf(e.Model).Elem(), "")
	return out
}

// FieldPath returns the dotted path of the model field ptr points to.
func (e *EditContext) FieldPath(ptr any) string {
	pv := reflect.ValueOf(ptr)
	if pv.Kind() != reflect.Pointer || pv.IsNil() {
		panic(fmt.Sprintf("forms: field identifier must be a non-nil pointer, got %T", ptr))
	}
	for _, f := range e.fields() {
		if f.val.Addr().Pointer() == pv.Pointer() && f.val.Type() == pv.Type().Elem() {
			return f.path
		}
	}
	panic(fmt.Sprintf("forms: %T does not point into the form model %T", ptr, e.Model))
}

// NotifyFieldChanged marks the field modified and re-validates it.
func (e *EditContext) NotifyFieldChanged(ptr any) {
	path := e.FieldPath(ptr)
	e.modified[path] = true
	delete(e.extra, path)
	e.validate(path)
}

// IsModified reports whether the user changed the field (nil: any field).
func (e *EditContext) IsModified(ptr any) bool {
	if ptr == nil {
		return len(e.modified) > 0
	}
	return e.modified[e.FieldPath(ptr)]
}

// MarkAsUnmodified clears modification state.
func (e *EditContext) MarkAsUnmodified() { e.modified = map[string]bool{} }

// Validate validates every field and reports whether the model is valid.
func (e *EditContext) Validate() bool {
	e.msgs = map[string][]string{}
	e.validate("")
	return len(e.Messages(nil)) == 0
}

// AddError attaches a custom message to a field (nil: the model), e.g. from a server check.
func (e *EditContext) AddError(ptr any, msg string) {
	path := ""
	if ptr != nil {
		path = e.FieldPath(ptr)
	}
	e.extra[path] = append(e.extra[path], msg)
}

// Messages returns the field's messages, or all messages when ptr is nil.
func (e *EditContext) Messages(ptr any) []string {
	if ptr != nil {
		path := e.FieldPath(ptr)
		return append(append([]string{}, e.msgs[path]...), e.extra[path]...)
	}
	var out []string
	for _, f := range append([]field{{path: ""}}, e.fields()...) {
		out = append(out, e.msgs[f.path]...)
		out = append(out, e.extra[f.path]...)
	}
	return out
}

// FieldClass is Blazor's CSS class for an input: "modified", "valid"/"invalid".
func (e *EditContext) FieldClass(ptr any) string {
	path := e.FieldPath(ptr)
	invalid := len(e.msgs[path])+len(e.extra[path]) > 0
	switch {
	case e.modified[path] && invalid:
		return "modified invalid"
	case e.modified[path]:
		return "modified valid"
	case invalid:
		return "invalid"
	}
	return "valid"
}

// Validator lets a model add its own rules: return messages keyed by field path ("" for the model).
type Validator interface {
	Validate() map[string]string
}

// validate re-runs rules for one field path, or all when path is "".
func (e *EditContext) validate(path string) {
	for _, f := range e.fields() {
		if path != "" && f.path != path {
			continue
		}
		delete(e.msgs, f.path)
		for _, rule := range strings.Split(f.tag, ",") {
			if rule = strings.TrimSpace(rule); rule == "" {
				continue
			}
			if msg := check(rule, f); msg != "" {
				e.msgs[f.path] = append(e.msgs[f.path], msg)
			}
		}
	}
	if v, ok := e.Model.(Validator); ok {
		if path == "" {
			delete(e.msgs, "")
		}
		for p, msg := range v.Validate() {
			if path == "" || p == path {
				e.msgs[p] = append(e.msgs[p], msg)
			}
		}
	}
}

var regexCache sync.Map // pattern -> *regexp.Regexp

// check applies one rule: required, min=N, max=N, len=N, email, url, pattern=RE.
// min/max compare numbers by value and strings/slices by length.
func check(rule string, f field) string {
	name, arg, _ := strings.Cut(rule, "=")
	v := f.val
	switch name {
	case "required":
		if v.IsZero() || (v.Kind() == reflect.String && strings.TrimSpace(v.String()) == "") {
			return fmt.Sprintf("The %s field is required.", f.label)
		}
	case "min", "max", "len":
		n, err := strconv.ParseFloat(arg, 64)
		if err != nil {
			panic(fmt.Sprintf("forms: bad rule %q on %s", rule, f.path))
		}
		if v.Kind() == reflect.String && v.Len() == 0 {
			return "" // empty is required's job
		}
		var have float64
		isLen := false
		switch v.Kind() {
		case reflect.String, reflect.Slice, reflect.Map, reflect.Array:
			have, isLen = float64(len([]rune(fmt.Sprint(v.Interface())))), true
			if v.Kind() != reflect.String {
				have = float64(v.Len())
			}
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			have = float64(v.Int())
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			have = float64(v.Uint())
		case reflect.Float32, reflect.Float64:
			have = v.Float()
		default:
			panic(fmt.Sprintf("forms: rule %q does not apply to %s", rule, v.Type()))
		}
		switch {
		case name == "min" && have < n && isLen:
			return fmt.Sprintf("%s must be at least %s characters.", f.label, arg)
		case name == "max" && have > n && isLen:
			return fmt.Sprintf("%s must be at most %s characters.", f.label, arg)
		case name == "len" && have != n:
			return fmt.Sprintf("%s must be exactly %s characters.", f.label, arg)
		case name == "min" && have < n:
			return fmt.Sprintf("%s must be at least %s.", f.label, arg)
		case name == "max" && have > n:
			return fmt.Sprintf("%s must be at most %s.", f.label, arg)
		}
	case "email":
		if s := v.String(); s != "" {
			if a, err := mail.ParseAddress(s); err != nil || a.Address != s {
				return fmt.Sprintf("%s is not a valid e-mail address.", f.label)
			}
		}
	case "pattern":
		cached, ok := regexCache.Load(arg)
		if !ok {
			cached, _ = regexCache.LoadOrStore(arg, regexp.MustCompile(arg))
		}
		re := cached.(*regexp.Regexp)
		if s := v.String(); s != "" && !re.MatchString(s) {
			return fmt.Sprintf("%s is not in the expected format.", f.label)
		}
	default:
		panic(fmt.Sprintf("forms: unknown rule %q on %s", rule, f.path))
	}
	return ""
}

// EditForm renders a <form> that validates its model on submit and cascades
// its EditContext to inputs and validation components.
type EditForm struct {
	gozer.Base
	Model           any
	EditContext     *EditContext
	OnSubmit        func(*EditContext)
	OnValidSubmit   func(*EditContext)
	OnInvalidSubmit func(*EditContext)
	Attrs           map[string]any
	ChildContent    gozer.Node
	ctx             *EditContext
}

func (f *EditForm) OnParametersSet() {
	switch {
	case f.EditContext != nil:
		f.ctx = f.EditContext
	case f.Model == nil:
		panic("forms: EditForm needs Model or EditContext")
	case f.ctx == nil || f.ctx.Model != f.Model:
		f.ctx = NewEditContext(f.Model)
	}
}

func (f *EditForm) submit() {
	if f.OnSubmit != nil {
		f.OnSubmit(f.ctx)
		return
	}
	if f.ctx.Validate() {
		if f.OnValidSubmit != nil {
			f.OnValidSubmit(f.ctx)
		}
	} else if f.OnInvalidSubmit != nil {
		f.OnInvalidSubmit(f.ctx)
	}
}

func (f *EditForm) Render() gozer.Node {
	return gozer.C(func(cv *gozer.CascadingValue) {
		cv.Value = f.ctx
		cv.ChildContent = gozer.El("form", gozer.Attrs(f.Attrs), gozer.On("submit", f.submit), f.ChildContent)
	})
}

// inputAttrs merges user attributes with the validation CSS class.
func inputAttrs(attrs map[string]any, ctx *EditContext, ptr any) gozer.Node {
	class, _ := attrs["class"].(string)
	if ctx != nil {
		class = strings.TrimSpace(class + " " + ctx.FieldClass(ptr))
	}
	var c gozer.Node
	if class != "" {
		c = gozer.Attr("class", class)
	}
	return gozer.Frag(gozer.Attrs(attrs), c)
}

// changed writes v through the binding and tells the EditContext.
func changed[T any](b gozer.Binding[T], ctx *EditContext, v T) {
	b.Set(v)
	if ctx != nil {
		ctx.NotifyFieldChanged(b.Ptr())
	}
}

// InputText is <input type=text> bound to a string.
type InputText struct {
	gozer.Base
	Value gozer.Binding[string]
	Attrs map[string]any
	Ctx   *EditContext `cascade:""`
}

func (i *InputText) Render() gozer.Node {
	return gozer.El("input", gozer.Attr("type", "text"), inputAttrs(i.Attrs, i.Ctx, i.Value.Ptr()),
		gozer.Attr("value", i.Value.Get()),
		gozer.On("change", func(e gozer.Event) { changed(i.Value, i.Ctx, e.Value) }))
}

// InputTextArea is a <textarea> bound to a string.
type InputTextArea struct {
	gozer.Base
	Value gozer.Binding[string]
	Attrs map[string]any
	Ctx   *EditContext `cascade:""`
}

func (i *InputTextArea) Render() gozer.Node {
	return gozer.El("textarea", inputAttrs(i.Attrs, i.Ctx, i.Value.Ptr()),
		gozer.Attr("value", i.Value.Get()),
		gozer.On("change", func(e gozer.Event) { changed(i.Value, i.Ctx, e.Value) }))
}

// Number is the set of types InputNumber accepts.
type Number interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 | ~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 | ~float32 | ~float64
}

// InputNumber is <input type=number> bound to a number.
type InputNumber[T Number] struct {
	gozer.Base
	Value gozer.Binding[T]
	Attrs map[string]any
	Ctx   *EditContext `cascade:""`
}

func (i *InputNumber[T]) Render() gozer.Node {
	return gozer.El("input", gozer.Attr("type", "number"), inputAttrs(i.Attrs, i.Ctx, i.Value.Ptr()),
		gozer.Attr("value", gozer.FormatValue(i.Value.Get(), "")),
		gozer.On("change", func(e gozer.Event) {
			var v T
			if err := gozer.ParseValue(&v, e.Value, ""); err != nil {
				if i.Ctx != nil {
					p := i.Value.Ptr()
					i.Ctx.NotifyFieldChanged(p)
					i.Ctx.AddError(p, fmt.Sprintf("The %s field must be a number.", i.Ctx.label(p)))
				}
				return
			}
			changed(i.Value, i.Ctx, v)
		}))
}

func (e *EditContext) label(ptr any) string {
	path := e.FieldPath(ptr)
	for _, f := range e.fields() {
		if f.path == path {
			return f.label
		}
	}
	return path
}

// InputCheckbox is <input type=checkbox> bound to a bool.
type InputCheckbox struct {
	gozer.Base
	Value gozer.Binding[bool]
	Attrs map[string]any
	Ctx   *EditContext `cascade:""`
}

func (i *InputCheckbox) Render() gozer.Node {
	return gozer.El("input", gozer.Attr("type", "checkbox"), inputAttrs(i.Attrs, i.Ctx, i.Value.Ptr()),
		gozer.Attr("checked", i.Value.Get()),
		gozer.On("change", func(e gozer.Event) { changed(i.Value, i.Ctx, e.Checked) }))
}

// InputDate is <input type=date> bound to a time.Time.
type InputDate struct {
	gozer.Base
	Value gozer.Binding[time.Time]
	Attrs map[string]any
	Ctx   *EditContext `cascade:""`
}

func (i *InputDate) Render() gozer.Node {
	return gozer.El("input", gozer.Attr("type", "date"), inputAttrs(i.Attrs, i.Ctx, i.Value.Ptr()),
		gozer.Attr("value", gozer.FormatValue(i.Value.Get(), time.DateOnly)),
		gozer.On("change", func(e gozer.Event) {
			var v time.Time
			if gozer.ParseValue(&v, e.Value, time.DateOnly) == nil {
				changed(i.Value, i.Ctx, v)
			}
		}))
}

// InputSelect is a <select> bound to any string-convertible value; put <option>s in ChildContent.
type InputSelect[T any] struct {
	gozer.Base
	Value        gozer.Binding[T]
	Attrs        map[string]any
	ChildContent gozer.Node
	Ctx          *EditContext `cascade:""`
}

func (i *InputSelect[T]) Render() gozer.Node {
	return gozer.El("select", inputAttrs(i.Attrs, i.Ctx, i.Value.Ptr()),
		gozer.Attr("value", gozer.FormatValue(i.Value.Get(), "")),
		gozer.On("change", func(e gozer.Event) {
			var v T
			if gozer.ParseValue(&v, e.Value, "") == nil {
				changed(i.Value, i.Ctx, v)
			}
		}), i.ChildContent)
}

// InputRadioGroup renders one radio button per option, bound to a value.
type InputRadioGroup[T comparable] struct {
	gozer.Base
	Value   gozer.Binding[T]
	Options []T
	Label   func(T) string
	Name    string
	Ctx     *EditContext `cascade:""`
}

func (r *InputRadioGroup[T]) Render() gozer.Node {
	var out []gozer.Node
	for _, opt := range r.Options {
		text := fmt.Sprint(opt)
		if r.Label != nil {
			text = r.Label(opt)
		}
		out = append(out, gozer.El("label",
			gozer.El("input", gozer.Attr("type", "radio"), gozer.Attr("name", r.Name),
				gozer.Attr("checked", r.Value.Get() == opt),
				gozer.On("change", func() { changed(r.Value, r.Ctx, opt) })),
			gozer.Text(" "+text)))
	}
	return gozer.Frag(out...)
}

// ValidationMessage shows the messages for one field.
type ValidationMessage struct {
	gozer.Base
	For any
	Ctx *EditContext `cascade:""`
}

func (v *ValidationMessage) Render() gozer.Node {
	if v.Ctx == nil {
		panic("forms: ValidationMessage must be inside an EditForm")
	}
	var out []gozer.Node
	for _, m := range v.Ctx.Messages(v.For) {
		out = append(out, gozer.El("div", gozer.Attr("class", "validation-message"), gozer.Text(m)))
	}
	return gozer.Frag(out...)
}

// ValidationSummary lists every validation message.
type ValidationSummary struct {
	gozer.Base
	Ctx *EditContext `cascade:""`
}

func (v *ValidationSummary) Render() gozer.Node {
	if v.Ctx == nil {
		panic("forms: ValidationSummary must be inside an EditForm")
	}
	msgs := v.Ctx.Messages(nil)
	if len(msgs) == 0 {
		return nil
	}
	var items []gozer.Node
	for _, m := range msgs {
		items = append(items, gozer.El("li", gozer.Attr("class", "validation-message"), gozer.Text(m)))
	}
	return gozer.El("ul", gozer.Attr("class", "validation-errors"), gozer.Frag(items...))
}
