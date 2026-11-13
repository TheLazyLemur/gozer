package forms

import (
	"strings"
	"testing"
)

type account struct {
	Name    string `validate:"required,min=3"`
	Email   string `validate:"email" label:"E-mail"`
	Age     int    `validate:"min=18"`
	Address struct {
		City string `validate:"required"`
	}
}

func (a *account) Validate() map[string]string {
	if a.Name == "root" {
		return map[string]string{"Name": "reserved"}
	}
	return nil
}

func TestEditContextValidatesTagsNestedFieldsAndModelRules(t *testing.T) {
	// given
	// ... a model with tag rules, a nested struct and a custom rule
	m := &account{Name: "root", Email: "nope", Age: 12}
	ctx := NewEditContext(m)

	// when
	// ... the whole model is validated, then Name is fixed and re-checked on its own
	valid := ctx.Validate()
	all := strings.Join(ctx.Messages(nil), "|")
	m.Name = "Ada"
	ctx.NotifyFieldChanged(&m.Name)

	// then
	// ... every rule reported, and the fixed field is now modified and valid
	want := "reserved|E-mail is not a valid e-mail address.|Age must be at least 18.|The City field is required."
	if valid || all != want {
		t.Fatalf("valid=%v messages=%q", valid, all)
	}
	if ctx.FieldPath(&m.Address.City) != "Address.City" || ctx.FieldClass(&m.Name) != "modified valid" {
		t.Fatalf("path=%q class=%q", ctx.FieldPath(&m.Address.City), ctx.FieldClass(&m.Name))
	}
}
