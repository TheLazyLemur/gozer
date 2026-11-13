# gozer

Blazor Server for Go. Stateful components run on the server, the browser gets DOM diffs over a WebSocket, and events go back the same way. Templates are `.gozer` files (Razor-like) compiled to plain Go, so an app builds to one binary with the client script embedded.

```sh
cd examples/demo
go generate      # .gozer -> .gozer.go
go build         # one binary, assets embedded
./demo           # http://localhost:8080
```

## A component

`Counter.gozer`:

```
@page "/counter"
@page "/counter/{start:int}"

@code {
type Counter struct {
	gozer.Base
	Start           int   // route value or parameter
	IncrementAmount int   // parameter
	count           int   // state
}

func (c *Counter) OnInit()     { c.count = c.Start }
func (c *Counter) increment()  { c.count += c.IncrementAmount }
}

<h1>Counter</h1>
<p>Current count: @c.count</p>
<button @onclick="c.increment">Click me</button>
```

Use it from another template: `<Counter IncrementAmount=@5 />`. Inside a template the component is `c`.

`main.go`:

```go
//go:generate go run github.com/TheLazyLemur/gozer/cmd/gozer

func main() {
	app := gozer.New()
	app.Root = gozer.Factory[App]()      // App.gozer holds <Router DefaultLayout=@gozer.Factory[MainLayout]() />
	app.Static("/static/", assets)       // embed.FS
	gozer.Provide(app, NewWeatherService())
	log.Fatal(app.ListenAndServe(":8080"))
}
```

## Template syntax

| Syntax | Meaning |
|---|---|
| `@c.Name`, `@f.Date.Format("Jan 2")`, `@(a + b)` | output (escaped); a `gozer.Node` value renders as markup |
| `@if cond { … } else if … { … } else { … }` | conditionals |
| `@for _, x := range c.Items { … }` | loops |
| `@{ x := 1 }` | Go statements inside `Render` |
| `@* comment *@`, `@@` | comment, literal `@` (`a@b.com` stays literal) |
| `class="btn @c.Kind"`, `disabled=@c.Busy` | attributes; `false`/`nil` omit them |
| `@onclick="c.Save"`, `@onclick="func() { c.Del(x.ID) }"` | events; handler is `func()` or `func(gozer.Event)` |
| `@onsubmit:preventDefault`, `@onclick:stopPropagation` | event modifiers |
| `@bind="c.Name"`, `@bind:event="oninput"`, `@bind:format="2006-01-02"` | two-way binding (strings, numbers, bools → `checked`, `time.Time`) |
| `@key="x.ID"`, `@ref="c.input"`, `@attributes="c.Extra"` | keys, element refs (`gozer.ElementRef`), attribute splatting |
| `<Comp Title="text" Count=@n OnPick=@c.Pick />` | component parameters; quoted text is a string, `@expr` is Go |
| `<Comp @bind-Value="c.Name" />` | two-way component parameter (`Value gozer.Binding[T]`) |
| `<Comp @ref="c.comp" />` | reference to the component instance |
| `<Grid[Row] Items=@rows><:Header>…</:Header><:Row args="r Row">…</:Row></Grid[Row]>` | generic, templated components; `<:Name>` fills a `Node` or `func(...) Node` field; other children go to `ChildContent` |
| `@page "/x/{id:int}"`, `@layout Main`, `@import "fmt"`, `@inherits gozer.LayoutBase` | directives |
| `@param Title string`, `@inject Svc *Service` | fields for template-only components (no `@code` struct) |
| `@code { … }` | Go declarations; declare `type <FileName> struct{ gozer.Base; … }` for state |

`Router`, `NavLink`, `PageTitle`, `HeadContent`, `CascadingValue`, `ErrorBoundary` and `AuthorizeView` need no prefix; other packages are qualified (`<forms.InputText …>`).

