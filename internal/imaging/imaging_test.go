package imaging

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func pngOf(w, h int, transparent bool) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			a := uint8(255)
			if transparent {
				a = 0
			}
			img.Set(x, y, color.NRGBA{R: 200, G: 30, B: 30, A: a})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func TestRenderSizes(t *testing.T) {
	cases := []struct{ w, h, ww, wh, sq, tw, th int }{
		{2400, 3000, 1600, 2000, 2000, 480, 600},
		{1200, 1500, 1200, 1500, 1500, 480, 600},
		{300, 200, 300, 200, 300, 300, 200}, // never upscaled
	}
	for _, c := range cases {
		out, err := Render(pngOf(c.w, c.h, false))
		if err != nil {
			t.Fatal(err)
		}
		got := map[string][2]int{}
		for _, r := range out {
			cfg, format, err := image.DecodeConfig(bytes.NewReader(r.Data))
			if err != nil || format != "jpeg" || cfg.Width != r.Width || cfg.Height != r.Height {
				t.Fatalf("%s: %v %s %dx%d", r.Name, err, format, cfg.Width, cfg.Height)
			}
			got[r.Name] = [2]int{r.Width, r.Height}
		}
		if got["web"] != [2]int{c.ww, c.wh} || got["square"] != [2]int{c.sq, c.sq} || got["thumb"] != [2]int{c.tw, c.th} {
			t.Errorf("%dx%d: %v", c.w, c.h, got)
		}
	}
}

func TestTransparencyIsFlattenedOnWhite(t *testing.T) {
	out, err := Render(pngOf(100, 100, true))
	if err != nil {
		t.Fatal(err)
	}
	img, _, _ := image.Decode(bytes.NewReader(out[0].Data))
	r, g, b, _ := img.At(50, 50).RGBA()
	if r>>8 < 250 || g>>8 < 250 || b>>8 < 250 {
		t.Fatalf("expected white, got %d %d %d", r>>8, g>>8, b>>8)
	}
}

func TestSniff(t *testing.T) {
	if Sniff(pngOf(2, 2, false)) != "image/png" || Sniff([]byte("<svg")) != "" || Sniff([]byte{0xFF, 0xD8, 0xFF, 0xE0}) != "image/jpeg" {
		t.Fatal("sniff")
	}
}
