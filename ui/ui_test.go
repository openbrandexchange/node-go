package ui

import (
	"encoding/json"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The 25 languages of the product (spec/docs/glossary.md).
var languages = strings.Fields("bg cs da de el en es et fi fr ga hr hu it lt lv mt nl nb pl pt ro sk sl sv")

var pluralSuffix = regexp.MustCompile(`_(zero|one|two|few|many|other)$`)
var placeholder = regexp.MustCompile(`\{\{\s*([a-zA-Z0-9_]+)\s*\}\}`)

func flatten(prefix string, v any, out map[string]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, vv := range x {
			key := k
			if prefix != "" {
				key = prefix + "." + k
			}
			flatten(key, vv, out)
		}
	case string:
		out[prefix] = x
	}
}

func load(t *testing.T, lang string) map[string]string {
	t.Helper()
	b, err := fs.ReadFile(FS(), "locales/"+lang+".json")
	if err != nil {
		t.Fatalf("%s: %v", lang, err)
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("%s.json: %v", lang, err)
	}
	out := map[string]string{}
	flatten("", v, out)
	return out
}

// Every language has every text of the English source (plural forms may
// differ per language), with the same placeholders, and nothing empty.
func TestLocalesComplete(t *testing.T) {
	var index struct {
		Languages []string `json:"languages"`
	}
	b, err := fs.ReadFile(FS(), "locales/index.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &index); err != nil {
		t.Fatal(err)
	}
	got := append([]string(nil), index.Languages...)
	want := append([]string(nil), languages...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("locales/index.json lists %v, want the 25 languages %v", got, want)
	}
	en := load(t, "en")
	base := func(m map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range m {
			out[pluralSuffix.ReplaceAllString(k, "")] = v
		}
		return out
	}
	enBase := base(en)
	plural := map[string]bool{} // plural forms may leave the number out ("yesterday")
	for k := range en {
		if pluralSuffix.MatchString(k) {
			plural[pluralSuffix.ReplaceAllString(k, "")] = true
		}
	}
	for _, lang := range languages {
		m := load(t, lang)
		mb := base(m)
		var missing, empty, wrongVars []string
		for k, ev := range enBase {
			v, ok := mb[k]
			if !ok {
				missing = append(missing, k)
				continue
			}
			if strings.TrimSpace(v) == "" && strings.TrimSpace(ev) != "" {
				empty = append(empty, k)
			}
			if vars(ev) != vars(v) && !plural[k] {
				wrongVars = append(wrongVars, k+" ("+vars(ev)+" vs "+vars(v)+")")
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("%s: %d missing texts, e.g. %v", lang, len(missing), missing[:min(5, len(missing))])
		}
		if len(empty) > 0 {
			t.Errorf("%s: empty texts %v", lang, empty[:min(5, len(empty))])
		}
		if len(wrongVars) > 0 {
			t.Errorf("%s: placeholders differ from English: %v", lang, wrongVars[:min(5, len(wrongVars))])
		}
	}
}

func vars(s string) string {
	var out []string
	seen := map[string]bool{}
	for _, m := range placeholder.FindAllStringSubmatch(s, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// The node composes its e-mails from these texts (internal/node/mail.go).
func TestEmailTextsPresent(t *testing.T) {
	for _, lang := range languages {
		m := load(t, lang)
		for _, kind := range []string{"invite", "accessRequestReceived", "accessRequestNew", "accessApproved", "accessRejected", "zipReady", "passwordReset"} {
			if m["email."+kind+".subject"] == "" {
				t.Errorf("%s: email.%s.subject missing", lang, kind)
			}
		}
		if m["glossary.portal"] == "" {
			t.Errorf("%s: glossary.portal (the local word for the portal) missing", lang)
		}
	}
}
