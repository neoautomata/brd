package imgconv

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func unpackNibble(raw []byte, dstW, dstX, dstY int) byte {
	rowBytes := (dstW + 1) / 2
	b := raw[dstY*rowBytes+dstX/2]
	if dstX%2 == 0 {
		return b & 0x0F
	}
	return b >> 4
}

func TestConvertRotationCornersAndPacking(t *testing.T) {
	// Portrait 540x960 image with distinct upper nibbles at each corner.
	img := image.NewGray(image.Rect(0, 0, DefaultWidth, DefaultHeight))
	img.SetGray(0, 0, color.Gray{Y: 0x10})                            // top-left -> dst (0, 539)
	img.SetGray(DefaultWidth-1, 0, color.Gray{Y: 0x20})               // top-right -> dst (0, 0)
	img.SetGray(0, DefaultHeight-1, color.Gray{Y: 0x30})              // bottom-left -> dst (959, 539)
	img.SetGray(DefaultWidth-1, DefaultHeight-1, color.Gray{Y: 0x40}) // bottom-right -> dst (959, 0)

	raw, shaHex, err := Convert(img, Options{})
	if err != nil {
		t.Fatalf("Convert failed: %v", err)
	}

	const wantLen = (DefaultHeight / 2) * DefaultWidth // 480 * 540 = 259200
	if len(raw) != wantLen {
		t.Fatalf("len(raw) = %d, want %d", len(raw), wantLen)
	}

	rowBytes := DefaultHeight / 2 // 480 bytes per rotated row
	tests := []struct {
		name     string
		offset   int
		wantByte byte
	}{
		{"top-right -> dst(0,0) low nibble", 0*rowBytes + 0, 0x02},
		{"bottom-right -> dst(959,0) high nibble", 0*rowBytes + (rowBytes - 1), 0x40},
		{"top-left -> dst(0,539) low nibble", (DefaultWidth-1)*rowBytes + 0, 0x01},
		{"bottom-left -> dst(959,539) high nibble", (DefaultWidth-1)*rowBytes + (rowBytes - 1), 0x30},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := raw[tc.offset]; got != tc.wantByte {
				t.Errorf("raw[%d] = 0x%02x, want 0x%02x", tc.offset, got, tc.wantByte)
			}
		})
	}

	wantSum := sha256.Sum256(raw)
	wantHex := hex.EncodeToString(wantSum[:])
	if shaHex != wantHex {
		t.Errorf("shaHex = %q, want %q", shaHex, wantHex)
	}
}

func TestToLumaITU601AndColorModels(t *testing.T) {
	tests := []struct {
		name string
		c    color.Color
		want uint8
	}{
		{"RGBA black", color.RGBA{0, 0, 0, 255}, 0},
		{"RGBA white", color.RGBA{255, 255, 255, 255}, 255},
		{"RGBA pure red", color.RGBA{255, 0, 0, 255}, 76},
		{"RGBA pure green", color.RGBA{0, 255, 0, 255}, 150},
		{"RGBA pure blue", color.RGBA{0, 0, 255, 255}, 29},
		{"RGBA transparent composites over white", color.RGBA{0, 0, 0, 0}, 255},
		{"RGBA 50% black over white", color.RGBA{0, 0, 0, 128}, 127},
		{"NRGBA 50% red over white", color.NRGBA{255, 0, 0, 128}, 165},
		{"NRGBA transparent with non-zero RGB composites over white", color.NRGBA{255, 128, 64, 0}, 255},
		{"Gray16 mid-gray", color.Gray16{Y: 0x8000}, 128},
		{"Gray16 white", color.Gray16{Y: 0xFFFF}, 255},
		{"YCbCr neutral gray", color.YCbCr{Y: 128, Cb: 128, Cr: 128}, 128},
		{"CMYK pure cyan", color.CMYK{C: 255, M: 0, Y: 0, K: 0}, 179},
		{"CMYK black", color.CMYK{C: 0, M: 0, Y: 0, K: 255}, 0},
		{"invalid premultiplied RGBA R>A clamps to alpha", color.RGBA{R: 255, G: 0, B: 0, A: 128}, 165},
		{"invalid premultiplied RGBA non-zero RGB with A=0 clamps to white", color.RGBA{R: 255, G: 255, B: 255, A: 0}, 255},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := toLuma(tc.c); got != tc.want {
				t.Errorf("toLuma(%T %v) = %d, want %d", tc.c, tc.c, got, tc.want)
			}
		})
	}

	// Verify Paletted image conversion uses palette entries accurately.
	pal := color.Palette{
		color.RGBA{0, 0, 0, 255},
		color.RGBA{255, 255, 255, 255},
	}
	palImg := image.NewPaletted(image.Rect(0, 0, 2, 2), pal)
	palImg.SetColorIndex(0, 0, 0)
	palImg.SetColorIndex(1, 0, 1) // top-right -> dst(0,0) = 0xF
	palImg.SetColorIndex(0, 1, 1)
	palImg.SetColorIndex(1, 1, 0) // bottom-right -> dst(1,0) = 0x0
	raw, _, err := Convert(palImg, Options{Width: 2, Height: 2})
	if err != nil {
		t.Fatalf("Convert paletted failed: %v", err)
	}
	if unpackNibble(raw, 2, 0, 0) != 0x0F || unpackNibble(raw, 2, 1, 0) != 0x00 {
		t.Errorf("unexpected paletted output: %x", raw)
	}
}

