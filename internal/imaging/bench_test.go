package imaging

import (
	"os"
	"testing"
)

func BenchmarkRender(b *testing.B) {
	data, err := os.ReadFile("../../testdata/spec/kestrel/images/KO-1002_OLV_1_front.png")
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < b.N; i++ {
		if _, err := Render(data); err != nil {
			b.Fatal(err)
		}
	}
}
