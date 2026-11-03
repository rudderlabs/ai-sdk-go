package rudderstackai

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Keep the traversal here so the identical catalog can be checked in other SDKs.
// Shared properties describe the union across events; event-specific restrictions
// remain in the schemas because catalog property refs cannot override enums.
func TestCatalogMatchesSchemas(t *testing.T) {
	type property struct {
		ID         string         `yaml:"id"`
		Name       string         `yaml:"name"`
		Type       string         `yaml:"type"`
		Types      []string       `yaml:"types"`
		ItemType   string         `yaml:"item_type"`
		ItemTypes  []string       `yaml:"item_types"`
		Config     map[string]any `yaml:"config"`
		Properties []catalogRef   `yaml:"properties"`
	}
	props, custom := map[string]property{}, map[string]property{}
	events, rules := map[string]string{}, map[string][]catalogRef{}
	files, err := filepath.Glob("spec/rudderstack-catalog/*.yaml")
	if err != nil || len(files) != 4 {
		t.Fatalf("catalog YAML files: %v, %v", files, err)
	}
	for _, path := range files {
		var doc struct {
			Spec struct {
				Properties []property
				Types      []property
				Events     []property
				Rules      []struct {
					Event      string
					Properties []catalogRef
				}
			}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := yaml.Unmarshal(data, &doc); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, p := range doc.Spec.Properties {
			if _, ok := props[p.ID]; ok {
				t.Fatalf("duplicate property ID %s", p.ID)
			}
			for _, existing := range props {
				if existing.Name == p.Name {
					t.Fatalf("duplicate wire name %s", p.Name)
				}
			}
			props[p.ID] = p
		}
		for _, p := range doc.Spec.Types {
			custom[p.ID] = p
		}
		for _, e := range doc.Spec.Events {
			events[e.ID] = e.Name
		}
		for _, r := range doc.Spec.Rules {
			id := strings.TrimPrefix(r.Event, "#event:")
			if _, ok := rules[id]; ok {
				t.Fatalf("duplicate event rule %s", id)
			}
			rules[id] = r.Properties
		}
	}
	expandType := func(typ string) property {
		if strings.HasPrefix(typ, "#custom-type:") {
			p, ok := custom[strings.TrimPrefix(typ, "#custom-type:")]
			if !ok {
				t.Fatalf("unresolved custom type %s", typ)
			}
			return p
		}
		return property{Type: typ}
	}
	var checkRefs func([]catalogRef)
	checkRefs = func(refs []catalogRef) {
		seen := map[string]bool{}
		for _, ref := range refs {
			p, ok := props[strings.TrimPrefix(ref.Property, "#property:")]
			if !ok || seen[p.Name] {
				t.Fatalf("unresolved or duplicate property ref %s", ref.Property)
			}
			seen[p.Name] = true
			checkRefs(ref.Properties)
		}
	}
	for id, refs := range rules {
		if events[id] == "" {
			t.Fatalf("unresolved event %s", id)
		}
		checkRefs(refs)
	}
	for _, p := range custom {
		checkRefs(p.Properties)
	}

	want, wantItems := map[string]catalogShape{}, map[string]catalogShape{}
	for _, path := range []string{"spec/ai-events-v0.4.schema.json", "spec/ai-client-events-v0.2.schema.json"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		var resolve func(map[string]any) map[string]any
		resolve = func(s map[string]any) map[string]any {
			out := map[string]any{}
			merge := func(part map[string]any) {
				for k, v := range part {
					if k == "properties" {
						if out[k] == nil {
							out[k] = map[string]any{}
						}
						for name, p := range v.(map[string]any) {
							out[k].(map[string]any)[name] = p
						}
					} else {
						out[k] = v
					}
				}
			}
			if ref, ok := s["$ref"].(string); ok {
				if !strings.HasPrefix(ref, "#/") {
					t.Fatalf("unsupported schema ref %s", ref)
				}
				target := doc
				for _, key := range strings.Split(ref[2:], "/") {
					key = strings.ReplaceAll(strings.ReplaceAll(key, "~1", "/"), "~0", "~")
					target = target[key].(map[string]any)
				}
				merge(resolve(target))
			}
			if parts, ok := s["allOf"].([]any); ok {
				for _, part := range parts {
					merge(resolve(part.(map[string]any)))
				}
			}
			merge(s)
			return out
		}
		var walk func(map[string]any, []catalogRef, string)
		walk = func(s map[string]any, refs []catalogRef, loc string) {
			s = resolve(s)
			attached := map[string]catalogRef{}
			for _, ref := range refs {
				attached[props[strings.TrimPrefix(ref.Property, "#property:")].Name] = ref
			}
			fields, _ := s["properties"].(map[string]any)
			for name, raw := range fields {
				ref, ok := attached[name]
				if !ok {
					t.Errorf("%s.%s missing catalog attachment", loc, name)
					continue
				}
				field := resolve(raw.(map[string]any))
				shape := want[name]
				shape.add(field)
				want[name] = shape
				p := props[strings.TrimPrefix(ref.Property, "#property:")]
				nested := expandType(p.Type).Properties
				if len(ref.Properties) > 0 {
					nested = ref.Properties
				}
				if item, ok := field["items"].(map[string]any); ok {
					item = resolve(item)
					shape := wantItems[name]
					shape.add(item)
					wantItems[name] = shape
					if len(nested) == 0 {
						nested = expandType(p.ItemType).Properties
					}
					walk(item, nested, loc+"."+name+"[]")
				} else {
					walk(field, nested, loc+"."+name)
				}
			}
		}
		for name, raw := range doc["$defs"].(map[string]any) {
			if (!strings.HasPrefix(name, "ai_") && !strings.HasPrefix(name, "mcp_") && name != "declared_tool_event") || name == "mcp_fields" || name == "mcp_error_type" {
				continue
			}
			refs, ok := rules[name]
			if !ok {
				t.Errorf("%s: missing event rule %s", path, name)
				continue
			}
			walk(raw.(map[string]any), refs, name)
		}
	}
	compare := func(loc string, expected catalogShape, p property) {
		base := expandType(p.Type)
		if p.Type != "" {
			p.Type, p.Properties = base.Type, base.Properties
			if len(base.Config) > 0 {
				p.Config = base.Config
			}
		}
		var actual catalogShape
		s := map[string]any{"type": p.Type}
		if len(p.Types) > 0 {
			s["type"] = p.Types
		}
		if enum, ok := p.Config["enum"]; ok {
			s["enum"] = enum
		}
		actual.add(s)
		if !reflect.DeepEqual(expected, actual) {
			t.Errorf("%s type/enum mismatch: want %+v, got %+v", loc, expected, actual)
		}
	}
	for _, p := range props {
		if shape, ok := want[p.Name]; ok {
			compare(p.Name, shape, p)
		}
		if shape, ok := wantItems[p.Name]; ok {
			compare(p.Name+"[]", shape, property{Type: p.ItemType, Types: p.ItemTypes})
		}
	}
}

type catalogRef struct {
	Property   string       `yaml:"property"`
	Properties []catalogRef `yaml:"properties"`
}

type catalogShape struct {
	Types []string
	Enum  []string // nil means unrestricted, including an unconstrained schema.
	Seen  bool
}

func (shape *catalogShape) add(s map[string]any) {
	var types, enum []string
	if values, ok := s["enum"].([]any); ok {
		for _, value := range values {
			b, _ := json.Marshal(value)
			enum = append(enum, string(b))
		}
	} else if value, ok := s["const"]; ok {
		b, _ := json.Marshal(value)
		enum = []string{string(b)}
	}
	switch typ := s["type"].(type) {
	case string:
		types = []string{typ}
	case []string:
		types = typ
	case []any:
		for _, t := range typ {
			types = append(types, t.(string))
		}
	default:
		if enum == nil {
			types = []string{"string", "number", "integer", "boolean", "null", "array", "object"}
		} else {
			for _, v := range enum {
				var value any
				_ = json.Unmarshal([]byte(v), &value)
				switch value.(type) {
				case string:
					types = append(types, "string")
				case bool:
					types = append(types, "boolean")
				case nil:
					types = append(types, "null")
				default:
					types = append(types, "number")
				}
			}
		}
	}
	shape.Types = append(shape.Types, types...)
	slices.Sort(shape.Types)
	shape.Types = slices.Compact(shape.Types)
	if !shape.Seen {
		shape.Enum = enum
	} else if enum == nil || shape.Enum == nil {
		shape.Enum = nil
	} else {
		shape.Enum = append(shape.Enum, enum...)
	}
	slices.Sort(shape.Enum)
	shape.Enum = slices.Compact(shape.Enum)
	shape.Seen = true
}