func TestConvertCenterCropAndResize(t *testing.T) {
	t.Run("horizontal center crop trims outer margins and preserves inner orientation", func(t *testing.T) {
		// Source 40x20, target 4x4 (square): centered crop keeps x in [10, 30).
		// Paint margins [0,10) and [30,40) black (0x00), inner left [10,20) dark (0x40),
		// inner right [20,30) bright (0xC0).
		img := image.NewGray(image.Rect(0, 0, 40, 20))
		for y := 0; y < 20; y++ {
			for x := 10; x < 20; x++ {
				img.SetGray(x, y, color.Gray{Y: 0x40})
			}
			for x := 20; x < 30; x++ {
				img.SetGray(x, y, color.Gray{Y: 0xC0})
			}
		}

		raw, _, err := Convert(img, Options{Width: 4, Height: 4})
		if err != nil {
			t.Fatalf("Convert failed: %v", err)
		}
		// After 90 deg CCW rotation:
		// portrait right half (srcX=2,3 -> 0xC0) maps to dstY=0,1.
		// portrait left half (srcX=0,1 -> 0x40) maps to dstY=2,3.
		if got := unpackNibble(raw, 4, 0, 0); got != 0x0C {
			t.Errorf("dst(0,0) = 0x%x, want 0xC (cropped inner right)", got)
		}
		if got := unpackNibble(raw, 4, 0, 3); got != 0x04 {
			t.Errorf("dst(0,3) = 0x%x, want 0x4 (cropped inner left)", got)
		}
	})

	t.Run("vertical center crop trims top and bottom margins", func(t *testing.T) {
		// Source 20x40, target 4x4 (square): centered crop keeps y in [10, 30).
		// Paint top [0,10) and bottom [30,40) black, inner top [10,20) 0x50, inner bottom [20,30) 0xD0.
		img := image.NewGray(image.Rect(0, 0, 20, 40))
		for x := 0; x < 20; x++ {
			for y := 10; y < 20; y++ {
				img.SetGray(x, y, color.Gray{Y: 0x50})
			}
			for y := 20; y < 30; y++ {
				img.SetGray(x, y, color.Gray{Y: 0xD0})
			}
		}

		raw, _, err := Convert(img, Options{Width: 4, Height: 4})
		if err != nil {
			t.Fatalf("Convert failed: %v", err)
		}
		// Portrait top (srcY=0 -> 0x50) maps to dstX=0.
		// Portrait bottom (srcY=3 -> 0xD0) maps to dstX=3.
		if got := unpackNibble(raw, 4, 0, 0); got != 0x05 {
			t.Errorf("dst(0,0) = 0x%x, want 0x5 (cropped inner top)", got)
		}
		if got := unpackNibble(raw, 4, 3, 0); got != 0x0D {
			t.Errorf("dst(3,0) = 0x%x, want 0xD (cropped inner bottom)", got)
		}
	})

	t.Run("upscale bilinear interpolation blends adjacent pixels", func(t *testing.T) {
		// 2x1 horizontal gradient: left=0x00, right=0xF0.
		// Upscale to 4x2: portrait x=0..3 samples at fx = -0.25 (clamped 0), 0.25, 0.75, 1.25 (clamped 1).
		// Expected portrait luma: 0, 60 (0x3C -> nibble 3), 180 (0xB4 -> nibble B), 240 (0xF0 -> nibble F).
		img := image.NewGray(image.Rect(0, 0, 2, 1))
		img.SetGray(0, 0, color.Gray{Y: 0x00})
		img.SetGray(1, 0, color.Gray{Y: 0xF0})

		raw, _, err := Convert(img, Options{Width: 4, Height: 2})
		if err != nil {
			t.Fatalf("Convert failed: %v", err)
		}
		// dstY = targetW - 1 - srcX: dstY=0 is srcX=3 (0xF), dstY=1 is srcX=2 (0xB), dstY=2 is srcX=1 (0x3), dstY=3 is srcX=0 (0x0).
		wantByDstY := []byte{0x0F, 0x0B, 0x03, 0x00}
		for dstY, want := range wantByDstY {
			if got := unpackNibble(raw, 2, 0, dstY); got != want {
				t.Errorf("dst(0,%d) = 0x%x, want 0x%x", dstY, got, want)
			}
		}
	})

	t.Run("downscale averages alternating stripes", func(t *testing.T) {
		// 4x4 checkerboard of 0x00 and 0xF0 downscaled to 2x2 averages to ~0x78 (nibble 7).
		img := image.NewGray(image.Rect(0, 0, 4, 4))
		for y := 0; y < 4; y++ {
			for x := 0; x < 4; x++ {
				if (x+y)%2 == 0 {
					img.SetGray(x, y, color.Gray{Y: 0xF0})
				}
			}
		}
		raw, shaHex, err := Convert(img, Options{Width: 2, Height: 2})
		if err != nil {
			t.Fatalf("Convert failed: %v", err)
		}
		if len(shaHex) != 64 {
			t.Errorf("len(shaHex) = %d, want 64", len(shaHex))
		}
		for dstY := 0; dstY < 2; dstY++ {
			for dstX := 0; dstX < 2; dstX++ {
				if got := unpackNibble(raw, 2, dstX, dstY); got != 0x07 {
					t.Errorf("dst(%d,%d) = 0x%x, want 0x7", dstX, dstY, got)
				}
			}
		}
	})
}

