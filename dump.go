package tracepanel

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Dump prints values to stderr as a debug print does, each with the expression that gave it, its pretty Go-syntax
// text, its type and its length, under the line that called it:
//
//	tracepanel.Dump(ctx, order, user.Roles)
//
//	handlers.go:42 checkout
//	    order: &main.Order{
//	        ID:    7,
//	        Items: []main.Item{},
//	    } (*main.Order)
//	    user.Roles: []string{"admin"} ([]string) len=1
//
// It always prints, also when Tracepanel isn't recording. When ctx carries a trace, the dump is recorded too, a log
// record on its current span that the console shows in the call tree. A value whose expression names a secret
// (password, token, api_key) is masked, printed and recorded.
func Dump(ctx context.Context, values ...any) {
	pc, file, line, ok := runtime.Caller(1)
	function := ""
	if ok {
		function = shortFunction(runtime.FuncForPC(pc).Name())
	}
	exprs := dumpExprs(file, line, len(values))
	args := make([]any, 0, len(values))
	lines := make([]string, 0, len(values)+1)
	lines = append(lines, fmt.Sprintf("%s:%d %s", relative(file), line, function))
	for i, value := range values {
		var expr *string
		if exprs != nil {
			expr = exprs[i]
		}
		entry, text := dumpArg(expr, value)
		args = append(args, entry)
		lines = append(lines, text)
	}
	message := strings.Join(lines, "\n    ")
	writeDump(message + "\n")
	s := scopeOf(ctx)
	if s == nil {
		return
	}
	attrs := attrMap{"log.kind": "dump", "dump.lang": "go", "dump.args": args}
	attrs["code.origin"] = nil
	if ok {
		attrs["code.origin"] = fmt.Sprintf("%s:%d in %s", relative(file), line, function)
	}
	s.rec.log(logDoc{
		SpanID: ptr(s.parent()), Time: micros(time.Now()), Level: "debug", Logger: "tracepanel",
		Message: message, Attrs: attrs,
	})
}

// dumpOutput is where Dump prints: stderr (a test's buffer).
var (
	dumpMu     sync.Mutex
	dumpOutput io.Writer = os.Stderr
)

func writeDump(text string) {
	dumpMu.Lock()
	defer dumpMu.Unlock()
	_, _ = io.WriteString(dumpOutput, text)
}

// dumpEntry is one value of a dump, in dump.args.
type dumpEntry struct {
	Expr *string `json:"expr"`
	Repr string  `json:"repr"`
	Type string  `json:"type"`
	Len  *int    `json:"len"`
}

// dumpArg is a value's entry in dump.args and its line of the printed text: `expr: repr (type) len=N`, the repr's
// later lines indented under it.
func dumpArg(expr *string, value any) (dumpEntry, string) {
	entry := dumpEntry{Expr: expr, Type: fmt.Sprintf("%T", value)}
	if expr != nil && secret.MatchString(*expr) {
		entry.Repr = mask
	} else {
		entry.Repr = prettyValue(value)
		if v := reflect.ValueOf(value); v.IsValid() {
			switch v.Kind() {
			case reflect.Slice, reflect.Array, reflect.Map, reflect.String, reflect.Chan:
				n := v.Len()
				entry.Len = &n
			}
		}
	}
	var text strings.Builder
	if expr != nil {
		text.WriteString(*expr + ": ")
	}
	text.WriteString(strings.ReplaceAll(entry.Repr, "\n", "\n    "))
	text.WriteString(" (" + entry.Type + ")")
	if entry.Len != nil {
		text.WriteString(" len=" + strconv.Itoa(*entry.Len))
	}
	return entry, text.String()
}

// ---------- the expressions, from the call's source ----------

// readSource reads a source file of the app (a test's stand-in).
var readSource = os.ReadFile

type parsedSource struct {
	fset *token.FileSet
	file *ast.File
	data []byte
}

