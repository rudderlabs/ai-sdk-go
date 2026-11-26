package rudderstackai

import (
	"errors"
	"reflect"
	"strconv"
)

var (
	errUnsizable  = errors.New("rudderstackai: a value could not be encoded as JSON")
	errCycle      = errors.New("rudderstackai: a cycle")
	errSerializer = errors.New("rudderstackai: an app serializer is in the value")
)

// sizer is len(compactJSON(v)) for any value without expanding shared references: each map,
// slice and pointer is measured once and its size added at every place it appears.
type sizer struct {
	memo   map[visit]int
	active map[visit]bool
}

func newSizer() *sizer { return &sizer{memo: map[visit]int{}, active: map[visit]bool{}} }

func (z *sizer) leaf(v any) (int, error) {
	b, err := compactJSON(v)
	return len(b), err
}

// text is the size of s as a JSON string: plain printable ASCII needs no encoder.
func (z *sizer) text(s string) (int, error) {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c >= 0x80 || c == '"' || c == '\\' {
			return z.leaf(s)
		}
	}
	return len(s) + 2, nil
}

func (z *sizer) shared(rv reflect.Value, measure func() (int, error)) (int, error) {
	key, stable := serializerIdentity(rv)
	if !stable {
		return measure()
	}
	if n, ok := z.memo[key]; ok {
		return n, nil
	}
	if z.active[key] {
		return 0, errCycle
	}
	z.active[key] = true
	defer delete(z.active, key)
	n, err := measure()
	if err == nil {
		z.memo[key] = n
	}
	return n, err
}

func (z *sizer) size(rv reflect.Value) (int, error) {
	if !rv.IsValid() {
		return 4, nil
	}
	if nonData(rv.Kind()) {
		return 0, errUnsizable
	}
	if rv.Type() == rawMessageType { // decoded by the walk, never run as app code
		return z.leaf(rv.Interface())
	}
	if _, custom := customTarget(rv); custom {
		return 0, errSerializer // never run an app serializer outside the report's cache
	}
	switch rv.Kind() {
	case reflect.Interface:
		if rv.IsNil() {
			return 4, nil
		}
		return z.size(rv.Elem())
	case reflect.Pointer:
		if rv.IsNil() {
			return 4, nil
		}
		return z.shared(rv, func() (int, error) { return z.size(rv.Elem()) })
	case reflect.Slice, reflect.Array:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			return z.leaf(rv.Interface())
		}
		if rv.Kind() == reflect.Slice {
			if rv.IsNil() {
				return 4, nil
			}
			if rv.Len() > 0 {
				return z.shared(rv, func() (int, error) { return z.list(rv) })
			}
		}
		return z.list(rv)
	case reflect.Map:
		if rv.IsNil() {
			return 4, nil
		}
		if !legacyValidMapKey(rv.Type().Key()) {
			return 0, errUnsizable
		}
		return z.shared(rv, func() (int, error) { return z.object(rv) })
	case reflect.Struct:
		return z.fields(rv)
	case reflect.Bool:
		if rv.Bool() {
			return 4, nil
		}
		return 5, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return len(strconv.FormatInt(rv.Int(), 10)), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return len(strconv.FormatUint(rv.Uint(), 10)), nil
	case reflect.String:
		if rv.Type() != jsonNumberType {
			return z.text(rv.String())
		}
		return z.leaf(rv.Interface())
	case reflect.Float32, reflect.Float64:
		return z.leaf(rv.Interface())
	}
	return 0, errUnsizable // channels, funcs, complex numbers, unsafe pointers
}

func (z *sizer) list(rv reflect.Value) (int, error) {
	total := 2
	for n := 0; n < rv.Len(); n++ {
		s, err := z.size(rv.Index(n))
		if err != nil {
			return 0, err
		}
		total += s
	}
	return total + max(rv.Len()-1, 0), nil
}

func (z *sizer) object(rv reflect.Value) (int, error) {
	if kt := rv.Type().Key(); kt.Kind() != reflect.String && kt.Implements(textType) {
		return 0, errSerializer
	}
	total := 2 + max(rv.Len()-1, 0)
	for it := rv.MapRange(); it.Next(); {
		name, ok := legacyMapKey(it.Key())
		if !ok {
			return 0, errUnsizable
		}
		k, err := z.text(name)
		if err != nil {
			return 0, err
		}
		s, err := z.size(it.Value())
		if err != nil {
			return 0, err
		}
		total += k + 1 + s
	}
	return total, nil
}

func (z *sizer) fields(rv reflect.Value) (int, error) {
	total, count := 2, 0
	for _, f := range cachedJSONFields(rv.Type()) {
		fv, ok := jsonFieldValue(rv, f)
		if !ok {
			continue
		}
		k, err := z.leaf(f.name)
		if err != nil {
			return 0, err
		}
		var s int
		switch _, custom := customTarget(fv); {
		case f.quoted && !custom && quotable(fv):
			// ,string wraps a scalar's encoding, or a pointer to one, in a JSON string.
			if fv.Kind() == reflect.Pointer && fv.IsNil() {
				s = 4
				break
			}
			for fv.Kind() == reflect.Pointer {
				fv = fv.Elem()
			}
			b, encErr := compactJSON(fv.Interface())
			if encErr == nil {
				s, err = z.leaf(string(b))
			} else {
				err = encErr
			}
		default:
			s, err = z.size(fv)
		}
		if err != nil {
			return 0, err
		}
		total += k + 1 + s
		count++
	}
	return total + max(count-1, 0), nil
}

