package rudderstackai

import (
	"errors"
	"reflect"
	"strconv"
)

var errUnsizable = errors.New("rudderstackai: a value could not be encoded as JSON")

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

func (z *sizer) shared(rv reflect.Value, measure func() (int, error)) (int, error) {
	key, stable := serializerIdentity(rv)
	if !stable {
		return measure()
	}
	if n, ok := z.memo[key]; ok {
		return n, nil
	}
	if z.active[key] {
		return 0, errUnsizable // a cycle
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
	if target, custom := customTarget(rv); custom {
		return z.leaf(target.Interface())
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
	case reflect.Float32, reflect.Float64, reflect.String:
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
	total := 2 + max(rv.Len()-1, 0)
	for it := rv.MapRange(); it.Next(); {
		name, ok := legacyMapKey(it.Key())
		if !ok {
			return 0, errUnsizable
		}
		k, err := z.leaf(name)
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
		case f.quoted && !custom && fv.Kind() != reflect.Interface && fv.Kind() != reflect.Pointer:
			// ,string wraps a scalar's encoding in a JSON string.
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
