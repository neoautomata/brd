package imgconv

import (
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

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

func TestToLumaITU601(t *testing.T) {
	tests := []struct {
		name string
		c    color.RGBA
		want uint8
	}{
		{"black", color.RGBA{0, 0, 0, 255}, 0},
		{"white", color.RGBA{255, 255, 255, 255}, 255},
		{"pure red", color.RGBA{255, 0, 0, 255}, 76},
		{"pure green", color.RGBA{0, 255, 0, 255}, 150},
		{"pure blue", color.RGBA{0, 0, 255, 255}, 29},
		{"fully transparent composites onto white", color.RGBA{0, 0, 0, 0}, 255},
		{"50% black over white", color.RGBA{0, 0, 0, 128}, 127},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := toLuma(tc.c); got != tc.want {
				t.Errorf("toLuma(%v) = %d, want %d", tc.c, got, tc.want)
			}
		})
	}
}

func TestConvertCenterCropAndResize(t *testing.T) {
	tests := []struct {
		name string
		srcW int
		srcH int
		dstW int
		dstH int
	}{
		{"wider aspect ratio cropped horizontally", 200, 100, 10, 20},
		{"taller aspect ratio cropped vertically", 100, 400, 10, 20},
		{"larger uniform image downscaled", 1080, 1920, 540, 960},
		{"smaller uniform image upscaled", 27, 48, 54, 96},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			img := image.NewRGBA(image.Rect(0, 0, tc.srcW, tc.srcH))
			for y := 0; y < tc.srcH; y++ {
				for x := 0; x < tc.srcW; x++ {
					img.SetRGBA(x, y, color.RGBA{255, 255, 255, 255})
				}
			}
			raw, shaHex, err := Convert(img, Options{Width: tc.dstW, Height: tc.dstH})
			if err != nil {
				t.Fatalf("Convert failed: %v", err)
			}
			wantBytes := ((tc.dstH + 1) / 2) * tc.dstW
			if len(raw) != wantBytes {
				t.Fatalf("len(raw) = %d, want %d", len(raw), wantBytes)
			}
			if len(shaHex) != 64 {
				t.Errorf("len(shaHex) = %d, want 64", len(shaHex))
			}
			for i, b := range raw {
				if b != 0xFF {
					t.Fatalf("raw[%d] = 0x%02x, want 0xff for solid white", i, b)
				}
			}
		})
	}
}

func TestConvertErrors(t *testing.T) {
	if _, _, err := Convert(nil, Options{}); err == nil {
		t.Error("expected error for nil image")
	}
	empty := image.NewGray(image.Rect(0, 0, 0, 0))
	if _, _, err := Convert(empty, Options{}); err == nil {
		t.Error("expected error for empty image")
	}
	valid := image.NewGray(image.Rect(0, 0, 4, 4))
	if _, _, err := Convert(valid, Options{Width: -1, Height: 10}); err == nil {
		t.Error("expected error for negative target width")
	}
}

func TestConvertFileAndErrorPaths(t *testing.T) {
	dir := t.TempDir()
	inPath := filepath.Join(dir, "in.png")
	outPath := filepath.Join(dir, "out.raw")

	img := image.NewGray(image.Rect(0, 0, 4, 6))
	img.SetGray(3, 0, color.Gray{Y: 0xF0})
	f, err := os.Create(inPath)
	if err != nil {
		t.Fatalf("create png: %v", err)
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		t.Fatalf("encode png: %v", err)
	}
	f.Close()

	if err := ConvertFile(inPath, outPath, Options{Width: 4, Height: 6}); err != nil {
		t.Fatalf("ConvertFile failed: %v", err)
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
