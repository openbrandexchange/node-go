package importfmt

import (
	"archive/zip"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"
)

// Roles and views (docs/protocol.md §5.4).
var (
	Roles = map[string]bool{"packshot": true, "model": true, "detail": true, "ghost": true, "flatlay": true, "scene": true, "swatch": true, "size_chart": true, "other": true}
	Views = map[string]bool{"front": true, "back": true, "side": true, "left": true, "right": true, "top": true, "bottom": true, "tilted_left": true, "tilted_right": true, "inside": true, "detail": true, "other": true}
)

// ImageExts are the image extensions of the format.
var ImageExts = map[string]bool{"jpg": true, "jpeg": true, "png": true, "webp": true, "tif": true, "tiff": true}

// IsImage reports whether a file name has an image extension.
func IsImage(name string) bool {
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(name), "."))
	return ImageExts[ext]
}

// ImageName is a parsed image file name.
type ImageName struct {
	Style    string
	Color    string
	Position int
	Role     string // "" = not given
	View     string // "" = not given
}

// ParseImageName parses {style}_{color}_{position}[_{qualifier}…].{ext}
// from the right.
func ParseImageName(name string) (*ImageName, error) {
	base := path.Base(strings.ReplaceAll(name, "\\", "/"))
	ext := path.Ext(base)
	if !ImageExts[strings.ToLower(strings.TrimPrefix(ext, "."))] {
		return nil, fmt.Errorf("%s is not an image file (jpg, png, webp, tif)", base)
	}
	parts := strings.Split(strings.TrimSuffix(base, ext), "_")
	out := &ImageName{}
	// Pop qualifiers (roles/views) while more than three parts remain.
	for len(parts) > 3 {
		last := strings.ToLower(parts[len(parts)-1])
		// tilted_left / tilted_right / size_chart span two parts.
		if len(parts) > 4 {
			two := strings.ToLower(parts[len(parts)-2]) + "_" + last
			if Views[two] || Roles[two] {
				if !assign(out, two) {
					break
				}
				parts = parts[:len(parts)-2]
				continue
			}
		}
		if !(Roles[last] || Views[last]) {
			break
		}
		if !assign(out, last) {
			break
		}
		parts = parts[:len(parts)-1]
	}
	if len(parts) < 3 {
		return nil, fmt.Errorf("%s does not follow {style}_{color}_{position}.jpg (position missing)", base)
	}
	posStr := parts[len(parts)-1]
	pos, err := strconv.Atoi(posStr)
	if err != nil || pos < 1 || pos > 999 || strings.HasPrefix(posStr, "0") || strconv.Itoa(pos) != posStr {
		return nil, fmt.Errorf("%s does not follow {style}_{color}_{position}.jpg (position must be 1-999)", base)
	}
	out.Position = pos
	out.Color = parts[len(parts)-2]
	out.Style = strings.Join(parts[:len(parts)-2], "_")
	if out.Style == "" || out.Color == "" {
		return nil, fmt.Errorf("%s does not follow {style}_{color}_{position}.jpg", base)
	}
	return out, nil
}

// assign sets a qualifier; false when it conflicts with an earlier one.
func assign(n *ImageName, q string) bool {
	if q == "detail" {
		if n.Role == "" {
			n.Role = "detail"
		}
		if n.View == "" {
			n.View = "detail"
		}
		return true
	}
	if Roles[q] && n.Role == "" {
		n.Role = q
		return true
	}
	if Views[q] && n.View == "" {
		n.View = q
		return true
	}
	return false
}

// Ignored reports whether a package entry is junk that must be skipped.
func Ignored(name string) bool {
	name = strings.ReplaceAll(name, "\\", "/")
	if strings.HasPrefix(name, "__MACOSX/") || strings.Contains(name, "/__MACOSX/") {
		return true
	}
	base := path.Base(name)
	return strings.HasPrefix(base, ".") || base == "Thumbs.db" || base == "desktop.ini"
}

var windowsDevices = map[string]bool{"con": true, "prn": true, "aux": true, "nul": true, "com1": true, "com2": true, "com3": true, "com4": true, "lpt1": true, "lpt2": true, "lpt3": true}

// SafeEntry checks a zip entry against zip-slip and similar tricks.
func SafeEntry(f *zip.File) error {
	name := f.Name
	if strings.Contains(name, "\x00") {
		return fmt.Errorf("entry name contains a NUL byte")
	}
	n := strings.ReplaceAll(name, "\\", "/")
	if strings.HasPrefix(n, "/") {
		return fmt.Errorf("entry %q has an absolute path", name)
	}
	if len(n) >= 2 && n[1] == ':' {
		return fmt.Errorf("entry %q has a drive letter", name)
	}
	for _, seg := range strings.Split(n, "/") {
		if seg == ".." {
			return fmt.Errorf("entry %q contains '..'", name)
		}
		base := strings.ToLower(seg)
		if i := strings.IndexByte(base, '.'); i >= 0 {
			base = base[:i]
		}
		if windowsDevices[base] {
			return fmt.Errorf("entry %q uses a device name", name)
		}
	}
	if f.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("entry %q is a symbolic link", name)
	}
	return nil
}