func TestConvertEdgeDimensionsAndNonZeroOrigin(t *testing.T) {
	t.Run("non-zero image rect origin exact and resized", func(t *testing.T) {
		img := image.NewGray(image.Rect(10, 20, 14, 26)) // 4x6 starting at (10,20)
		img.SetGray(13, 20, color.Gray{Y: 0xE0})         // top-right -> dst(0,0)

		rawExact, _, err := Convert(img, Options{Width: 4, Height: 6})
		if err != nil {
			t.Fatalf("Convert exact non-zero origin: %v", err)
		}
		if got := unpackNibble(rawExact, 6, 0, 0); got != 0x0E {
			t.Errorf("exact dst(0,0) = 0x%x, want 0xE", got)
		}

		// Fill solid 0xA0 and test resize path with non-zero origin.
		for y := 20; y < 26; y++ {
			for x := 10; x < 14; x++ {
				img.SetGray(x, y, color.Gray{Y: 0xA0})
			}
		}
		rawResized, _, err := Convert(img, Options{Width: 2, Height: 3})
		if err != nil {
			t.Fatalf("Convert resized non-zero origin: %v", err)
		}
		if got := unpackNibble(rawResized, 3, 2, 1); got != 0x0A {
			t.Errorf("resized dst(2,1) = 0x%x, want 0xA", got)
		}
	})

	t.Run("1x1 source exact and upscaled", func(t *testing.T) {
		img := image.NewGray(image.Rect(0, 0, 1, 1))
		img.SetGray(0, 0, color.Gray{Y: 0x90})

		raw1, _, err := Convert(img, Options{Width: 1, Height: 1})
		if err != nil {
			t.Fatalf("Convert 1x1->1x1: %v", err)
		}
		if len(raw1) != 1 || raw1[0] != 0x09 {
			t.Errorf("1x1 raw = %x, want [09]", raw1)
		}

		rawUp, _, err := Convert(img, Options{Width: 3, Height: 5})
		if err != nil {
			t.Fatalf("Convert 1x1->3x5: %v", err)
		}
		// dstW=5 -> rowBytes=3, dstH=3 -> total 9 bytes.
		if len(rawUp) != 9 {
			t.Fatalf("len(rawUp) = %d, want 9", len(rawUp))
		}
		// Trailing odd column (dstX=4) must have low nibble 0x9 and high nibble 0x0.
		if rawUp[2] != 0x09 {
			t.Errorf("odd row trailing byte = 0x%02x, want 0x09", rawUp[2])
		}
	})

	t.Run("odd dimensions pack trailing low nibble cleanly", func(t *testing.T) {
		img := image.NewGray(image.Rect(0, 0, 3, 5))
		img.SetGray(2, 4, color.Gray{Y: 0xB0}) // bottom-right -> dst(4,0)

		raw, _, err := Convert(img, Options{Width: 3, Height: 5})
		if err != nil {
			t.Fatalf("Convert 3x5: %v", err)
		}
		if len(raw) != 9 {
			t.Fatalf("len(raw) = %d, want 9", len(raw))
		}
		if raw[2] != 0x0B {
			t.Errorf("raw[2] = 0x%02x, want 0x0b", raw[2])
		}
	})
}

