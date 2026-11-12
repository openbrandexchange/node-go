package node

import (
	"fmt"
	"math"
	"strings"

	"github.com/openbrandexchange/node-go/internal/importfmt"
)

// Article data of spec 0.2 (protocol.md §5.9). Protocol objects always carry
// the new product and variant properties (null when not stated); inside
// measurements and features only stated members are present, and the fibre
// flags recycled and organic only when true.

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
	// column is the products.csv column an import names.
	column string
}

// normalizeMaterials applies the 0.2 fibre rule to stored materials:
// recycled_<fibre> and organic_<fibre> become the fibre with its flag.
func normalizeMaterials(ms []MaterialPart) {
	for i := range ms {
		for j := range ms[i].Fibers {
			f := &ms[i].Fibers[j]
			key, rec, org := importfmt.NormalizeFiber(f.Fiber)
			f.Fiber, f.Recycled, f.Organic = key, f.Recycled || rec, f.Organic || org
		}
	}
}

// prepareArticleInput applies ingest.md §3.4 to a write document before it
// is validated: an empty measurements or features object (or members that
// are null, "not stated") and an empty certifications list are null.
func prepareArticleInput(kind string, doc map[string]any) {
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
		emptyToNull("features")
		if l, ok := doc["certifications"].([]any); ok && len(l) == 0 {
			doc["certifications"] = nil
		}
	case KindVariant:
		emptyToNull("measurements")
	}
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
					if animalFibres[f.Fiber] || strings.HasSuffix(f.Fiber, "_wool") {
						add(warnAnimalOriginUnknown, "/contains_animal_origin", "contains_animal_origin",
							fmt.Sprintf("the materials contain %s; state whether the product contains non-textile parts of animal origin (Regulation (EU) No 1007/2011 Art. 12)", f.Fiber))
						break animal
					}
				}
			}
		}
		for i, p := range d.Materials {
			sum := 0.0
			for _, f := range p.Fibers {
				sum += f.Percentage
			}
			if math.Abs(sum-100) > 0.01 {
				add(warnFibrePercentagesNot100, fmt.Sprintf("/materials/%d", i), "materials",
					fmt.Sprintf("the percentages of %s add up to %g, not 100 (Regulation (EU) No 1007/2011 Art. 9)", p.Part, sum))
			}
		}
	case footwearItemTypes[it]:
		if !d.FootwearMaterials.complete() {
			add(warnFootwearMaterialsMissing, "/footwear_materials", "footwear_materials",
				"the footwear materials of "+strings.Join(d.FootwearMaterials.missing(), ", ")+" are missing; they must be on the label of footwear (Directive 94/11/EC)")
		}
	}
	return out
}
