package node

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/openbrandexchange/node-go/internal/importfmt"
	"github.com/openbrandexchange/node-go/internal/vocab"
)

// Article data of spec 0.2 and 0.3 (protocol.md §5.9). Protocol objects
// always carry the product and variant properties (null when not stated);
// inside measurements and features only stated members are present. The
// parts, materials and qualifiers of materials and the care entries are
// terms with their names from the vocabularies (0.3, protocol.md §5.9.1).

// FootwearMaterials is the material of the three parts of a shoe
// (Directive 94/11/EC); responses always include all three parts.
type FootwearMaterials struct {
	Upper         *string `json:"upper"`
	LiningAndSock *string `json:"lining_and_sock"`
	OuterSole     *string `json:"outer_sole"`
}

func (f *FootwearMaterials) complete() bool {
	return f != nil && f.Upper != nil && f.LiningAndSock != nil && f.OuterSole != nil
}

func (f *FootwearMaterials) missing() []string {
	var out []string
	if f == nil || f.Upper == nil {
		out = append(out, "upper")
	}
	if f == nil || f.LiningAndSock == nil {
		out = append(out, "lining_and_sock")
	}
	if f == nil || f.OuterSole == nil {
		out = append(out, "outer_sole")
	}
	return out
}

// Certification is one voluntary certification the brand states.
type Certification struct {
	Scheme        string  `json:"scheme"`
	LicenceNumber *string `json:"licence_number"`
	Scope         string  `json:"scope"`
}

// Range is a shortest and longest setting (strap_length_cm).
type Range struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

// Measurements of one variant; nil members are not stated and left out.
type Measurements struct {
	HeightCM        *float64 `json:"height_cm,omitempty"`
	WidthCM         *float64 `json:"width_cm,omitempty"`
	DepthCM         *float64 `json:"depth_cm,omitempty"`
	WeightKG        *float64 `json:"weight_kg,omitempty"`
	VolumeL         *float64 `json:"volume_l,omitempty"`
	VolumeExpandedL *float64 `json:"volume_expanded_l,omitempty"`
	HandleDropCM    *float64 `json:"handle_drop_cm,omitempty"`
	StrapLengthCM   *Range   `json:"strap_length_cm,omitempty"`
	LaptopMaxInch   *float64 `json:"laptop_max_inch,omitempty"`
	HeelHeightCM    *float64 `json:"heel_height_cm,omitempty"`
	ShaftHeightCM   *float64 `json:"shaft_height_cm,omitempty"`
	InseamCM        *float64 `json:"inseam_cm,omitempty"`
}

func (m *Measurements) empty() bool {
	return m == nil || *m == Measurements{}
}

// Cell returns one measurement as a products.csv cell ("" when not stated).
func (m *Measurements) Cell(key string) string {
	if m == nil {
		return ""
	}
	num := map[string]*float64{"height_cm": m.HeightCM, "width_cm": m.WidthCM, "depth_cm": m.DepthCM, "weight_kg": m.WeightKG,
		"volume_l": m.VolumeL, "volume_expanded_l": m.VolumeExpandedL, "handle_drop_cm": m.HandleDropCM,
		"laptop_max_inch": m.LaptopMaxInch, "heel_height_cm": m.HeelHeightCM, "shaft_height_cm": m.ShaftHeightCM, "inseam_cm": m.InseamCM}
	if key == "strap_length_cm" {
		if r := m.StrapLengthCM; r != nil {
			if r.Min == r.Max {
				return importfmt.FormatNumber(r.Min)
			}
			return importfmt.FormatNumber(r.Min) + "-" + importfmt.FormatNumber(r.Max)
		}
		return ""
	}
	if v := num[key]; v != nil {
		return importfmt.FormatNumber(*v)
	}
	return ""
}

// Features are the typed product features (registry keys and other keys,
// values as decoded from JSON); nil marshals as null.
type Features map[string]any

// WarningDetail is a warning with a stable code (IngestResult.warning_details).
type WarningDetail struct {
	Code    string  `json:"code"`
	Pointer *string `json:"pointer"`
	Detail  string  `json:"detail"`
	// column is the products.csv column an import names; value its value.
	column, value string
}