// sources are the files a Dump was called from, parsed once each (nil: unreadable or not Go).
var (
	sourcesMu sync.Mutex
	sources   = map[string]*parsedSource{}
)

func sourceOf(file string) *parsedSource {
	sourcesMu.Lock()
	defer sourcesMu.Unlock()
	if source, seen := sources[file]; seen {
		return source
	}
	var source *parsedSource
	if data, err := readSource(file); err == nil {
		fset := token.NewFileSet()
		if tree, err := parser.ParseFile(fset, file, data, parser.SkipObjectResolution); err == nil {
			source = &parsedSource{fset: fset, file: tree, data: data}
		}
	}
	sources[file] = source
	return source
}

// dumpExprs are the source text of each value the Dump call at file:line was given, its context left out: nil when
// the call can't be told (no source, two calls on the line, a spread slice), an entry nil for a literal.
func dumpExprs(file string, line, n int) []*string {
	source := sourceOf(file)
	if source == nil {
		return nil
	}
	var found []*ast.CallExpr
	ast.Inspect(source.file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || !isDumpCall(call) {
			return true
		}
		if source.fset.Position(call.Pos()).Line <= line && line <= source.fset.Position(call.End()).Line {
			found = append(found, call)
		}
		return true
	})
	if len(found) != 1 || found[0].Ellipsis.IsValid() || len(found[0].Args) != n+1 {
		return nil
	}
	exprs := make([]*string, n)
	for i, arg := range found[0].Args[1:] {
		if isLiteral(arg) {
			continue
		}
		start, end := source.fset.Position(arg.Pos()).Offset, source.fset.Position(arg.End()).Offset
		if start < 0 || end > len(source.data) || start >= end {
			return nil
		}
		text := string(source.data[start:end])
		exprs[i] = &text
	}
	return exprs
}

func isDumpCall(call *ast.CallExpr) bool {
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		return fun.Sel.Name == "Dump"
	case *ast.Ident:
		return fun.Name == "Dump"
	}
	return false
}

// isLiteral: a value written as it is ("here", 42, -1), which needs no name.
func isLiteral(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.BasicLit:
		return true
	case *ast.ParenExpr:
		return isLiteral(e.X)
	case *ast.UnaryExpr:
		return isLiteral(e.X)
	}
	return false
}

// ---------- the pretty printer ----------

const (
	// How much of one value's text is kept, in characters; a longer one is cut there and marked.
	maxRepr = 16 * 1024
	cut     = "… (truncated)"
	// How deep the printer goes into nested values; deeper ones are {…}.
	maxDepth = 6
	// A list or a map whose elements fit on one line of this width is printed on one.
	lineWidth = 80
	// The most bytes of binary data printed.
	maxBytes = 32
)

// prettyValue is a value as multi-line Go syntax: `main.Order{\n    ID: 7,\n}`, cut at maxRepr characters.
func prettyValue(value any) string {
	p := &printer{seen: map[uintptr]bool{}, written: new(int)}
	p.value(reflect.ValueOf(value), 0)
	text := p.out.String()
	if utf8.RuneCountInString(text) > maxRepr {
		runes := []rune(text)
		text = string(runes[:maxRepr]) + cut
	}
	return text
}

type printer struct {
	out strings.Builder
	// The pointers and maps on the path to the value printed: one met again is a cycle.
	seen map[uintptr]bool
	// What every printer of the value wrote so far, in bytes.
	written *int
}

func (p *printer) write(text string) {
	*p.written += len(text)
	p.out.WriteString(text)
}

// full: the text is past what is kept, the rest isn't printed.
func (p *printer) full() bool { return *p.written > maxRepr*4 }

