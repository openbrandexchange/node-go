package node

import (
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Extension members (protocol.md §2, ingest.md §3.1): members named
// x-<vendor>-<name> are stored with any JSON value on the objects whose
// schema allows them and returned unchanged wherever the object is
// returned. They travel with the write models and the representations as
// Extensions (JSON Pointer of the object → its members), collected when
// JSON is decoded and put back when it is encoded, so every code path that
// decodes and re-encodes a stored object keeps them.

// Extensions maps the JSON Pointer of an object to its extension members.
type Extensions map[string]map[string]json.RawMessage

var extensionName = regexp.MustCompile(`^x-[a-z0-9]+-[a-z0-9_-]+$`)

// Where extension members are allowed ("*" is any array index), per stored
// write model and representation.
var (
	productExtPaths = []string{"", "/materials/*", "/materials/*/part", "/materials/*/fibers/*", "/materials/*/fibers/*/material",
		"/materials/*/fibers/*/qualifiers/*", "/care/*", "/certifications/*", "/footwear_materials"}
	variantExtPaths = []string{"", "/color", "/measurements"}
	assetExtPaths   = []string{"", "/rights", "/provenance"}
	brandExtPaths   = []string{""}
)

// collectExtensions returns the extension members of raw at the allowed
// paths (nil when there are none).
func collectExtensions(raw []byte, paths []string) Extensions {
	if !bytes.Contains(raw, []byte(`"x-`)) {
		return nil
	}
	var doc any
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	out := Extensions{}
	for _, p := range paths {
		forEachAt(doc, splitPath(p), "", func(ptr string, obj map[string]any) {
			for k, v := range obj {
				if extensionName.MatchString(k) {
					if out[ptr] == nil {
						out[ptr] = map[string]json.RawMessage{}
					}
					out[ptr][k] = mustJSON(v)
				}
			}
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func splitPath(p string) []string {
	if p == "" {
		return nil
	}
	return strings.Split(strings.TrimPrefix(p, "/"), "/")
}

// forEachAt calls fn for every object at the path pattern segs below v.
func forEachAt(v any, segs []string, ptr string, fn func(string, map[string]any)) {
	if len(segs) == 0 {
		if m, ok := v.(map[string]any); ok {
			fn(ptr, m)
		}
		return
	}
	switch t := v.(type) {
	case map[string]any:
		if segs[0] != "*" {
			if c, ok := t[segs[0]]; ok {
				forEachAt(c, segs[1:], ptr+"/"+segs[0], fn)
			}
		}
	case []any:
		if segs[0] == "*" {
			for i, c := range t {
				forEachAt(c, segs[1:], ptr+"/"+strconv.Itoa(i), fn)
			}
		} else if i, err := strconv.Atoi(segs[0]); err == nil && i >= 0 && i < len(t) {
			forEachAt(t[i], segs[1:], ptr+"/"+segs[0], fn)
		}
	}
}

// injectExtensions puts ext into the JSON object raw. An object that is
// null or missing at the end of a pointer is created; a pointer into an
// array element that does not exist is skipped. Without extensions raw is
// returned as it is.
func injectExtensions(raw []byte, ext Extensions) []byte {
	if len(ext) == 0 {
		return raw
	}
	var doc any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&doc) != nil {
		return raw
	}
	ptrs := make([]string, 0, len(ext))
	for p := range ext {
		ptrs = append(ptrs, p)
	}
	sort.Strings(ptrs)
	for _, p := range ptrs {
		obj := objectAt(&doc, splitPath(p))
		if obj == nil {
			continue
		}
		for k, v := range ext[p] {
			obj[k] = v
		}
	}
	return mustJSON(doc)
}

// objectAt returns the object at segs below *v, creating objects for
// missing or null members (not array elements).
func objectAt(v *any, segs []string) map[string]any {
	if *v == nil {
		*v = map[string]any{}
	}
	if len(segs) == 0 {
		m, _ := (*v).(map[string]any)
		return m
	}
	switch t := (*v).(type) {
	case map[string]any:
		c := t[segs[0]]
		m := objectAt(&c, segs[1:])
		if m != nil {
			t[segs[0]] = c
		}
		return m
	case []any:
		i, err := strconv.Atoi(segs[0])
		if err != nil || i < 0 || i >= len(t) || t[i] == nil {
			return nil
		}
		return objectAt(&t[i], segs[1:])
	}
	return nil
}

// carryExtensions copies the extension members of a stored object into doc
// where doc has the object and does not set the member itself (imports
// leave extensions untouched, import-format.md §4 rule 2).
func carryExtensions(doc map[string]any, stored Extensions) {
	for p, members := range stored {
		var root any = doc
		var target map[string]any
		forEachAt(root, splitPath(p), "", func(_ string, obj map[string]any) { target = obj })
		if seg := strings.TrimPrefix(p, "/"); target == nil && (seg == "rights" || seg == "provenance" || seg == "measurements") && doc[seg] == nil {
			target = map[string]any{}
			doc[seg] = target
		}
		if target == nil {
			continue
		}
		for k, v := range members {
			if _, set := target[k]; !set {
				target[k] = v
			}
		}
	}
}

// stripExtensionMembers removes x- members from the rights/provenance
// maps, which keep their extensions in Extensions instead.
func stripExtensionMembers(m map[string]json.RawMessage) map[string]json.RawMessage {
	for k := range m {
		if strings.HasPrefix(k, "x-") {
			delete(m, k)
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// ---- JSON methods of the types that carry extensions ---------------------------

func (d ProductData) MarshalJSON() ([]byte, error) {
	type plain ProductData
	b, err := json.Marshal(plain(d))
	return injectExtensions(b, d.Ext), err
}

func (d *ProductData) UnmarshalJSON(b []byte) error {
	type plain ProductData
	if err := json.Unmarshal(b, (*plain)(d)); err != nil {
		return err
	}
	d.Ext = collectExtensions(b, productExtPaths)
	return nil
}

func (d VariantData) MarshalJSON() ([]byte, error) {
	type plain VariantData
	b, err := json.Marshal(plain(d))
	return injectExtensions(b, d.Ext), err
}

func (d *VariantData) UnmarshalJSON(b []byte) error {
	type plain VariantData
	if err := json.Unmarshal(b, (*plain)(d)); err != nil {
		return err
	}
	d.Ext = collectExtensions(b, variantExtPaths)
	return nil
}

func (d AssetData) MarshalJSON() ([]byte, error) {
	type plain AssetData
	b, err := json.Marshal(plain(d))
	return injectExtensions(b, d.Ext), err
}

func (d *AssetData) UnmarshalJSON(b []byte) error {
	type plain AssetData
	if err := json.Unmarshal(b, (*plain)(d)); err != nil {
		return err
	}
	d.Ext = collectExtensions(b, assetExtPaths)
	d.Rights, d.Provenance = stripExtensionMembers(d.Rights), stripExtensionMembers(d.Provenance)
	return nil
}

func (d BrandData) MarshalJSON() ([]byte, error) {
	type plain BrandData
	b, err := json.Marshal(plain(d))
	return injectExtensions(b, d.Ext), err
}

func (d *BrandData) UnmarshalJSON(b []byte) error {
	type plain BrandData
	if err := json.Unmarshal(b, (*plain)(d)); err != nil {
		return err
	}
	d.Ext = collectExtensions(b, brandExtPaths)
	return nil
}

func (p Product) MarshalJSON() ([]byte, error) {
	type plain Product
	b, err := json.Marshal(plain(p))
	return injectExtensions(b, p.Ext), err
}

func (p *Product) UnmarshalJSON(b []byte) error {
	type plain Product
	if err := json.Unmarshal(b, (*plain)(p)); err != nil {
		return err
	}
	p.Ext = collectExtensions(b, productExtPaths)
	return nil
}

func (v Variant) MarshalJSON() ([]byte, error) {
	type plain Variant
	b, err := json.Marshal(plain(v))
	return injectExtensions(b, v.Ext), err
}

func (v *Variant) UnmarshalJSON(b []byte) error {
	type plain Variant
	if err := json.Unmarshal(b, (*plain)(v)); err != nil {
		return err
	}
	v.Ext = collectExtensions(b, variantExtPaths)
	return nil
}

func (a Asset) MarshalJSON() ([]byte, error) {
	type plain Asset
	b, err := json.Marshal(plain(a))
	return injectExtensions(b, a.Ext), err
}

func (a *Asset) UnmarshalJSON(b []byte) error {
	type plain Asset
	if err := json.Unmarshal(b, (*plain)(a)); err != nil {
		return err
	}
	a.Ext = collectExtensions(b, assetExtPaths)
	return nil
}

func (br Brand) MarshalJSON() ([]byte, error) {
	type plain Brand
	b, err := json.Marshal(plain(br))
	return injectExtensions(b, br.Ext), err
}

func (br *Brand) UnmarshalJSON(b []byte) error {
	type plain Brand
	if err := json.Unmarshal(b, (*plain)(br)); err != nil {
		return err
	}
	br.Ext = collectExtensions(b, brandExtPaths)
	return nil
}