// quotable is a field value ,string applies to: a bool, number or string, or a pointer to one.
func quotable(v reflect.Value) bool {
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return true
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.String:
		return true
	}
	return false
}

// fieldSize is base's encoded size of one captured value. orig is measured itself, exactly at
// any size; a value with an app serializer in it, or a cycle, is sized from its captured copy.
func fieldSize(orig, copied any, fields bool, in *inspector, z *sizer) int {
	if _, ok := orig.(string); ok {
		return captureSize(orig)
	}
	within, custom := in.shape(orig, fields)
	if !custom {
		if within {
			return captureSize(orig)
		}
		n, err := z.size(reflect.ValueOf(orig))
		if err == nil {
			return n
		}
		if !errors.Is(err, errCycle) && !errors.Is(err, errSerializer) {
			return -1
		}
	}
	return resolvedSize(orig, copied, fields)
}

// inspector counts a value's JSON tree, a shared value at each place, once per identity: it says
// whether the tree fits the budget (so encoding it directly is bounded) and whether an app
// serializer is in it.
type inspector struct {
	memo   map[visit]counted
	active map[visit]bool
}

type counted struct {
	n      int
	custom bool
}

func newInspector() *inspector {
	return &inspector{memo: map[visit]counted{}, active: map[visit]bool{}}
}

// shape is within (at most maxWalkNodes values) and custom (a serializer, looked for while the
// tree fits). With fields, each value of the map is its own captured value.
func (in *inspector) shape(v any, fields bool) (within, custom bool) {
	defer func() {
		if recover() != nil {
			within, custom = true, false
		}
	}()
	if m, ok := v.(map[string]any); ok && fields {
		within = true
		for _, e := range m {
			w, c := in.shape(e, false)
			within, custom = within && w, custom || c
		}
		return within, custom
	}
	c := in.count(reflect.ValueOf(v), 0)
	return c.n <= maxWalkNodes, c.custom
}

func (in *inspector) count(rv reflect.Value, chain int) counted {
	over := counted{n: maxWalkNodes + 1}
	if !rv.IsValid() {
		return counted{n: 1}
	}
	if chain > maxWalkDepth { // pointers alone, in a cycle
		return over
	}
	if nonData(rv.Kind()) {
		return counted{n: 1}
	}
	if rv.Type() == rawMessageType {
		// Its values count as the walk's do: a raw document is capped like any other.
		return counted{n: min(rawNodes(rv.Bytes()), maxWalkNodes+1)}
	}
	if _, custom := customTarget(rv); custom {
		return counted{n: 1, custom: true}
	}
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return counted{n: 1}
		}
		if rv.Kind() == reflect.Interface {
			return in.count(rv.Elem(), chain+1)
		}
		return in.shared(rv, over, func() counted { return in.count(rv.Elem(), chain+1) })
	case reflect.Slice, reflect.Array:
		if rv.Type().Elem().Kind() == reflect.Uint8 || rv.Len() == 0 {
			return counted{n: 1}
		}
		measure := func() counted {
			c := counted{n: 1}
			for n := 0; n < rv.Len() && c.n <= maxWalkNodes; n++ {
				c = add(c, in.count(rv.Index(n), 0))
			}
			return c
		}
		if rv.Kind() == reflect.Slice {
			return in.shared(rv, over, measure)
		}
		return measure()
	case reflect.Map:
		return in.shared(rv, over, func() counted {
			c := counted{n: 1}
			if kt := rv.Type().Key(); kt.Kind() != reflect.String && kt.Implements(textType) {
				c.custom = true
			}
			for it := rv.MapRange(); it.Next() && c.n <= maxWalkNodes; {
				c = add(c, in.count(it.Value(), 0))
			}
			return c
		})
	case reflect.Struct:
		c := counted{n: 1}
		for _, f := range cachedJSONFields(rv.Type()) {
			if fv, ok := jsonFieldValue(rv, f); ok {
				if c = add(c, in.count(fv, 0)); c.n > maxWalkNodes {
					break
				}
			}
		}
		return c
	}
	return counted{n: 1}
}

func add(c, child counted) counted {
	return counted{n: min(c.n+child.n, maxWalkNodes+1), custom: c.custom || child.custom}
}

func (in *inspector) shared(rv reflect.Value, over counted, measure func() counted) counted {
	key, stable := serializerIdentity(rv)
	if !stable {
		return measure()
	}
	if c, ok := in.memo[key]; ok {
		return c
	}
	if in.active[key] {
		return over
	}
	in.active[key] = true
	defer delete(in.active, key)
	c := measure()
	in.memo[key] = c
	return c
}

// nonData is a kind JSON cannot hold: such a value is left out before any serializer its type
// has is called.
func nonData(k reflect.Kind) bool {
	switch k {
	case reflect.Func, reflect.Chan, reflect.UnsafePointer, reflect.Complex64, reflect.Complex128:
		return true
	}
	return false
}

// rawNodes is an upper bound on the values in a JSON document: one for the document, one for each
// separator and one for each container, found without decoding it.
func rawNodes(raw []byte) int {
	n, inString := 1, false
	for i := 0; i < len(raw); i++ {
		switch c := raw[i]; {
		case inString && c == '\\':
			i++
		case c == '"':
			inString = !inString
		case inString:
		case c == ',' || c == '{' || c == '[':
			n++
		}
	}
	return n
}
