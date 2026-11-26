// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in JSON_FIELDS_LICENSE.

package rudderstackai

import (
	"cmp"
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Adapted from encoding/json Go 1.26.11: selection and omission, without encoder caches. Member
// names come from the running encoding/json (see tagName), whose tag rules change across releases.
// A field represents a single field found in a struct.
type jsonField struct {
	name string

	tag       bool
	index     []int
	typ       reflect.Type
	omitEmpty bool
	omitZero  bool
	isZero    func(reflect.Value) bool
	quoted    bool
}

type isZeroer interface {
	IsZero() bool
}

var isZeroerType = reflect.TypeFor[isZeroer]()

func jsonTypeFields(t reflect.Type) []jsonField {
	// Anonymous fields to explore at the current level and the next.
	current := []jsonField{}
	next := []jsonField{{typ: t}}

	// Count of queued names for current level and the next.
	var count, nextCount map[reflect.Type]int

	// Types already visited at an earlier level.
	visited := map[reflect.Type]bool{}

	// Fields found.
	var fields []jsonField

	for len(next) > 0 {
		current, next = next, current[:0]
		count, nextCount = nextCount, map[reflect.Type]int{}

		for _, f := range current {
			if visited[f.typ] {
				continue
			}
			visited[f.typ] = true

			// Scan f.typ for fields to include.
			for i := 0; i < f.typ.NumField(); i++ {
				sf := f.typ.Field(i)
				if sf.Anonymous {
					t := sf.Type
					if t.Kind() == reflect.Pointer {
						t = t.Elem()
					}
					if !sf.IsExported() && t.Kind() != reflect.Struct {
						// Ignore embedded fields of unexported non-struct types.
						continue
					}
					// Do not ignore embedded fields of unexported struct types
					// since they may have exported fields.
				} else if !sf.IsExported() {
					// Ignore unexported non-embedded fields.
					continue
				}
				tag := sf.Tag.Get("json")
				if tag == "-" {
					continue
				}
				_, opts := parseTag(tag)
				name := tagName(tag)
				index := make([]int, len(f.index)+1)
				copy(index, f.index)
				index[len(f.index)] = i

				ft := sf.Type
				if ft.Name() == "" && ft.Kind() == reflect.Pointer {
					// Follow pointer.
					ft = ft.Elem()
				}

				// Only strings, floats, integers, and booleans can be quoted.
				quoted := false
				if opts.Contains("string") {
					switch ft.Kind() {
					case reflect.Bool,
						reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
						reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
						reflect.Float32, reflect.Float64,
						reflect.String:
						quoted = true
					}
				}

				// Record found field and index sequence.
				if name != "" || !sf.Anonymous || ft.Kind() != reflect.Struct {
					tagged := name != ""
					if name == "" {
						name = sf.Name
					}
					field := jsonField{
						name:      name,
						tag:       tagged,
						index:     index,
						typ:       ft,
						omitEmpty: opts.Contains("omitempty"),
						omitZero:  opts.Contains("omitzero"),
						quoted:    quoted,
					}
					if field.omitZero {
						t := sf.Type
						// Provide a function that uses a type's IsZero method.
						switch {
						case t.Kind() == reflect.Interface && t.Implements(isZeroerType):
							field.isZero = func(v reflect.Value) bool {
								// Avoid panics calling IsZero on a nil interface or
								// non-nil interface with nil pointer.
								return v.IsNil() ||
									(v.Elem().Kind() == reflect.Pointer && v.Elem().IsNil()) ||
									v.Interface().(isZeroer).IsZero()
							}
						case t.Kind() == reflect.Pointer && t.Implements(isZeroerType):
							field.isZero = func(v reflect.Value) bool {
								// Avoid panics calling IsZero on nil pointer.
								return v.IsNil() || v.Interface().(isZeroer).IsZero()
							}
						case t.Implements(isZeroerType):
							field.isZero = func(v reflect.Value) bool {
								return v.Interface().(isZeroer).IsZero()
							}
						case reflect.PointerTo(t).Implements(isZeroerType):
							field.isZero = func(v reflect.Value) bool {
								if !v.CanAddr() {
									// Temporarily box v so we can take the address.
									v2 := reflect.New(v.Type()).Elem()
									v2.Set(v)
									v = v2
								}
								return v.Addr().Interface().(isZeroer).IsZero()
							}
						}
					}

					fields = append(fields, field)
					if count[f.typ] > 1 {
						// Two copies suffice to mark an ambiguous embedded field.
						fields = append(fields, fields[len(fields)-1])
					}
					continue
				}

				// Record new anonymous struct to explore in next round.
				nextCount[ft]++
				if nextCount[ft] == 1 {
					next = append(next, jsonField{name: ft.Name(), index: index, typ: ft})
				}
			}
		}
	}

	slices.SortFunc(fields, func(a, b jsonField) int {
		if c := strings.Compare(a.name, b.name); c != 0 {
			return c
		}
		if c := cmp.Compare(len(a.index), len(b.index)); c != 0 {
			return c
		}
		if a.tag != b.tag {
			if a.tag {
				return -1
			}
			return +1
		}
		return slices.Compare(a.index, b.index)
	})

	// Resolve embedding conflicts by name, giving JSON tags precedence.
	out := fields[:0]
	for advance, i := 0, 0; i < len(fields); i += advance {
		// One iteration per name.
		// Find the sequence of fields with the name of this first field.
		fi := fields[i]
		name := fi.name
		for advance = 1; i+advance < len(fields); advance++ {
			fj := fields[i+advance]
			if fj.name != name {
				break
			}
		}
		if advance == 1 { // Only one field with this name
			out = append(out, fi)
			continue
		}
		dominant, ok := dominantField(fields[i : i+advance])
		if ok {
			out = append(out, dominant)
		}
	}

	fields = out
	slices.SortFunc(fields, func(i, j jsonField) int {
		return slices.Compare(i.index, j.index)
	})

	return fields
}

// dominantField rejects equally shallow fields with the same tag priority.
func dominantField(fields []jsonField) (jsonField, bool) {
	// Depth and tag priority are sorted, so only the first two can be ambiguous.
	if len(fields) > 1 && len(fields[0].index) == len(fields[1].index) && fields[0].tag == fields[1].tag {
		return jsonField{}, false
	}
	return fields[0], true
}

var tagNames sync.Map // json tag -> member name; never holds application values.

// tagName is the member name the running encoding/json gives a field with this json tag, or ""
// when it uses the Go field name. It encodes one-field structs that differ only in the field's
// name, so no application code runs.
func tagName(tag string) string {
	if tag == "" || strings.HasPrefix(tag, ",") {
		return ""
	}
	if name, ok := tagNames.Load(tag); ok {
		return name.(string)
	}
	probe := func(field string) (string, bool) {
		t := reflect.StructOf([]reflect.StructField{{Name: field, Type: reflect.TypeFor[int](), Tag: reflect.StructTag("json:" + strconv.Quote(tag))}})
		v := reflect.New(t).Elem()
		v.Field(0).SetInt(1)
		b, err := json.Marshal(v.Interface())
		var members map[string]json.RawMessage
		if err != nil || json.Unmarshal(b, &members) != nil || len(members) != 1 {
			return "", false
		}
		for name := range members {
			return name, true
		}
		return "", false
	}
	name, ok := probe("A")
	if other, _ := probe("B"); !ok || other != name {
		name = ""
	}
	tagNames.Store(tag, name)
	return name
}

func isEmptyValue(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64,
		reflect.Interface, reflect.Pointer:
		return v.IsZero()
	}
	return false
}

