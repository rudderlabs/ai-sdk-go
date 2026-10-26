package main

import (
	_ "embed"
	"encoding/json"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
)

// JSON Schema validation with one message per failed keyword, worded the same in every AI SDK's
// checker. The validators differ between languages, so the checker words its own messages from the
// failing keyword, the schema's own value of it and the instance location, and never echoes the
// captured value.

//go:embed schemas/ai-events-v0.3.schema.json
var specSchema []byte

//go:embed schemas/ai-client-events-v0.2.schema.json
var clientSchema []byte

// schemaDoc is one schema file.
type schemaDoc struct {
	id   string
	defs *Object // the $defs entries, as read
	raw  any     // the document as compiled (see split), to read keyword values from
}

// schemas validates against the spec and browser-event schemas, one $defs entry at a time.
type schemas struct {
	spec, client *schemaDoc
	byID         map[string]*schemaDoc
	compiler     *jsonschema.Compiler
	compiled     map[string]*jsonschema.Schema
}

// readSchema reads a schema file, or the bundled one when path is empty.
func readSchema(path string, bundled []byte) (*schemaDoc, error) {
	b, name := bundled, "bundled schema"
	if path != "" {
		var err error
		if b, err = os.ReadFile(path); err != nil {
			return nil, captureErr("%s: cannot read schema", path)
		}
		name = path
	}
	var v any
	var err error
	if v, err = parseSchemaJSON(b); err != nil {
		return nil, captureErr("%s: cannot read schema", name)
	}
	o, ok := v.(*Object)
	if !ok {
		return nil, captureErr("%s: cannot read schema", name)
	}
	id, ok := o.Get("$id").(string)
	if !ok {
		return nil, captureErr("%s: cannot read schema", name)
	}
	return &schemaDoc{id: id, defs: obj(o.Get("$defs")), raw: split(o)}, nil
}

// parseSchemaJSON reads a whole schema file as Python's json.load does.
func parseSchemaJSON(b []byte) (any, error) {
	s := string(b)
	if !utf8.ValidString(s) {
		return nil, errSyntax
	}
	return parseJSON(s)
}

func newSchemas(spec, client *schemaDoc) (*schemas, error) {
	c := jsonschema.NewCompiler()
	byID := map[string]*schemaDoc{}
	for _, d := range []*schemaDoc{spec, client} {
		if err := c.AddResource(d.id, toValidator(d.raw)); err != nil {
			return nil, captureErr("%s: cannot read schema", d.id)
		}
		byID[d.id] = d
	}
	return &schemas{spec: spec, client: client, byID: byID, compiler: c, compiled: map[string]*jsonschema.Schema{}}, nil
}

// errors returns the sorted, distinct messages for instance against doc's $defs/<name>.
func (s *schemas) errors(doc *schemaDoc, name string, instance *Object) ([]string, error) {
	ref := doc.id + "#/$defs/" + url.PathEscape(strings.ReplaceAll(strings.ReplaceAll(name, "~", "~0"), "/", "~1"))
	sch, ok := s.compiled[ref]
	if !ok {
		var err error
		if sch, err = s.compiler.Compile(ref); err != nil {
			return nil, captureErr("%s: cannot compile $defs/%s", doc.id, name)
		}
		s.compiled[ref] = sch
	}
	err := sch.Validate(toValidator(instance))
	if err == nil {
		return nil, nil
	}
	found := map[string]bool{}
	s.collect(err.(*jsonschema.ValidationError), instance, nil, found)
	out := make([]string, 0, len(found))
	for m := range found {
		out = append(out, m)
	}
	sort.Strings(out)
	return out, nil
}

