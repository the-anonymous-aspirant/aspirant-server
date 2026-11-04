package invoicing_pdf

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"net/http"
)

// Logo limits (system_3 #9858). The byte cap bounds the upload; the pixel
// caps are read from the image header and refuse a decompression bomb before
// any pixel is decoded (a small file can declare a huge canvas). The output
// bound is print-sized: the logo is drawn at most 140×72 pt, so 600 px on the
// long side is over 300 dpi at that size.
const (
	MaxLogoBytes     = 5 << 20
	maxLogoSide      = 8000
	maxLogoPixels    = 25_000_000
	LogoOutputMaxPx  = 600
	logoMinVisibleA8 = 0 // an alpha above this is ink, for the trim
)

// LogoError is a logo the server refuses. Its message is safe to show the
// user and names what to change.
type LogoError struct{ msg string }

func (e *LogoError) Error() string { return e.msg }

func logoErrorf(format string, a ...interface{}) error {
	return &LogoError{msg: fmt.Sprintf(format, a...)}
}

// IsLogoError reports whether err is a refusal the user can act on.
func IsLogoError(err error) bool {
	var le *LogoError
	return errors.As(err, &le)
}

// Logo is a normalized logo ready to print: PNG bytes and their pixel size.
type Logo struct {
	PNG    []byte
	Width  int
	Height int
}

// NormalizeLogo turns an uploaded PNG or JPEG into the PNG the invoice
// prints. The type is sniffed from the bytes, never taken from the client. It
// trims a fully transparent margin, so the drawn box is the ink and not the
// canvas around it, scales the result down to LogoOutputMaxPx on the long
// side, and re-encodes it, which drops any metadata the file carried.
func NormalizeLogo(raw []byte) (Logo, error) {
	if len(raw) == 0 {
		return Logo{}, logoErrorf("the logo file is empty")
	}
	if len(raw) > MaxLogoBytes {
		return Logo{}, logoErrorf("the logo is larger than %d MB", MaxLogoBytes>>20)
	}
	var (
		decodeConfig func([]byte) (image.Config, error)
		decode       func([]byte) (image.Image, error)
	)
	switch http.DetectContentType(raw) {
	case "image/png":
		decodeConfig = func(b []byte) (image.Config, error) { return png.DecodeConfig(bytes.NewReader(b)) }
		decode = func(b []byte) (image.Image, error) { return png.Decode(bytes.NewReader(b)) }
	case "image/jpeg":
		decodeConfig = func(b []byte) (image.Config, error) { return jpeg.DecodeConfig(bytes.NewReader(b)) }
		decode = func(b []byte) (image.Image, error) { return jpeg.Decode(bytes.NewReader(b)) }
	default:
		return Logo{}, logoErrorf("the logo must be a PNG or JPEG image")
	}
	cfg, err := decodeConfig(raw)
	if err != nil {
		return Logo{}, logoErrorf("the logo image could not be read")
	}
	if cfg.Width < 1 || cfg.Height < 1 {
		return Logo{}, logoErrorf("the logo image has no pixels")
	}
	if cfg.Width > maxLogoSide || cfg.Height > maxLogoSide || cfg.Width*cfg.Height > maxLogoPixels {
		return Logo{}, logoErrorf("the logo is %d×%d pixels; the limit is %d pixels on a side and %d megapixels",
			cfg.Width, cfg.Height, maxLogoSide, maxLogoPixels/1_000_000)
	}
	src, err := decode(raw)
	if err != nil {
		return Logo{}, logoErrorf("the logo image could not be read")
	}

	// Premultiplied RGBA, so averaging never pulls the colour of a
	// transparent pixel into the ink's edge.
	b := src.Bounds()
	rgba := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(rgba, rgba.Bounds(), src, b.Min, draw.Src)

	ink := inkBounds(rgba)
	if ink.Empty() {
		return Logo{}, logoErrorf("the logo image is fully transparent")
	}
	out := downscale(rgba.SubImage(ink).(*image.RGBA), LogoOutputMaxPx)

	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return Logo{}, fmt.Errorf("encode logo: %w", err)
	}
	return Logo{PNG: buf.Bytes(), Width: out.Bounds().Dx(), Height: out.Bounds().Dy()}, nil
}

// inkBounds is the smallest rectangle holding every pixel that is not fully
// transparent. An opaque image is its own bounds.
func inkBounds(img *image.RGBA) image.Rectangle {
	b := img.Bounds()
	minX, minY, maxX, maxY := b.Max.X, b.Max.Y, b.Min.X-1, b.Min.Y-1
	for y := b.Min.Y; y < b.Max.Y; y++ {
		row := img.Pix[img.PixOffset(b.Min.X, y) : img.PixOffset(b.Max.X-1, y)+4]
		for x := 0; x < b.Dx(); x++ {
			if row[4*x+3] > logoMinVisibleA8 {
				if b.Min.X+x < minX {
					minX = b.Min.X + x
				}
				if b.Min.X+x > maxX {
					maxX = b.Min.X + x
				}
				if y < minY {
					minY = y
				}
				if y > maxY {
					maxY = y
				}
			}
		}
	}
	if maxX < minX {
		return image.Rectangle{}
	}
	return image.Rect(minX, minY, maxX+1, maxY+1)
}

// downscale shrinks img so its long side is at most maxSide, averaging each
// output pixel over the source pixels it covers. An image already within the
// bound is copied unscaled.
func downscale(img *image.RGBA, maxSide int) *image.RGBA {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	ow, oh := w, h
	if w > maxSide || h > maxSide {
		if w >= h {
			ow, oh = maxSide, max(1, (h*maxSide+w/2)/w)
		} else {
			ow, oh = max(1, (w*maxSide+h/2)/h), maxSide
		}
	}
	out := image.NewRGBA(image.Rect(0, 0, ow, oh))
	for oy := 0; oy < oh; oy++ {
		sy0, sy1 := oy*h/oh, max((oy+1)*h/oh, oy*h/oh+1)
		for ox := 0; ox < ow; ox++ {
			sx0, sx1 := ox*w/ow, max((ox+1)*w/ow, ox*w/ow+1)
			var r, g, bl, a, n uint64
			for sy := sy0; sy < sy1; sy++ {
				off := img.PixOffset(b.Min.X+sx0, b.Min.Y+sy)
				for sx := sx0; sx < sx1; sx++ {
					p := img.Pix[off : off+4 : off+4]
					r, g, bl, a = r+uint64(p[0]), g+uint64(p[1]), bl+uint64(p[2]), a+uint64(p[3])
					off += 4
				}
				n += uint64(sx1 - sx0)
			}
			o := out.PixOffset(ox, oy)
			out.Pix[o], out.Pix[o+1], out.Pix[o+2], out.Pix[o+3] =
				uint8((r+n/2)/n), uint8((g+n/2)/n), uint8((bl+n/2)/n), uint8((a+n/2)/n)
		}
	}
	return out
}
