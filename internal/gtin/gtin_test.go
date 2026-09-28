package gtin

import "testing"

func TestGTIN(t *testing.T) {
	if n, ok := Normalize(" 2000 100101025 "); !ok || n != "02000100101025" {
		t.Fatal(n)
	}
	if _, ok := Normalize("12345"); ok {
		t.Fatal("5 digits")
	}
	if !ValidCheckDigit("2000100101025") || ValidCheckDigit("2000100101026") || !ValidCheckDigit("96385074") || !ValidCheckDigit("012345678905") {
		t.Fatal("check digits")
	}
}