// collect adds the leaf errors' messages: allOf, $ref, properties, items, dependentSchemas and
// if/then/else report their failing children; anyOf, oneOf and not are one error each.
// at overrides the instance location (propertyNames errors sit at the object).
func (s *schemas) collect(e *jsonschema.ValidationError, instance any, at []string, found map[string]bool) {
	loc := e.InstanceLocation
	if at != nil {
		loc = at
	}
	schemaObj, ptr := s.schemaAt(e.SchemaURL)
	// unevaluatedProperties and unevaluatedItems are one error at the object, as in Python.
	if n := len(ptr); n > 0 && (ptr[n-1] == "unevaluatedProperties" || ptr[n-1] == "unevaluatedItems") && len(loc) > 0 {
		found[location(instance, loc[:len(loc)-1])+": fails "+ptr[n-1]] = true
		return
	}
	switch k := e.ErrorKind.(type) {
	case *kind.Schema, *kind.Group, *kind.AllOf, *kind.Reference:
		for _, c := range e.Causes {
			s.collect(c, instance, at, found)
		}
		return
	case *kind.PropertyNames:
		for _, c := range e.Causes {
			s.collect(c, instance, loc, found)
		}
		return
	case *kind.Required:
		target, _ := walk(instance, loc).(*Object)
		for _, name := range strs(schemaObj.Get("required")) {
			if target != nil && !target.Has(name) {
				found[location(instance, loc)+": missing required "+reprStr(name)] = true
			}
		}
		return
	case *kind.AdditionalProperties:
		allowed := obj(schemaObj.Get("properties"))
		for _, name := range obj(walk(instance, loc)).Keys() {
			if !allowed.Has(name) {
				found[location(instance, loc)+": unexpected "+reprStr(name)] = true
			}
		}
		return
	case *kind.FalseSchema:
		// Python's validator reports a false subschema at the object or array that holds the value.
		if len(loc) == 0 || !entersValue(ptr) {
			found[location(instance, loc)+": not allowed"] = true
		} else if n := len(ptr); ptr[n-1] == "items" && (n < 2 || !schemaMapKeywords[ptr[n-2]]) {
			found[location(instance, loc[:len(loc)-1])+": fails items"] = true // items: false
		} else {
			found[location(instance, loc[:len(loc)-1])+": not allowed"] = true
		}
	default:
		found[location(instance, loc)+": "+message(k, schemaObj)] = true
	}
}

// entersValue reports whether a subschema at ptr applies to a property or item of the instance
// it is reached from (properties, patternProperties, prefixItems, items; a false additionalProperties
// is its own error kind).
func entersValue(ptr []string) bool {
	n := len(ptr)
	if n >= 2 && (ptr[n-2] == "properties" || ptr[n-2] == "patternProperties" || ptr[n-2] == "prefixItems") {
		return true
	}
	return n >= 1 && ptr[n-1] == "items" && (n < 2 || !schemaMapKeywords[ptr[n-2]])
}

// message words one failed keyword from the schema's value of it.
func message(k jsonschema.ErrorKind, schemaObj *Object) string {
	switch k := k.(type) {
	case *kind.Type:
		return "not of type " + types(schemaObj.Get("type"))
	case *kind.Enum:
		return "not one of " + repr(schemaObj.Get("enum"))
	case *kind.Const:
		return "not " + repr(schemaObj.Get("const"))
	case *kind.MinLength:
		return "shorter than " + repr(schemaObj.Get("minLength"))
	case *kind.MaxLength:
		return "longer than " + repr(schemaObj.Get("maxLength"))
	case *kind.Minimum:
		return "less than " + repr(schemaObj.Get("minimum"))
	case *kind.Maximum:
		return "greater than " + repr(schemaObj.Get("maximum"))
	case *kind.Pattern:
		return "does not match " + repr(schemaObj.Get("pattern"))
	case *kind.UniqueItems:
		return "items not unique"
	case *kind.Not:
		return "matches a schema it must not"
	case *kind.AnyOf:
		return "matches none of anyOf"
	case *kind.OneOf:
		return "does not match exactly one of oneOf"
	case *kind.FalseSchema:
		return "not allowed"
	case *kind.MinContains:
		if len(k.Got) == 0 { // Python reports no match at all as contains
			return "fails contains"
		}
	}
	return "fails " + k.KeywordPath()[0]
}

func types(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return strings.Join(strs(v), " or ")
}