// normalizeTerms gives stored materials and care their canonical 0.3 form
// (protocol.md §5.9.1, ingest.md §3.4): a 0.1 key recycled_<fibre> or
// organic_<fibre> becomes the material with that qualifier (first in
// qualifiers, before the ones that were sent; repeats are dropped); a known code gets the vocabulary's name
// (and for care its category and symbol), whatever was sent; an unknown code
// keeps what was sent, or null. A language without a name is left out.
//
// ext are the product's extension members: those of qualifier terms move
// with their term when the list is reordered or deduplicated (a sent term
// that repeats a 0.1 key's qualifier lends it its members).
func normalizeTerms(ms []MaterialPart, care []CareItem, ext Extensions) {
	for i := range ms {
		fillTerm(&ms[i].Part, vocab.Parts)
		for j := range ms[i].Fibers {
			f := &ms[i].Fibers[j]
			code, qs := importfmt.NormalizeMaterial(f.Material.Code)
			if code != f.Material.Code {
				f.Material = Term{Code: code}
			}
			fillTerm(&f.Material, vocab.Materials)
			// The 0.1 key's qualifiers come first; then the sent ones in
			// order, each code once (the first occurrence wins).
			list := make([]Term, 0, len(qs)+len(f.Qualifiers))
			src := make([]int, 0, cap(list)) // index in the sent list, -1 for a 0.1 key's
			at := map[string]int{}
			for _, q := range qs {
				at[q] = len(list)
				list = append(list, Term{Code: q})
				src = append(src, -1)
			}
			for j, q := range f.Qualifiers {
				if k, dup := at[q.Code]; dup {
					if src[k] == -1 {
						src[k] = j
					}
					continue
				}
				at[q.Code] = len(list)
				list = append(list, q)
				src = append(src, j)
			}
			for k := range list {
				fillTerm(&list[k], vocab.Qualifiers)
			}
			f.Qualifiers = list
			moveQualifierExtensions(ext, fmt.Sprintf("/materials/%d/fibers/%d/qualifiers/", i, j), src)
		}
	}
	for i := range care {
		c := &care[i]
		if e, ok := vocab.Care.Lookup(c.Code); ok {
			c.Category, c.Symbol, c.Name = cloneString(e.Category), cloneString(e.Symbol), vocabName(e.Name)
		} else {
			c.Name = cleanText(c.Name)
		}
		c.Text = cleanText(c.Text)
	}
}

// moveQualifierExtensions renumbers the extension members of the qualifier
// terms below prefix: the new term k gets those of sent term src[k]; the
// members of a dropped repeat are dropped.
func moveQualifierExtensions(ext Extensions, prefix string, src []int) {
	old := map[int]map[string]json.RawMessage{}
	for p, m := range ext {
		if rest, ok := strings.CutPrefix(p, prefix); ok {
			if n, err := strconv.Atoi(rest); err == nil {
				old[n] = m
				delete(ext, p)
			}
		}
	}
	for k, j := range src {
		if m, ok := old[j]; ok && j >= 0 {
			ext[prefix+strconv.Itoa(k)] = m
		}
	}
}

func fillTerm(t *Term, v *vocab.Vocabulary) {
	if e, ok := v.Lookup(t.Code); ok {
		t.Name = vocabName(e.Name)
		return
	}
	t.Name = cleanText(t.Name)
}