func TestConvertErrorsAndDimensionLimits(t *testing.T) {
	if _, _, err := Convert(nil, Options{}); err == nil {
		t.Error("expected error for nil image")
	}
	empty := image.NewGray(image.Rect(0, 0, 0, 0))
	if _, _, err := Convert(empty, Options{}); err == nil {
		t.Error("expected error for empty image")
	}
	valid := image.NewGray(image.Rect(0, 0, 4, 4))

	badOpts := []struct {
		name string
		opts Options
	}{
		{"negative width", Options{Width: -1, Height: 10}},
		{"negative height", Options{Width: 10, Height: -1}},
		{"width exceeds max dimension", Options{Width: maxImageDimension + 1, Height: 10}},
		{"height exceeds max dimension", Options{Width: 10, Height: maxImageDimension + 1}},
		{"pixel count exceeds limit", Options{Width: 10000, Height: 10000}},
	}
	for _, tc := range badOpts {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := Convert(valid, tc.opts); err == nil {
				t.Errorf("expected error for %+v", tc.opts)
			}
		})
	}
}

func makeBombPNGHeader(width, height uint32) []byte {
	var buf bytes.Buffer
	buf.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})

	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], width)
	binary.BigEndian.PutUint32(ihdr[4:8], height)
	ihdr[8] = 8 // bit depth
	ihdr[9] = 2 // RGB

	var lenBytes [4]byte
	binary.BigEndian.PutUint32(lenBytes[:], uint32(len(ihdr)))
	buf.Write(lenBytes[:])
	buf.WriteString("IHDR")
	buf.Write(ihdr)

	crc := crc32.NewIEEE()
	_, _ = crc.Write([]byte("IHDR"))
	_, _ = crc.Write(ihdr)
	var crcBytes [4]byte
	binary.BigEndian.PutUint32(crcBytes[:], crc.Sum32())
	buf.Write(crcBytes[:])
	return buf.Bytes()
}