func strs(v any) []string {
	var out []string
	if list, ok := v.([]any); ok {
		for _, item := range list {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// schemaAt returns the schema object at a compiled schema location and its JSON pointer tokens.
func (s *schemas) schemaAt(loc string) (*Object, []string) {
	base, frag, _ := strings.Cut(loc, "#")
	doc, ok := s.byID[base]
	if !ok {
		return newObject(), nil
	}
	frag, err := url.PathUnescape(frag)
	if err != nil || frag == "" {
		return obj(doc.raw), nil
	}
	var ptr []string
	v := doc.raw
	for _, tok := range strings.Split(frag, "/")[1:] {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		ptr = append(ptr, tok)
		v = child(v, tok)
	}
	return obj(v), ptr
}

// location is "properties" followed by ".key" or "[index]" per step into the instance.
func location(instance any, path []string) string {
	var b strings.Builder
	b.WriteString("properties")
	v := instance
	for _, tok := range path {
		if _, ok := v.([]any); ok {
			b.WriteString("[" + tok + "]")
		} else {
			b.WriteString("." + originalKey(v, tok))
		}
		v = child(v, tok)
	}
	return b.String()
}

// walk follows an instance location.
func walk(v any, path []string) any {
	for _, tok := range path {
		v = child(v, tok)
	}
	return v
}

// child steps into an object key or array index as the validator names it.
func child(v any, tok string) any {
	switch v := v.(type) {
	case *Object:
		return v.Get(originalKey(v, tok))
	case []any:
		if i, err := strconv.Atoi(tok); err == nil && i >= 0 && i < len(v) {
			return v[i]
		}
	}
	return nil
}

// originalKey maps a key the validator saw (see validatorString) back to the object's own.
func originalKey(v any, tok string) string {
	if o, ok := v.(*Object); ok && !o.Has(tok) {
		for _, k := range o.keys {
			if validatorString(k) == tok {
				return k
			}
		}
	}
	return tok
}

// --- the document the validator compiles ---

// The validator stops at a failed type, const, enum or format and skips the schema's other
// keywords; Python's reports them all. split moves those keywords into allOf entries of their own,
// so each is checked independently; locations and the other keywords stay where they were.
var shortCircuit = []string{"type", "const", "enum", "format"}

// Keywords whose value is a schema, an object of schemas or a list of schemas (2020-12 and older).
var (
	schemaKeywords = map[string]bool{
		"additionalProperties": true, "propertyNames": true, "items": true, "contains": true, "not": true,
		"if": true, "then": true, "else": true, "unevaluatedProperties": true, "unevaluatedItems": true,
		"contentSchema": true, "additionalItems": true,
	}
	schemaMapKeywords = map[string]bool{
		"$defs": true, "definitions": true, "properties": true, "patternProperties": true, "dependentSchemas": true,
	}
	schemaListKeywords = map[string]bool{"allOf": true, "anyOf": true, "oneOf": true, "prefixItems": true, "items": true}
)

func split(v any) any {
	o, ok := v.(*Object)
	if !ok {
		return v
	}
	out := newObject()
	var moved []any
	for _, k := range o.keys {
		val := o.vals[k]
		switch {
		case isShortCircuit(k) && hasOtherKeywords(o):
			one := newObject()
			one.Set(k, val)
			moved = append(moved, one)
			continue
		case schemaMapKeywords[k]:
			if m, ok := val.(*Object); ok {
				sub := newObject()
				for _, name := range m.keys {
					sub.Set(name, split(m.vals[name]))
				}
				val = sub
			}
		case schemaListKeywords[k] && isList(val):
			list := val.([]any)
			sub := make([]any, len(list))
			for i, item := range list {
				sub[i] = split(item)
			}
			val = sub
		case schemaKeywords[k]:
			val = split(val)
		}
		out.Set(k, val)
	}
	if len(moved) > 0 {
		all, _ := out.Get("allOf").([]any)
		out.Set("allOf", append(append([]any{}, all...), moved...))
	}
	return out
}

func isList(v any) bool {
	_, ok := v.([]any)
	return ok
}

func isShortCircuit(k string) bool {
	for _, s := range shortCircuit {
		if k == s {
			return true
		}
	}
	return false
}

// hasOtherKeywords reports whether a short-circuiting keyword shares the schema with another
// assertion, so that moving it out changes what is reported.
func hasOtherKeywords(o *Object) bool {
	n := 0
	for _, k := range o.keys {
		switch k {
		case "$schema", "$id", "$comment", "title", "description", "examples", "default", "$defs", "definitions":
		default:
			n++
		}
	}
	return n > 1
}

// toValidator converts a value for the validator: maps, json.Number, and strings whose lone
// surrogates (not valid in a Go string) become private-use characters, one code point each.
func toValidator(v any) any {
	switch v := v.(type) {
	case *Object:
		m := make(map[string]any, v.Len())
		for _, k := range v.keys {
			m[validatorString(k)] = toValidator(v.vals[k])
		}
		return m
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = toValidator(item)
		}
		return out
	case string:
		return validatorString(v)
	case Int:
		return json.Number(v)
	case Float:
		if !isFinite(v) { // a 64-bit inf: larger than any finite number, and not an integer
			n := "1" + strings.Repeat("0", 400) + ".5"
			if v < 0 {
				n = "-" + n
			}
			return json.Number(n)
		}
		return json.Number(strconv.FormatFloat(float64(v), 'g', -1, 64))
	}
	return v
}

func validatorString(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := decodeRune(s[i:])
		i += n
		if r >= 0xd800 && r <= 0xdfff {
			r = 0x100000 + (r - 0xd800)
		}
		b.WriteRune(r)
	}
	return b.String()
}
