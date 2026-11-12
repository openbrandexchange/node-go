// Package vocab embeds the vocabularies of the OBX specification
// (vocabularies/*.json, copied by scripts/sync-spec.sh, CC-BY-4.0): the
// part, material, qualifier and care codes with their names per language
// and, for care, the ISO 3758 category and the GINETEX symbol code
// (protocol.md §5.9.1). A node returns these values for known codes.
package vocab

import (
	"embed"
	"encoding/json"
	"fmt"
)

//go:embed json/*.json
var files embed.FS

// Entry is one vocabulary entry. Name is nil when the code has no name
// (the part main); Category and Symbol are only used by care.
type Entry struct {
	Code     string            `json:"code"`
	Name     map[string]string `json:"name"`
	Category *string           `json:"category"`
	Symbol   *string           `json:"symbol"`
}

// Vocabulary is one vocabulary file, in file order.
type Vocabulary struct {
	Entries []Entry
	byCode  map[string]*Entry
}

// Lookup returns the entry of code.
func (v *Vocabulary) Lookup(code string) (*Entry, bool) {
	e, ok := v.byCode[code]
	return e, ok
}

// Has reports whether code is in the vocabulary.
func (v *Vocabulary) Has(code string) bool {
	_, ok := v.byCode[code]
	return ok
}

// The four vocabularies of the specification.
var (
	Parts      = load("parts")
	Materials  = load("materials")
	Qualifiers = load("qualifiers")
	Care       = load("care")
)

func load(name string) *Vocabulary {
	b, err := files.ReadFile("json/" + name + ".json")
	if err != nil {
		panic(fmt.Sprintf("vocab: %s: %v", name, err))
	}
	var doc struct {
		Entries []Entry `json:"entries"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		panic(fmt.Sprintf("vocab: %s: %v", name, err))
	}
	v := &Vocabulary{Entries: doc.Entries, byCode: make(map[string]*Entry, len(doc.Entries))}
	for i := range v.Entries {
		e := &v.Entries[i]
		for lang, s := range e.Name {
			if s == "" {
				delete(e.Name, lang)
			}
		}
		if len(e.Name) == 0 {
			e.Name = nil
		}
		v.byCode[e.Code] = e
	}
	return v
}