// vocabName copies a vocabulary name (nil stays nil).
func vocabName(m map[string]string) LocalizedText {
	if len(m) == 0 {
		return nil
	}
	out := make(LocalizedText, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// cleanText leaves out languages without text; nothing left is nil.
func cleanText(t LocalizedText) LocalizedText {
	for k, v := range t {
		if strings.TrimSpace(v) == "" {
			delete(t, k)
		}
	}
	if len(t) == 0 {
		return nil
	}
	return t
}

func cloneString(s *string) *string {
	if s == nil {
		return nil
	}
	v := *s
	return &v
}

// prepareArticleInput applies ingest.md §3.4 to a write document before it
// is validated: an empty measurements or features object (or members that
// are null, "not stated") and an empty certifications list are null.
func prepareArticleInput(kind string, doc map[string]any) (deprecated string) {
	emptyToNull := func(key string) {
		m, ok := doc[key].(map[string]any)
		if !ok {
			return
		}
		for k, v := range m {
			if v == nil {
				delete(m, k)
			}
		}
		if len(m) == 0 {
			doc[key] = nil
		}
	}
	switch kind {
	case KindProduct:
		deprecated = draftQualifier(doc["materials"])
		emptyToNull("features")
		if l, ok := doc["certifications"].([]any); ok && len(l) == 0 {
			doc["certifications"] = nil
		}
	case KindVariant:
		emptyToNull("measurements")
	}
	return deprecated
}

// draftQualifier reads the single "qualifier" of a fibre, as drafts of 0.3
// wrote it (ingest.md §3.1): without "qualifiers" it becomes qualifiers
// [that term] (null: []); with both, "qualifiers" wins. It returns the
// pointer of the first such fibre for the warning qualifier_deprecated
// (once per write), or "".
func draftQualifier(materials any) string {
	first := ""
	parts, _ := materials.([]any)
	for i, p := range parts {
		pm, _ := p.(map[string]any)
		fibers, _ := pm["fibers"].([]any)
		for j, f := range fibers {
			fm, ok := f.(map[string]any)
			if !ok {
				continue
			}
			q, has := fm["qualifier"]
			if !has {
				continue
			}
			if first == "" {
				first = fmt.Sprintf("/materials/%d/fibers/%d/qualifier", i, j)
			}
			delete(fm, "qualifier")
			if _, ok := fm["qualifiers"]; ok {
				continue
			}
			if q != nil {
				fm["qualifiers"] = []any{q}
			} else {
				fm["qualifiers"] = []any{}
			}
		}
	}
	return first
}

// Item types that get label completeness warnings (import-format.md rule 10).
var (
	textileItemTypes  = map[string]bool{}
	footwearItemTypes = map[string]bool{"shoes": true, "sneakers": true, "boots": true, "sandals": true}
	animalFibres      = map[string]bool{"leather": true, "suede": true, "fur": true, "down": true, "feather": true, "wool": true,
		"merino_wool": true, "lambswool": true, "cashmere": true, "alpaca": true, "mohair": true, "silk": true}
)

func init() {
	for _, k := range strings.Fields(`t_shirt top shirt blouse polo sweater cardigan hoodie sweatshirt vest jacket coat blazer suit
dress skirt trousers jeans shorts jumpsuit leggings underwear lingerie nightwear swimwear socks hat cap scarf gloves tie
bag backpack luggage`) {
		textileItemTypes[k] = true
	}
}

// Label completeness warning codes (Common.json WarningCode).
const (
	warnFibreCompositionMissing  = "fibre_composition_missing"
	warnAnimalOriginUnknown      = "animal_origin_unknown"
	warnFootwearMaterialsMissing = "footwear_materials_missing"
	warnFibrePercentagesNot100   = "fibre_percentages_not_100"
	warnMaterialUnknown          = "material_unknown"
	warnQualifierDeprecated      = "qualifier_deprecated"
)

// labelWarnings checks a product as stored and returns what a consumer label
// would miss (import-format.md rule 10, ingest.md §3.4), in the order
// fibre_composition_missing, animal_origin_unknown,
// fibre_percentages_not_100 (once per part) for textile item types, and
// footwear_materials_missing for footwear item types. Other and unknown item
// types get none.
func labelWarnings(d *ProductData) []WarningDetail {
	it := deref(d.ItemType)
	var out []WarningDetail
	add := func(code, pointer, column, detail string) {
		p := pointer
		out = append(out, WarningDetail{Code: code, Pointer: &p, Detail: detail, column: column})
	}
	switch {
	case textileItemTypes[it]:
		if len(d.Materials) == 0 {
			add(warnFibreCompositionMissing, "/materials", "materials",
				"the fibre composition is missing; it must be on the label of a textile product and shown before purchase (Regulation (EU) No 1007/2011)")
		}
		if d.ContainsAnimalOrigin == nil {
		animal:
			for _, p := range d.Materials {
				for _, f := range p.Fibers {
					if animalFibres[f.Material.Code] || strings.HasSuffix(f.Material.Code, "_wool") {
						add(warnAnimalOriginUnknown, "/contains_animal_origin", "contains_animal_origin",
							fmt.Sprintf("the materials contain %s; state whether the product contains non-textile parts of animal origin (Regulation (EU) No 1007/2011 Art. 12)", f.Material.Code))
						break animal
					}
				}
			}
		}
		// Every fibre states a percentage and they do not sum to 100, or
		// the stated ones exceed 100 (import-format.md rule 10).
		for i, p := range d.Materials {
			sum, all := 0.0, true
			for _, f := range p.Fibers {
				if f.Percentage == nil {
					all = false
					continue
				}
				sum += *f.Percentage
			}
			if (all && math.Abs(sum-100) > 0.01) || sum > 100.01 {
				add(warnFibrePercentagesNot100, fmt.Sprintf("/materials/%d", i), "materials",
					fmt.Sprintf("the percentages of %s add up to %g, not 100 (Regulation (EU) No 1007/2011 Art. 9)", p.Part.Code, sum))
			}
		}
	case footwearItemTypes[it]:
		if !d.FootwearMaterials.complete() {
			add(warnFootwearMaterialsMissing, "/footwear_materials", "footwear_materials",
				"the footwear materials of "+strings.Join(d.FootwearMaterials.missing(), ", ")+" are missing; they must be on the label of footwear (Directive 94/11/EC)")
		}
	}
	// material_unknown for every item type, after the label warnings: once
	// per code outside vocabularies/materials.json, at its first fibre.
	seen := map[string]bool{}
	for i, p := range d.Materials {
		for j, f := range p.Fibers {
			c := f.Material.Code
			if _, known := vocab.Materials.Lookup(c); known || seen[c] {
				continue
			}
			seen[c] = true
			ptr := fmt.Sprintf("/materials/%d/fibers/%d/material/code", i, j)
			out = append(out, WarningDetail{Code: warnMaterialUnknown, Pointer: &ptr, column: "materials", value: c,
				Detail: fmt.Sprintf("material %q is not in the vocabulary (vocabularies/materials.json); it is kept as written, without a name", c)})
		}
	}
	return out
}
