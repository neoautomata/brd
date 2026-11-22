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
	"math"
	"os"

	"github.com/neoautomata/brd/internal/atomicfile"
)

const (
	// DefaultWidth is the portrait pixel width of the LilyGO T5 4.7-inch display.
	DefaultWidth = 540
	// DefaultHeight is the portrait pixel height of the LilyGO T5 4.7-inch display.
	DefaultHeight = 960
)

// Options configures target portrait dimensions before 90-degree CCW rotation.
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
	if w < 0 || h < 0 {
		return 0, 0, fmt.Errorf("invalid target dimensions %dx%d", w, h)
	}
	return w, h, nil
}

// Convert fits src to opts.Width x opts.Height, converts it to ITU-R 601-2 grayscale,
// rotates it 90 degrees counter-clockwise, and packs pixels into 4bpp little-endian nibbles.
// It returns the raw framebuffer bytes and the 64-character lowercase SHA-256 hex digest.
func Convert(src image.Image, opts Options) ([]byte, string, error) {
	if src == nil {
		return nil, "", errors.New("source image is nil")
	}
	bounds := src.Bounds()
	srcW := bounds.Dx()
	srcH := bounds.Dy()
	if srcW <= 0 || srcH <= 0 {
		return nil, "", fmt.Errorf("source image has empty dimensions %dx%d", srcW, srcH)
	}

	targetW, targetH, err := opts.normalized()
	if err != nil {
		return nil, "", err
	}

	// Build an 8-bit grayscale portrait buffer of size targetW x targetH.
	gray := fitToGrayscale(src, targetW, targetH)

	// Rotate 90 degrees counter-clockwise:
	// Destination dimensions are dstW = targetH, dstH = targetW.
	// Source mapping: srcX = targetW - 1 - dstY, srcY = dstX.
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

// ConvertFile decodes an image from inPath, converts it to 4bpp format, and writes
// both outPath and outPath+".sha" atomically with 0600 permissions.
func ConvertFile(inPath, outPath string, opts Options) error {
	f, err := os.Open(inPath)
	if err != nil {
		return fmt.Errorf("open input image: %w", err)
	}
	defer f.Close()

	src, _, err := image.Decode(f)
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

// fitToGrayscale performs center-crop and bilinear resize (matching ImageOps.fit)
// and converts each pixel to 8-bit grayscale using ITU-R 601-2 luma weights.
func fitToGrayscale(src image.Image, targetW, targetH int) []uint8 {
	b := src.Bounds()
	srcW := b.Dx()
	srcH := b.Dy()

	// Fast path: exact dimensions require no cropping or resampling.
	if srcW == targetW && srcH == targetH {
		out := make([]uint8, targetW*targetH)
		for y := 0; y < targetH; y++ {
			for x := 0; x < targetW; x++ {
				out[y*targetW+x] = toLuma(src.At(b.Min.X+x, b.Min.Y+y))
			}
		}
		return out
	}

	// Compute centered crop box matching target aspect ratio.
	cropX := 0.0
	cropY := 0.0
	cropW := float64(srcW)
	cropH := float64(srcH)

	srcAspect := cropW / cropH
	dstAspect := float64(targetW) / float64(targetH)
	if srcAspect > dstAspect {
		// Source is wider than target: trim left and right edges.
		newW := cropH * dstAspect
		cropX = (cropW - newW) / 2.0
		cropW = newW
	} else if srcAspect < dstAspect {
		// Source is taller than target: trim top and bottom edges.
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

// toLuma applies ITU-R 601-2 luma weights to convert an RGB color to 8-bit grayscale.
func toLuma(c color.Color) uint8 {
	n := color.NRGBAModel.Convert(c).(color.NRGBA)
	luma := (299*int(n.R) + 587*int(n.G) + 114*int(n.B) + 500) / 1000
	if luma > 255 {
		return 255
	}
	return uint8(luma)
}
