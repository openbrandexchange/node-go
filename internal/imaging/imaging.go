// Package imaging detects image types, reads dimensions from file headers
// and renders the OBX renditions (web, square, thumb) in pure Go. Originals
// are never modified: renditions are new JPEG files.
package imaging

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png" // register decoder
	"io"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/tiff" // register decoder
	_ "golang.org/x/image/webp" // register decoder
)

// Rendition limits (docs/protocol.md §5.5).
const (
	WebMax      = 2000
	ThumbMax    = 600
	MaxPixels   = 100_000_000 // decompression-bomb guard
	JPEGQuality = 85
)

// AcceptedTypes are the original media types this node accepts.
var AcceptedTypes = []string{"image/jpeg", "image/png", "image/webp", "image/tiff"}

// Sniff returns the media type from the file signature ("" when unknown or
// not an accepted image type). SVG is never accepted.
func Sniff(head []byte) string {
	switch {
	case len(head) >= 3 && head[0] == 0xFF && head[1] == 0xD8 && head[2] == 0xFF:
		return "image/jpeg"
	case len(head) >= 8 && bytes.Equal(head[:8], []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case len(head) >= 12 && bytes.Equal(head[:4], []byte("RIFF")) && bytes.Equal(head[8:12], []byte("WEBP")):
		return "image/webp"
	case len(head) >= 4 && (bytes.Equal(head[:4], []byte("II*\x00")) || bytes.Equal(head[:4], []byte("MM\x00*"))):
		return "image/tiff"
	}
	return ""
}

// Ext returns the usual file extension for a media type.
func Ext(ct string) string {
	switch ct {
	case "image/jpeg":
		return "jpg"
	case "image/png":
		return "png"
	case "image/webp":
		return "webp"
	case "image/tiff":
		return "tif"
	}
	return "bin"
}

// Dimensions reads width and height from the header without decoding the
// pixels. For JPEG the EXIF orientation is NOT applied (the numbers describe
// the stored bytes, like every other tool reporting file dimensions).
func Dimensions(r io.Reader) (w, h int, ct string, err error) {
	cfg, format, err := image.DecodeConfig(r)
	if err != nil {
		return 0, 0, "", err
	}
	switch format {
	case "jpeg":
		ct = "image/jpeg"
	case "png":
		ct = "image/png"
	case "webp":
		ct = "image/webp"
	case "tiff":
		ct = "image/tiff"
	default:
		return 0, 0, "", fmt.Errorf("unsupported image format %s", format)
	}
	return cfg.Width, cfg.Height, ct, nil
}

// Rendition is one rendered file.
type Rendition struct {
	Name   string
	Data   []byte
	Width  int
	Height int
}

// ErrTooLarge is returned for images above MaxPixels.
var ErrTooLarge = errors.New("image exceeds the pixel limit")

// Render decodes an original and returns web, square and thumb JPEGs.
func Render(original []byte) ([]Rendition, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(original))
	if err != nil {
		return nil, fmt.Errorf("cannot read image: %w", err)
	}
	if int64(cfg.Width)*int64(cfg.Height) > MaxPixels {
		return nil, ErrTooLarge
	}
	src, _, err := image.Decode(bytes.NewReader(original))
	if err != nil {
		return nil, fmt.Errorf("cannot decode image: %w", err)
	}
	if Sniff(original) == "image/jpeg" {
		src = applyOrientation(src, jpegOrientation(original))
	}
	// Flatten on white into an RGBA image (fast paths for the scalers).
	sb := src.Bounds()
	flat := image.NewRGBA(image.Rect(0, 0, sb.Dx(), sb.Dy()))
	switch src.(type) {
	case *image.YCbCr, *image.Gray, *image.CMYK:
		draw.Draw(flat, flat.Bounds(), src, sb.Min, draw.Src) // opaque
	default:
		draw.Draw(flat, flat.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
		draw.Draw(flat, flat.Bounds(), src, sb.Min, draw.Over)
	}
	// web: longest side <= 2000, never upscaled.
	ww, wh := fit(sb.Dx(), sb.Dy(), WebMax)
	web := resize(flat, ww, wh)
	// square: web centred on a white square whose side is web's longest side.
	side := max(ww, wh)
	sq := image.NewRGBA(image.Rect(0, 0, side, side))
	draw.Draw(sq, sq.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	off := image.Pt((side-ww)/2, (side-wh)/2)
	draw.Draw(sq, image.Rectangle{Min: off, Max: off.Add(image.Pt(ww, wh))}, web, image.Point{}, draw.Src)
	// thumb: longest side <= 600 (from web; web already has the original
	// aspect ratio), never upscaled.
	tw, th := fit(sb.Dx(), sb.Dy(), ThumbMax)
	thumb := resize(web, tw, th)
	var out []Rendition
	for _, r := range []struct {
		name string
		img  *image.RGBA
	}{{"web", web}, {"square", sq}, {"thumb", thumb}} {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, r.img, &jpeg.Options{Quality: JPEGQuality}); err != nil {
			return nil, err
		}
		out = append(out, Rendition{Name: r.name, Data: buf.Bytes(), Width: r.img.Bounds().Dx(), Height: r.img.Bounds().Dy()})
	}
	return out, nil
}

// resize scales src to w×h: box-halving while the image is more than twice
// the target (fast, no aliasing), then Catmull-Rom for the final step.
func resize(src *image.RGBA, w, h int) *image.RGBA {
	if src.Bounds().Dx() == w && src.Bounds().Dy() == h {
		return src
	}
	for src.Bounds().Dx() >= 2*w && src.Bounds().Dy() >= 2*h {
		src = halve(src)
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)
	return dst
}

// halve averages 2×2 blocks.
func halve(src *image.RGBA) *image.RGBA {
	w, h := src.Bounds().Dx()/2, src.Bounds().Dy()/2
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	sp, dp := src.Pix, dst.Pix
	for y := 0; y < h; y++ {
		r0 := (2 * y) * src.Stride
		r1 := r0 + src.Stride
		d := y * dst.Stride
		for x := 0; x < w; x++ {
			a, b := r0+8*x, r1+8*x
			for c := 0; c < 4; c++ {
				dp[d+4*x+c] = uint8((uint16(sp[a+c]) + uint16(sp[a+4+c]) + uint16(sp[b+c]) + uint16(sp[b+4+c]) + 2) / 4)
			}
		}
	}
	return dst
}

// fit scales (w,h) so the longest side is at most limit, never upscaling.
func fit(w, h, limit int) (int, int) {
	if w <= limit && h <= limit {
		return w, h
	}
	if w >= h {
		nh := int(float64(h)*float64(limit)/float64(w) + 0.5)
		return limit, max(nh, 1)
	}
	nw := int(float64(w)*float64(limit)/float64(h) + 0.5)
	return max(nw, 1), limit
}

// Fit is exported for callers that need rendition sizes without rendering.
func Fit(w, h, limit int) (int, int) { return fit(w, h, limit) }

// jpegOrientation returns the EXIF orientation (1..8) of a JPEG, 1 when absent.
func jpegOrientation(b []byte) int {
	i := 2
	for i+4 <= len(b) {
		if b[i] != 0xFF {
			return 1
		}
		marker := b[i+1]
		if marker == 0xDA || marker == 0xD9 {
			return 1
		}
		size := int(binary.BigEndian.Uint16(b[i+2:]))
		if size < 2 || i+2+size > len(b) {
			return 1
		}
		seg := b[i+4 : i+2+size]
		if marker == 0xE1 && len(seg) > 14 && bytes.Equal(seg[:6], []byte("Exif\x00\x00")) {
			return exifOrientation(seg[6:])
		}
		i += 2 + size
	}
	return 1
}

func exifOrientation(t []byte) int {
	if len(t) < 8 {
		return 1
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	off := int(bo.Uint32(t[4:8]))
	if off+2 > len(t) {
		return 1
	}
	n := int(bo.Uint16(t[off:]))
	for k := 0; k < n; k++ {
		e := off + 2 + k*12
		if e+12 > len(t) {
			return 1
		}
		if bo.Uint16(t[e:]) == 0x0112 {
			v := int(bo.Uint16(t[e+8:]))
			if v >= 1 && v <= 8 {
				return v
			}
			return 1
		}
	}
	return 1
}

// applyOrientation returns src rotated/flipped per EXIF orientation.
func applyOrientation(src image.Image, o int) image.Image {
	if o <= 1 || o > 8 {
		return src
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dw, dh := w, h
	if o >= 5 {
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var dx, dy int
			switch o {
			case 2:
				dx, dy = w-1-x, y
			case 3:
				dx, dy = w-1-x, h-1-y
			case 4:
				dx, dy = x, h-1-y
			case 5:
				dx, dy = y, x
			case 6:
				dx, dy = h-1-y, x
			case 7:
				dx, dy = h-1-y, w-1-x
			case 8:
				dx, dy = y, w-1-x
			}
			dst.Set(dx, dy, src.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}