func (p *printer) value(v reflect.Value, depth int) {
	if !v.IsValid() {
		p.write("nil")
		return
	}
	if p.text(v) {
		return
	}
	t := v.Type()
	switch v.Kind() {
	case reflect.Bool:
		p.write(strconv.FormatBool(v.Bool()))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		p.write(strconv.FormatInt(v.Int(), 10))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		p.write(strconv.FormatUint(v.Uint(), 10))
	case reflect.Float32, reflect.Float64:
		p.write(strconv.FormatFloat(v.Float(), 'g', -1, t.Bits()))
	case reflect.Complex64, reflect.Complex128:
		p.write(strconv.FormatComplex(v.Complex(), 'g', -1, t.Bits()))
	case reflect.String:
		p.write(strconv.Quote(v.String()))
	case reflect.Interface:
		if v.IsNil() {
			p.write("nil")
			return
		}
		p.value(v.Elem(), depth)
	case reflect.Pointer:
		p.pointer(v, depth)
	case reflect.Struct:
		p.structure(v, depth)
	case reflect.Slice:
		if v.IsNil() {
			p.write("(" + t.String() + ")(nil)")
			return
		}
		if t.Elem().Kind() == reflect.Uint8 {
			p.bytes(v)
			return
		}
		key := v.Pointer()
		if v.Len() > 0 && p.seen[key] {
			p.write("<cycle: " + t.String() + ">")
			return
		}
		if v.Len() > 0 {
			p.seen[key] = true
			defer delete(p.seen, key)
		}
		p.list(v, depth)
	case reflect.Array:
		p.list(v, depth)
	case reflect.Map:
		p.mapping(v, depth)
	case reflect.Chan:
		if v.IsNil() {
			p.write("(" + t.String() + ")(nil)")
			return
		}
		p.write(fmt.Sprintf("(%s)(len=%d cap=%d)", t, v.Len(), v.Cap()))
	case reflect.Func:
		if v.IsNil() {
			p.write("(" + t.String() + ")(nil)")
			return
		}
		name := "func"
		if fn := runtime.FuncForPC(v.Pointer()); fn != nil {
			name = fn.Name()
		}
		p.write("(" + t.String() + ")(" + name + ")")
	case reflect.UnsafePointer:
		p.write(fmt.Sprintf("unsafe.Pointer(%#x)", v.Pointer()))
	default:
		p.write(t.String())
	}
}

// text prints a value that tells itself: as its type with its text (an error's Error, a Stringer's String:
// `time.Time("2026-10-31 09:00:00 +0000 UTC")`), else as the Go syntax its GoString writes. Only a value the printer may use: not one read
// from an unexported field, nor a nil pointer.
func (p *printer) text(v reflect.Value) (done bool) {
	if !v.CanInterface() || (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) && v.IsNil() {
		return false
	}
	var text string
	defer func() {
		if recover() != nil {
			// A method that panics: the value as it is made instead.
			done = false
		}
	}()
	switch value := v.Interface().(type) {
	case error:
		text = value.Error()
	case fmt.Stringer:
		text = value.String()
	case fmt.GoStringer:
		p.write(value.GoString())
		return true
	default:
		return false
	}
	p.write(v.Type().String() + "(" + strconv.Quote(text) + ")")
	return true
}

func (p *printer) pointer(v reflect.Value, depth int) {
	t := v.Type()
	if v.IsNil() {
		p.write("(" + t.String() + ")(nil)")
		return
	}
	key := v.Pointer()
	if p.seen[key] {
		p.write("<cycle: " + t.String() + ">")
		return
	}
	p.seen[key] = true
	defer delete(p.seen, key)
	p.write("&")
	p.value(v.Elem(), depth)
}

func (p *printer) structure(v reflect.Value, depth int) {
	t := v.Type()
	if v.NumField() == 0 {
		p.write(t.String() + "{}")
		return
	}
	if depth >= maxDepth {
		p.write(t.String() + "{…}")
		return
	}
	width := 0
	for i := range v.NumField() {
		width = max(width, len(t.Field(i).Name))
	}
	indent := strings.Repeat("    ", depth+1)
	p.write(t.String() + "{\n")
	for i := range v.NumField() {
		if p.full() {
			break
		}
		name := t.Field(i).Name
		p.write(indent + name + ":" + strings.Repeat(" ", width-len(name)+1))
		p.value(v.Field(i), depth+1)
		p.write(",\n")
	}
	p.write(strings.Repeat("    ", depth) + "}")
}

