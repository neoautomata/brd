package imgconv

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
	"os"

	"github.com/neoautomata/brd/internal/atomicfile"
)

const (
	// DefaultWidth is the portrait width in pixels for the LilyGO T5 display.
	DefaultWidth = 540
	// DefaultHeight is the portrait height in pixels for the LilyGO T5 display.
	DefaultHeight = 960

	maxImageDimension = 16384
	maxImagePixels    = 64_000_000
	maxInputFileBytes = 64 << 20 // 64 MiB
)

// Options sets the target portrait dimensions before the 90-degree counter-clockwise rotation.
type Options struct {
	Width  int
	Height int
}

func (o Options) normalized() (int, int, error) {
	w := o.Width
	h := o.Height
	if w == 0 {
		w = DefaultWidth
	}
	if h == 0 {
		h = DefaultHeight
	}
	if err := validateDimensions("target", w, h); err != nil {
		return 0, 0, err
	}
	return w, h, nil
}

func validateDimensions(kind string, w, h int) error {
	if w <= 0 || h <= 0 {
		return fmt.Errorf("invalid %s dimensions %dx%d", kind, w, h)
	}
	if w > maxImageDimension || h > maxImageDimension {
		return fmt.Errorf("%s dimensions %dx%d exceed maximum %d", kind, w, h, maxImageDimension)
	}
	if int64(w) > int64(maxImagePixels)/int64(h) {
		return fmt.Errorf("%s pixel count %dx%d exceeds maximum %d", kind, w, h, maxImagePixels)
	}
	return nil
}

// Convert resizes src to opts.Width by opts.Height and converts pixels to ITU-R 601-2 grayscale.
// It rotates the image 90 degrees counter-clockwise and packs two pixels into each byte.
// It returns the raw framebuffer bytes and the lowercase SHA-256 digest in hex.
func Convert(src image.Image, opts Options) ([]byte, string, error) {
	if src == nil {
		return nil, "", errors.New("source image is nil")
	}
	bounds := src.Bounds()
	srcW := bounds.Dx()
	srcH := bounds.Dy()
	if err := validateDimensions("source", srcW, srcH); err != nil {
		return nil, "", err
	}

	targetW, targetH, err := opts.normalized()
	if err != nil {
		return nil, "", err
	}

	// Build a portrait buffer of 8-bit grayscale pixels with size targetW by targetH.
	gray := fitToGrayscale(src, targetW, targetH)

	// Rotate 90 degrees counter-clockwise so dstW equals targetH and dstH equals targetW.
	// Map each output pixel to srcX = targetW - 1 - dstY and srcY = dstX.
	dstW := targetH
	dstH := targetW
	rowBytes := (dstW + 1) / 2
	raw := make([]byte, rowBytes*dstH)

	idx := 0
	for dstY := 0; dstY < dstH; dstY++ {
		srcX := targetW - 1 - dstY
		var packed byte
		hasLow := false
		for dstX := 0; dstX < dstW; dstX++ {
			srcY := dstX
			pixel := gray[srcY*targetW+srcX]
			if dstX%2 == 0 {
				packed = pixel >> 4
				hasLow = true
			} else {
				packed |= pixel & 0xF0
				raw[idx] = packed
				idx++
				hasLow = false
			}
		}
		if hasLow {
			raw[idx] = packed
			idx++
		}
	}

	sum := sha256.Sum256(raw)
	return raw, hex.EncodeToString(sum[:]), nil
}

// ConvertFile decodes an image from inPath and writes the 4bpp output to outPath.
// It also writes the SHA-256 digest to outPath+".sha" with 0600 permissions.
// The firmware uses this digest only to detect display changes, not as a security integrity check.
// The firmware skips local hashing to save CPU time and power.
func ConvertFile(inPath, outPath string, opts Options) error {
	if _, _, err := opts.normalized(); err != nil {
		return err
	}

	f, err := os.Open(inPath)
	if err != nil {
		return fmt.Errorf("open input image: %w", err)
	}
	defer f.Close()

	if info, err := f.Stat(); err == nil && info.Mode().IsRegular() && info.Size() > maxInputFileBytes {
		return fmt.Errorf("input image exceeds maximum size %d bytes", maxInputFileBytes)
	}

	cfg, _, err := image.DecodeConfig(io.LimitReader(f, maxInputFileBytes))
	if err != nil {
		return fmt.Errorf("decode input image header: %w", err)
	}
	if err := validateDimensions("source", cfg.Width, cfg.Height); err != nil {
		return err
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind input image: %w", err)
	}

	src, _, err := image.Decode(io.LimitReader(f, maxInputFileBytes))
	if err != nil {
		return fmt.Errorf("decode input image: %w", err)
	}

	raw, shaHex, err := Convert(src, opts)
	if err != nil {
		return err
	}

	if err := atomicfile.Write(outPath, raw, 0600); err != nil {
		return fmt.Errorf("write output image: %w", err)
	}
	if err := atomicfile.Write(outPath+".sha", []byte(shaHex), 0600); err != nil {
		return fmt.Errorf("write output hash: %w", err)
	}
	return nil
}