var jsonFieldCache sync.Map // reflect.Type -> []jsonField; never holds application values.

func cachedJSONFields(t reflect.Type) []jsonField {
	if f, ok := jsonFieldCache.Load(t); ok {
		return f.([]jsonField)
	}
	f, _ := jsonFieldCache.LoadOrStore(t, jsonTypeFields(t))
	return f.([]jsonField)
}

// jsonFieldValue follows the selected promotion path, skipping nil embedded pointers.
func jsonFieldValue(rv reflect.Value, f jsonField) (reflect.Value, bool) {
	for _, n := range f.index {
		if rv.Kind() == reflect.Pointer {
			if rv.IsNil() {
				return reflect.Value{}, false
			}
			rv = rv.Elem()
		}
		rv = rv.Field(n)
	}
	if f.omitEmpty && isEmptyValue(rv) {
		return rv, false
	}
	if f.omitZero && ((f.isZero == nil && rv.IsZero()) || (f.isZero != nil && f.isZero(rv))) {
		return rv, false
	}
	return rv, true
}

// tagOptions is the string following a comma in a struct field's "json"
// tag, or the empty string. It does not include the leading comma.
type tagOptions string

// parseTag splits a struct field's json tag into its name and
// comma-separated options.
func parseTag(tag string) (string, tagOptions) {
	tag, opt, _ := strings.Cut(tag, ",")
	return tag, tagOptions(opt)
}

// Contains reports whether a comma-separated list of options
// contains a particular substr flag. substr must be surrounded by a
// string boundary or commas.
func (o tagOptions) Contains(optionName string) bool {
	if len(o) == 0 {
		return false
	}
	s := string(o)
	for s != "" {
		var name string
		name, s, _ = strings.Cut(s, ",")
		if name == optionName {
			return true
		}
	}
	return false
}