Component parameters on `On*` fields are wrapped so the parent re-renders after the child calls them (Blazor's `EventCallback`).

## Blazor feature map

| Blazor | gozer |
|---|---|
| Components, parameters, `ChildContent`, `RenderFragment<T>` | `Component` + `gozer.Base`; parameters are the fields a parent assigns; `Node` / `func(T) Node` fields |
| `OnInitialized`, `OnParametersSet`, `OnAfterRender(first)`, `ShouldRender`, `IDisposable` | `OnInit`, `OnParametersSet`, `OnAfterRender(first)`, `ShouldRender`, `Dispose` |
| `StateHasChanged`, `InvokeAsync` | same names on `Base`; `StateHasChanged` is safe from any goroutine, `InvokeAsync(f)` runs `f` on the circuit |
| `@key`, `@ref`, `@bind`, `@attributes`, event modifiers | yes |
| `EventCallback`, `@bind-Value` | `gozer.Callback` (auto for `On*` params), `gozer.Binding[T]` |
| Router, route constraints, catch-all, optional segments, `[SupplyParameterFromQuery]` | `@page`, `{id:int}`, `{*path}`, `{page?}`, `query:"name"` field tag |
| Layouts, `NavLink`, `PageTitle`, `HeadContent`, `NavigationManager` | `LayoutBase` + `@layout` / `Router.DefaultLayout`, `NavLink`, `PageTitle`, `HeadContent` (meta, canonical and Open Graph tags; prerendered into `<head>` for crawlers), `Circuit().Nav` (`NavigateTo`, `ForceLoad`, `Replace`, `OnChange`) |
| `[Inject]`, singleton/scoped services | `inject:""` field tag; `gozer.Provide`, `gozer.ProvideScoped`, `gozer.Service[T]` |
| `CascadingValue`, `[CascadingParameter]` | `CascadingValue`, `cascade:""` / `cascade:"Name"` field tag |
| `ErrorBoundary` | `ErrorBoundary` with `ErrorContent func(error) Node` and `Recover()` |
| `IJSRuntime`, `DotNetObjectReference` | `JSRuntime.Invoke/InvokeVoid/InvokeInto`, `ElementRef.Focus`; `JS.Expose` + `gozer.invoke(name, …)` in the browser |
| `EditForm`, `DataAnnotationsValidator`, `Input*`, `ValidationMessage/Summary` | `gozer/forms`: `EditForm`, `validate:"required,min=,max=,len=,email,pattern="` tags, `Validator` interface, `InputText/TextArea/Number/Checkbox/Date/Select/RadioGroup`, CSS classes `modified valid/invalid` |
| `AuthorizeView`, `[Authorize]`, `AuthenticationState` | `App.Authenticate`, `AuthorizeView`, pages implementing `Authorize(*User) bool` |
| Prerendering, reconnection UI, circuit retention | prerender is on by default and reuses the same circuit (no double `OnInit`); unacked batches are replayed on reconnect; the reconnect overlay only appears after a working connection drops (so crawlers without WebSockets never see it); disconnected circuits live for `App.Retention` (3 min) |
| `MarkupString` | `gozer.Raw` |

## How it works

- Each browser tab is a `Circuit`: one goroutine that runs all component code, so components need no locks.
- `Render()` returns a tree of `gozer.Node`. The renderer diffs it against what is mounted (keyed and positional matching, components keep state by type and position) and emits ops: insert, remove, move, attribute, property, text, listen.
- Ops go out in numbered batches; the client acks each. On reconnect it sends the last applied number and the server replays the rest. An unknown or expired circuit makes the client reload.
- Bound inputs record what the browser already shows when an event arrives, so fast typing is not overwritten by stale echoes, and rejected input is reverted.
- `client/gozer.js` (embedded, ~250 lines, no dependencies) applies ops, forwards events, intercepts same-origin links, and runs JS interop calls.

## Editor support

`gozer lsp` is a language server for `.gozer` and `.go` files. It runs gopls as a child process and gives it an in-memory Go copy of every `.gozer` file in the workspace, rebuilt on each keystroke. Use it instead of gopls.

- In templates: Go completion, hover, definition, references, rename and signature help inside `@expr`, `@(…)`, `@if`/`@for`, `@code`, event handlers, bindings and component attributes. gopls errors are reported on the template line. Also completes HTML tags and attributes, `@` directives, component names after `<`, and component parameters inside a component tag. Auto-imports become `@import` lines.
- In Go files: everything gopls does, plus components and fields declared in `@code`. Definitions and references that land in generated code open the `.gozer` file instead.
- Highlighting: markup uses tree-sitter's HTML grammar. Go code and template syntax (`@`, `@if`/`@for`/`else`, `@code`, `@page`, …, `@* comments *@`) are coloured by semantic tokens, which gozer maps back from gopls. You get the same type-aware colours as in `.go` files. This needs gopls semantic tokens on (`init_options = { semanticTokens = true }`).
- Saving a `.gozer` file regenerates its `.gozer.go` file on disk, so `go build` matches the editor without running `go generate`.

Neovim 0.11+ (put this after your `vim.lsp.config('gopls', …)`; your gopls settings are reused):

```lua
vim.opt.rtp:append('/path/to/gozer/editors/nvim')
require('gozer').setup()            -- <leader>lG switches between gozer and plain gopls
-- require('gozer').setup({ cmd = { 'gozer', 'lsp', '-runtime', 'github.com/you/gozer' }, key = '<leader>gz' })
```

Install the binary with `go install gozer/cmd/gozer`.

## Not included

Virtualize, InputFile, QuickGrid, SectionOutlet, nested layouts, `@switch` in templates, `//line` mapping from generated Go back to `.gozer`, hot reload, and a non-root base path. Editor gaps: Go colouring comes from the server, so templates show HTML-only colours while gozer is off or still loading. Multi-line Go comments and raw strings are not coloured. No formatting or code actions for `.gozer`, and new `.gozer` files are only picked up once opened. Unknown in-app links fall back to a full page load (so static files under the same origin still work).

Caveat: `JSRuntime.Invoke` blocks the circuit until the browser answers. JavaScript called that way must not synchronously wait on `gozer.invoke` back into the same circuit.