// fitToGrayscale crops src from its center and resizes it with bilinear interpolation.
// It converts each pixel to 8-bit grayscale with ITU-R 601-2 weights.
func fitToGrayscale(src image.Image, targetW, targetH int) []uint8 {
	b := src.Bounds()
	srcW := b.Dx()
	srcH := b.Dy()

	// Exact dimensions need no crop or resize.
	if srcW == targetW && srcH == targetH {
		out := make([]uint8, targetW*targetH)
		for y := 0; y < targetH; y++ {
			for x := 0; x < targetW; x++ {
				out[y*targetW+x] = toLuma(src.At(b.Min.X+x, b.Min.Y+y))
			}
		}
		return out
	}

	// Calculate a centered crop box that matches the target aspect ratio.
	cropX := 0.0
	cropY := 0.0
	cropW := float64(srcW)
	cropH := float64(srcH)

	srcAspect := cropW / cropH
	dstAspect := float64(targetW) / float64(targetH)
	if srcAspect > dstAspect {
		// Trim the left and right edges when src is wider than the target.
		newW := cropH * dstAspect
		cropX = (cropW - newW) / 2.0
		cropW = newW
	} else if srcAspect < dstAspect {
		// Trim the top and bottom edges when src is taller than the target.
		newH := cropW / dstAspect
		cropY = (cropH - newH) / 2.0
		cropH = newH
	}

	out := make([]uint8, targetW*targetH)
	scaleX := cropW / float64(targetW)
	scaleY := cropH / float64(targetH)

	for y := 0; y < targetH; y++ {
		fy := cropY + (float64(y)+0.5)*scaleY - 0.5
		if fy < 0 {
			fy = 0
		}
		if fy > float64(srcH-1) {
			fy = float64(srcH - 1)
		}
		y0 := int(math.Floor(fy))
		y1 := y0 + 1
		if y1 >= srcH {
			y1 = srcH - 1
		}
		wy := fy - float64(y0)

		for x := 0; x < targetW; x++ {
			fx := cropX + (float64(x)+0.5)*scaleX - 0.5
			if fx < 0 {
				fx = 0
			}
			if fx > float64(srcW-1) {
				fx = float64(srcW - 1)
			}
			x0 := int(math.Floor(fx))
			x1 := x0 + 1
			if x1 >= srcW {
				x1 = srcW - 1
			}
			wx := fx - float64(x0)

			c00 := float64(toLuma(src.At(b.Min.X+x0, b.Min.Y+y0)))
			c10 := float64(toLuma(src.At(b.Min.X+x1, b.Min.Y+y0)))
			c01 := float64(toLuma(src.At(b.Min.X+x0, b.Min.Y+y1)))
			c11 := float64(toLuma(src.At(b.Min.X+x1, b.Min.Y+y1)))

			top := c00*(1-wx) + c10*wx
			bot := c01*(1-wx) + c11*wx
			val := top*(1-wy) + bot*wy
			out[y*targetW+x] = uint8(math.Round(val))
		}
	}
	return out
}

// toLuma composites c over white in 16-bit premultiplied space.
// It clamps RGB channels to alpha and applies ITU-R 601-2 weights.
func toLuma(c color.Color) uint8 {
	r16, g16, b16, a16 := c.RGBA()
	if r16 > a16 {
		r16 = a16
	}
	if g16 > a16 {
		g16 = a16
	}
	if b16 > a16 {
		b16 = a16
	}

	inv := 0xFFFF - a16
	r := uint64(r16 + inv)
	g := uint64(g16 + inv)
	b := uint64(b16 + inv)

	// Scale 16-bit luma directly to [0, 255] with single-step rounding (65535 = 255 * 257).
	luma := (299*r + 587*g + 114*b + 128500) / 257000
	if luma > 255 {
		return 255
	}
	return uint8(luma)
}
