package importfmt

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The article data of spec 0.2 and 0.3 (protocol.md §5.9, import-format.md §1.6).

// FootwearParts are the three parts of a shoe (Directive 94/11/EC), in
// label order.
var FootwearParts = []string{"upper", "lining_and_sock", "outer_sole"}

// FootwearValues are the directive's four material categories.
var FootwearValues = []string{"leather", "coated_leather", "textile", "other"}

// ColorFamilies are the colour families of protocol.md §5.9.5.
var ColorFamilies = []string{"black", "white", "grey", "beige", "brown", "red", "pink", "orange", "yellow", "green", "blue", "purple", "gold", "silver", "multicolour", "transparent"}

// MeasurementColumns are the measurement keys, in the order of the schema
// and of the products.csv columns.
var MeasurementColumns = []string{"height_cm", "width_cm", "depth_cm", "weight_kg", "volume_l", "volume_expanded_l", "handle_drop_cm", "strap_length_cm", "laptop_max_inch", "heel_height_cm", "shaft_height_cm", "inseam_cm"}

// FeatureType is the type of a registry feature.
type FeatureType string

// Feature types of the registry.
const (
	FeatureEnum    FeatureType = "enum"
	FeatureInteger FeatureType = "integer"
	FeatureBoolean FeatureType = "boolean"
	FeatureList    FeatureType = "list"
)

// FeatureSpec is one registry key: its type and allowed values (integers as text).
type FeatureSpec struct {
	Type   FeatureType
	Values []string
}

// FeatureKeys are the registry keys in registry order (protocol.md §5.9.4).
var FeatureKeys = []string{"shell", "wheels", "lock", "expandable", "closure", "laptop_compartment", "water_resistance", "detachable_parts", "adjustable_parts", "fit", "sleeve_length", "neckline"}

// FeatureRegistry is the typed feature registry.
var FeatureRegistry = map[string]FeatureSpec{
	"shell":              {FeatureEnum, []string{"hard", "soft", "hybrid"}},
	"wheels":             {FeatureInteger, []string{"0", "2", "4", "8"}},
	"lock":               {FeatureEnum, []string{"none", "combination", "tsa_combination", "tsa_key", "key"}},
	"expandable":         {FeatureBoolean, nil},
	"closure":            {FeatureEnum, []string{"zip", "magnet", "flap", "buckle", "drawstring", "button", "hook_and_loop", "laces", "slip_on", "open"}},
	"laptop_compartment": {FeatureBoolean, nil},
	"water_resistance":   {FeatureEnum, []string{"none", "water_repellent", "waterproof"}},
	"detachable_parts":   {FeatureList, []string{"shoulder_strap", "handle", "lining", "insole", "hood", "belt", "pouch", "wheels"}},
	"adjustable_parts":   {FeatureList, []string{"shoulder_strap", "waist", "hem", "cuff"}},
	"fit":                {FeatureEnum, []string{"slim", "regular", "relaxed", "oversized"}},
	"sleeve_length":      {FeatureEnum, []string{"sleeveless", "short", "three_quarter", "long"}},
	"neckline":           {FeatureEnum, []string{"crew", "v_neck", "collar", "hood", "turtleneck", "boat", "scoop"}},
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// NormalizeMaterial turns a 0.1 fibre key recycled_<fibre> or
// organic_<fibre> into the material and its qualifier (protocol.md §5.9.1):
// recycled_polyester → polyester, recycled. A fibre has one qualifier: when
// the prefixes are combined, the outermost one is the qualifier. qualifier is
// "" for a key without a prefix.
func NormalizeMaterial(key string) (material, qualifier string) {
	for {
		var q string
		switch {
		case strings.HasPrefix(key, "recycled_") && len(key) > len("recycled_"):
			q = "recycled"
		case strings.HasPrefix(key, "organic_") && len(key) > len("organic_"):
			q = "organic"
		default:
			return key, qualifier
		}
		key = strings.TrimPrefix(key, q+"_")
		if qualifier == "" {
			qualifier = q
		}
	}
}

// Footwear is a parsed footwear_materials cell; a part that is not stated is "".
type Footwear struct{ Upper, LiningAndSock, OuterSole string }

// ParseFootwearMaterials parses "upper: leather; lining_and_sock: textile;
// outer_sole: other" (parts and values lowercased, spaces → "_").
func ParseFootwearMaterials(s string) (*Footwear, error) {
	out := &Footwear{}
	seen := map[string]bool{}
	for _, raw := range strings.Split(s, ";") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		name, value, ok := strings.Cut(raw, ":")
		if !ok {
			return nil, fmt.Errorf("%q: expected \"<part>: <material>\" with part upper, lining_and_sock or outer_sole", raw)
		}
		part, v := Key(name), Key(value)
		if !contains(FootwearParts, part) {
			return nil, fmt.Errorf("%q is not a footwear part (upper, lining_and_sock, outer_sole)", strings.TrimSpace(name))
		}
		if !contains(FootwearValues, v) {
			return nil, fmt.Errorf("%q is not a footwear material (leather, coated_leather, textile, other)", strings.TrimSpace(value))
		}
		if seen[part] {
			return nil, fmt.Errorf("part %s appears twice", part)
		}
		seen[part] = true
		switch part {
		case "upper":
			out.Upper = v
		case "lining_and_sock":
			out.LiningAndSock = v
		default:
			out.OuterSole = v
		}
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("no footwear materials found")
	}
	return out, nil
}

// Certification is one parsed certification; Licence is "" without a licence number.
type Certification struct{ Scheme, Licence, Scope string }

var scopeSuffix = regexp.MustCompile(`(?i)\s*\((material|product)\)\s*$`)