func TestConvertFileRejectsDecompressionBombPNG(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "out.raw")

	cases := []struct {
		name string
		w, h uint32
	}{
		{"exceeds max dimension", 20000, 100},
		{"exceeds max pixels", 10000, 10000},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bombPath := filepath.Join(dir, tc.name+".png")
			if err := os.WriteFile(bombPath, makeBombPNGHeader(tc.w, tc.h), 0600); err != nil {
				t.Fatalf("write bomb png: %v", err)
			}
			err := ConvertFile(bombPath, outPath, Options{Width: 4, Height: 6})
			if err == nil {
				t.Fatalf("expected ConvertFile to reject %dx%d PNG", tc.w, tc.h)
			}
			if !strings.Contains(err.Error(), "exceed") {
				t.Errorf("unexpected error = %v, want dimension/pixel limit error", err)
			}
		})
	}
}

func TestConvertFileFormatsAndErrorPaths(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "out.raw")

	img := image.NewGray(image.Rect(0, 0, 4, 6))
	img.SetGray(3, 0, color.Gray{Y: 0xF0})

	// PNG
	pngPath := filepath.Join(dir, "in.png")
	f, err := os.Create(pngPath)
	if err != nil {
		t.Fatalf("create png: %v", err)
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		t.Fatalf("encode png: %v", err)
	}
	f.Close()

	if err := ConvertFile(pngPath, outPath, Options{Width: 4, Height: 6}); err != nil {
		t.Fatalf("ConvertFile PNG failed: %v", err)
	}

	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read outPath: %v", err)
	}
	if len(raw) != 12 {
		t.Fatalf("len(raw) = %d, want 12", len(raw))
	}
	if raw[0] != 0x0F {
		t.Errorf("raw[0] = 0x%02x, want 0x0f", raw[0])
	}

	shaBytes, err := os.ReadFile(outPath + ".sha")
	if err != nil {
		t.Fatalf("read sha file: %v", err)
	}
	wantSum := sha256.Sum256(raw)
	if string(shaBytes) != hex.EncodeToString(wantSum[:]) {
		t.Errorf("sha = %q, want %q", string(shaBytes), hex.EncodeToString(wantSum[:]))
	}

	// JPEG
	jpgPath := filepath.Join(dir, "in.jpg")
	jf, err := os.Create(jpgPath)
	if err != nil {
		t.Fatalf("create jpg: %v", err)
	}
	if err := jpeg.Encode(jf, img, &jpeg.Options{Quality: 95}); err != nil {
		jf.Close()
		t.Fatalf("encode jpg: %v", err)
	}
	jf.Close()

	jpgOut := filepath.Join(dir, "out_jpg.raw")
	if err := ConvertFile(jpgPath, jpgOut, Options{Width: 4, Height: 6}); err != nil {
		t.Fatalf("ConvertFile JPEG failed: %v", err)
	}
	if got, err := os.ReadFile(jpgOut); err != nil || len(got) != 12 {
		t.Errorf("JPEG output len=%d err=%v, want 12 nil", len(got), err)
	}

	// GIF
	gifPath := filepath.Join(dir, "in.gif")
	gf, err := os.Create(gifPath)
	if err != nil {
		t.Fatalf("create gif: %v", err)
	}
	if err := gif.Encode(gf, img, nil); err != nil {
		gf.Close()
		t.Fatalf("encode gif: %v", err)
	}
	gf.Close()

	gifOut := filepath.Join(dir, "out_gif.raw")
	if err := ConvertFile(gifPath, gifOut, Options{Width: 4, Height: 6}); err != nil {
		t.Fatalf("ConvertFile GIF failed: %v", err)
	}
	if got, err := os.ReadFile(gifOut); err != nil || len(got) != 12 {
		t.Errorf("GIF output len=%d err=%v, want 12 nil", len(got), err)
	}

	// Invalid options fail before opening file
	if err := ConvertFile(pngPath, outPath, Options{Width: -5, Height: 6}); err == nil {
		t.Error("expected error for invalid target options")
	}

	// Missing input file
	if err := ConvertFile(filepath.Join(dir, "missing.png"), outPath, Options{}); err == nil {
		t.Error("expected error for missing input file")
	}

	// Corrupt PNG file
	corruptPath := filepath.Join(dir, "corrupt.png")
	if err := os.WriteFile(corruptPath, []byte("not-a-png"), 0600); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}
	if err := ConvertFile(corruptPath, outPath, Options{}); err == nil {
		t.Error("expected error for corrupt PNG")
	}
}