// list prints a slice's or an array's elements: on one line when they fit, else one a line.
func (p *printer) list(v reflect.Value, depth int) {
	t := v.Type().String()
	if v.Len() == 0 {
		p.write(t + "{}")
		return
	}
	if depth >= maxDepth {
		p.write(t + "{…}")
		return
	}
	items := make([]string, 0, v.Len())
	for i := range v.Len() {
		if p.full() {
			break
		}
		items = append(items, p.nested(v.Index(i), depth+1))
	}
	p.items(t, items, depth)
}

func (p *printer) mapping(v reflect.Value, depth int) {
	t := v.Type().String()
	if v.IsNil() {
		p.write("(" + t + ")(nil)")
		return
	}
	if v.Len() == 0 {
		p.write(t + "{}")
		return
	}
	key := v.Pointer()
	if p.seen[key] {
		p.write("<cycle: " + t + ">")
		return
	}
	if depth >= maxDepth {
		p.write(t + "{…}")
		return
	}
	p.seen[key] = true
	defer delete(p.seen, key)
	keys := v.MapKeys()
	sort.Slice(keys, func(i, j int) bool { return lessKey(keys[i], keys[j]) })
	items := make([]string, 0, len(keys))
	for _, k := range keys {
		if p.full() {
			break
		}
		items = append(items, p.nested(k, depth+1)+": "+p.nested(v.MapIndex(k), depth+1))
	}
	p.items(t, items, depth)
}

// nested is a value's text, printed apart: an element of a list or a map.
func (p *printer) nested(v reflect.Value, depth int) string {
	inner := &printer{seen: p.seen, written: p.written}
	inner.value(v, depth)
	return inner.out.String()
}

func (p *printer) items(t string, items []string, depth int) {
	oneLine := t + "{" + strings.Join(items, ", ") + "}"
	if !strings.Contains(oneLine, "\n") && len(oneLine)+4*depth <= lineWidth {
		p.write(oneLine)
		return
	}
	indent := strings.Repeat("    ", depth+1)
	p.write(t + "{\n")
	for _, item := range items {
		p.write(indent + item + ",\n")
	}
	p.write(strings.Repeat("    ", depth) + "}")
}

// bytes prints a []byte as its text when it is printable (`[]byte("{}")`), else as hex, the first maxBytes of it.
func (p *printer) bytes(v reflect.Value) {
	t := v.Type().String()
	data := v.Bytes()
	if utf8.Valid(data) && printable(string(data)) {
		p.write(t + "(" + strconv.Quote(string(data)) + ")")
		return
	}
	hex := make([]string, 0, min(len(data), maxBytes)+1)
	for _, b := range data[:min(len(data), maxBytes)] {
		hex = append(hex, fmt.Sprintf("0x%02x", b))
	}
	if len(data) > maxBytes {
		hex = append(hex, fmt.Sprintf("… (%d bytes)", len(data)))
	}
	p.write(t + "{" + strings.Join(hex, ", ") + "}")
}

func printable(text string) bool {
	for _, r := range text {
		if r < ' ' && r != '\n' && r != '\t' && r != '\r' || r == 0x7f {
			return false
		}
	}
	return true
}

// lessKey orders a map's keys: numbers by value, strings and the rest by their text.
func lessKey(a, b reflect.Value) bool {
	if a.Kind() == b.Kind() {
		switch a.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return a.Int() < b.Int()
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			return a.Uint() < b.Uint()
		case reflect.Float32, reflect.Float64:
			return a.Float() < b.Float()
		case reflect.String:
			return a.String() < b.String()
		case reflect.Bool:
			return !a.Bool() && b.Bool()
		}
	}
	return fmt.Sprint(a) < fmt.Sprint(b)
}
