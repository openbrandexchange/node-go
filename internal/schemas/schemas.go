// Package schemas embeds the OBX JSON Schemas (copied from the spec
// repository by scripts/sync-spec.sh, CC-BY-4.0) and validates documents.
//
// Two validators exist: Strict uses the schemas as published (closed
// objects; used by tests to check what the node produces) and Lenient drops
// every "additionalProperties": false so that inputs with unknown fields
// validate (nodes MUST ignore unknown fields).
package schemas

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

var printer = message.NewPrinter(language.English)

//go:embed json/*.json
var files embed.FS

// Base is the $id prefix of every schema.
const Base = "https://openbrandexchange.com/schemas/v0.3/"

// FieldError is one validation failure.
type FieldError struct {
	Pointer string
	Message string
}

// Validator validates documents against named schemas ("Product", ...).
type Validator struct {
	mu       sync.Mutex
	compiler *jsonschema.Compiler
	cache    map[string]*jsonschema.Schema
}

var (
	strictOnce, lenientOnce sync.Once
	strictV, lenientV       *Validator
)

// Strict returns the validator using the schemas as published.
func Strict() *Validator {
	strictOnce.Do(func() { strictV = newValidator(false) })
	return strictV
}

// Lenient returns the validator that ignores unknown properties.
func Lenient() *Validator {
	lenientOnce.Do(func() { lenientV = newValidator(true) })
	return lenientV
}

func openClosed(v any) any {
	switch t := v.(type) {
	case map[string]any:
		if ap, ok := t["additionalProperties"]; ok {
			if b, isBool := ap.(bool); isBool && !b {
				delete(t, "additionalProperties")
			}
		}
		for k, c := range t {
			t[k] = openClosed(c)
		}
	case []any:
		for i := range t {
			t[i] = openClosed(t[i])
		}
	}
	return v
}

func newValidator(lenient bool) *Validator {
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	entries, err := fs.ReadDir(files, "json")
	if err != nil {
		panic(err)
	}
	for _, e := range entries {
		raw, err := files.ReadFile("json/" + e.Name())
		if err != nil {
			panic(err)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			panic(fmt.Sprintf("schema %s: %v", e.Name(), err))
		}
		if lenient {
			doc = openClosed(doc)
		}
		if err := c.AddResource(Base+e.Name(), doc); err != nil {
			panic(err)
		}
	}
	return &Validator{compiler: c, cache: map[string]*jsonschema.Schema{}}
}

func (v *Validator) schema(name string) (*jsonschema.Schema, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if s, ok := v.cache[name]; ok {
		return s, nil
	}
	ref := Base + name + ".json"
	if strings.Contains(name, "#") {
		parts := strings.SplitN(name, "#", 2)
		ref = Base + parts[0] + ".json#" + parts[1]
	}
	s, err := v.compiler.Compile(ref)
	if err != nil {
		return nil, err
	}
	v.cache[name] = s
	return s, nil
}

// ValidateJSON validates raw JSON against schema name. It returns nil when
// valid, else the field errors (sorted by pointer).
func (v *Validator) ValidateJSON(name string, raw []byte) []FieldError {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return []FieldError{{Pointer: "", Message: "invalid JSON: " + err.Error()}}
	}
	return v.Validate(name, doc)
}

// ValidateValue marshals value and validates it.
func (v *Validator) ValidateValue(name string, value any) []FieldError {
	raw, err := json.Marshal(value)
	if err != nil {
		return []FieldError{{Message: err.Error()}}
	}
	return v.ValidateJSON(name, raw)
}

// Validate validates an already decoded document (from jsonschema.UnmarshalJSON).
func (v *Validator) Validate(name string, doc any) []FieldError {
	s, err := v.schema(name)
	if err != nil {
		return []FieldError{{Message: "schema: " + err.Error()}}
	}
	err = s.Validate(doc)
	if err == nil {
		return nil
	}
	ve, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return []FieldError{{Message: err.Error()}}
	}
	var out []FieldError
	seen := map[string]bool{}
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			ptr := ""
			if len(e.InstanceLocation) > 0 {
				ptr = "/" + strings.Join(escape(e.InstanceLocation), "/")
			}
			msg := e.ErrorKind.LocalizedString(printer)
			key := ptr + "|" + msg
			if !seen[key] {
				seen[key] = true
				out = append(out, FieldError{Pointer: ptr, Message: msg})
			}
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Pointer < out[j].Pointer })
	return out
}

func escape(parts []string) []string {
	out := make([]string, len(parts))
	for i, p := range parts {
		p = strings.ReplaceAll(p, "~", "~0")
		out[i] = strings.ReplaceAll(p, "/", "~1")
	}
	return out
}
