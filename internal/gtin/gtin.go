// Package gtin validates and normalises GS1 trade item numbers.
package gtin

import "strings"

// Clean removes spaces (and surrounding whitespace) from input.
func Clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == ' ' {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Normalize returns the 14-digit form of an 8, 12, 13 or 14 digit code, or
// ok=false. The check digit is not validated (lenient lookup rule).
func Normalize(s string) (string, bool) {
	s = Clean(s)
	if !allDigits(s) {
		return "", false
	}
	switch len(s) {
	case 8, 12, 13, 14:
		return strings.Repeat("0", 14-len(s)) + s, true
	}
	return "", false
}

// ValidCheckDigit reports whether an 8/12/13/14 digit GTIN has a correct
// GS1 mod-10 check digit.
func ValidCheckDigit(s string) bool {
	n, ok := Normalize(s)
	if !ok || Clean(s) != s {
		return false
	}
	sum := 0
	for i := 0; i < 13; i++ {
		d := int(n[i] - '0')
		if i%2 == 0 {
			sum += d * 3
		} else {
			sum += d
		}
	}
	return (10-sum%10)%10 == int(n[13]-'0')
}