// ParseCertifications parses "gots:CU 123456; rws:CU 200101 (material)".
// The scheme is a key; the licence number is everything after the first ":"
// (none without ":"); the scope is product unless "(material)" is given.
func ParseCertifications(s string) ([]Certification, error) {
	var out []Certification
	for _, raw := range strings.Split(s, ";") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		c := Certification{Scope: "product"}
		if m := scopeSuffix.FindStringSubmatch(raw); m != nil {
			c.Scope = strings.ToLower(m[1])
			raw = strings.TrimSpace(raw[:len(raw)-len(m[0])])
		}
		scheme, licence, _ := strings.Cut(raw, ":")
		c.Scheme = Key(scheme)
		if !ValidKey(c.Scheme) {
			return nil, fmt.Errorf("certification scheme %q is not a key such as gots or oeko_tex_standard_100", strings.TrimSpace(scheme))
		}
		c.Licence = strings.TrimSpace(licence)
		if len([]rune(c.Licence)) > 200 {
			return nil, fmt.Errorf("the licence number of %s is longer than 200 characters", c.Scheme)
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no certifications found")
	}
	return out, nil
}

var measurementNumber = regexp.MustCompile(`^([0-9]+([.,][0-9]+)?|[.,][0-9]+)$`)

// ParseMeasurement parses a positive number with "." or "," as decimal
// separator, without thousands separator or unit ("2.6", "2,6").
func ParseMeasurement(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if !measurementNumber.MatchString(s) {
		return 0, fmt.Errorf("%q is not a number (for example 2.6 or 2,6, without unit)", s)
	}
	v, err := strconv.ParseFloat(strings.Replace(s, ",", ".", 1), 64)
	if err != nil || !(v > 0) {
		return 0, fmt.Errorf("%q is not a positive number", s)
	}
	return v, nil
}

// ParseStrapLength parses "min-max" (80-130) or one number for a strap that
// cannot be adjusted (120 = 120-120).
func ParseStrapLength(s string) (min, max float64, err error) {
	s = strings.TrimSpace(s)
	lo, hi, ok := strings.Cut(s, "-")
	if !ok {
		v, err := ParseMeasurement(s)
		return v, v, err
	}
	if min, err = ParseMeasurement(lo); err != nil {
		return 0, 0, fmt.Errorf("%q: %v (use min-max, for example 80-130)", s, err)
	}
	if max, err = ParseMeasurement(hi); err != nil {
		return 0, 0, fmt.Errorf("%q: %v (use min-max, for example 80-130)", s, err)
	}
	if min > max {
		return 0, 0, fmt.Errorf("%q: the shortest setting comes first (min-max, for example 80-130)", s)
	}
	return min, max, nil
}

// ParseColorFamily lowercases a colour family; gray and multicolor are
// accepted for grey and multicolour.
func ParseColorFamily(s string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(s))
	switch v {
	case "gray":
		v = "grey"
	case "multicolor":
		v = "multicolour"
	}
	if !contains(ColorFamilies, v) {
		return "", fmt.Errorf("%q is not a colour family (%s)", strings.TrimSpace(s), strings.Join(ColorFamilies, ", "))
	}
	return v, nil
}

// ParseFeature parses the cell of a feature:<key> column: for a registry
// key a value of its type (enums as keys, integers as digits, booleans as
// yes/no, lists separated by "|" without duplicates), anything else is an
// error; a key outside the registry keeps the cell as a string.
func ParseFeature(key, s string) (any, error) {
	s = strings.TrimSpace(s)
	spec, ok := FeatureRegistry[key]
	if !ok {
		return s, nil
	}
	switch spec.Type {
	case FeatureEnum:
		v := Key(s)
		if !contains(spec.Values, v) {
			return nil, fmt.Errorf("%q is not one of %s", s, strings.Join(spec.Values, ", "))
		}
		return v, nil
	case FeatureInteger:
		n, err := strconv.Atoi(s)
		if err != nil || strings.ContainsAny(s, "+-") || !contains(spec.Values, strconv.Itoa(n)) {
			return nil, fmt.Errorf("%q is not one of %s", s, strings.Join(spec.Values, ", "))
		}
		return n, nil
	case FeatureBoolean:
		b, ok := ParseBool(s)
		if !ok {
			return nil, fmt.Errorf("%q is not yes or no", s)
		}
		return b, nil
	}
	var list []any
	seen := map[string]bool{}
	for _, raw := range strings.Split(s, "|") {
		v := Key(raw)
		if v == "" {
			continue
		}
		if !contains(spec.Values, v) {
			return nil, fmt.Errorf("%q is not one of %s", strings.TrimSpace(raw), strings.Join(spec.Values, ", "))
		}
		if !seen[v] {
			seen[v] = true
			list = append(list, v)
		}
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("%q is not a list of %s", s, strings.Join(spec.Values, ", "))
	}
	return list, nil
}

// FormatNumber writes a number with "." and without trailing zeros.
func FormatNumber(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// FormatFeature writes a feature value as ParseFeature reads it.
func FormatFeature(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case bool:
		if x {
			return "yes"
		}
		return "no"
	case []any:
		parts := make([]string, 0, len(x))
		for _, e := range x {
			parts = append(parts, FormatFeature(e))
		}
		return strings.Join(parts, "|")
	case []string:
		return strings.Join(x, "|")
	case float64:
		return FormatNumber(x)
	case string:
		return x
	}
	return fmt.Sprint(v)
}

// FeatureColumns lists the feature keys of an export: every registry key,
// then the other keys of the exported products, sorted.
func FeatureColumns(other map[string]bool) []string {
	keys := append([]string(nil), FeatureKeys...)
	var extra []string
	for k := range other {
		if _, reg := FeatureRegistry[k]; !reg {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	return append(keys, extra...)
}
