package imgproc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"

	"agenda/internal/domain"
)

// bands paints the centre third-ish of the width red and the sides blue, so that a
// correct centre crop of a wide image contains no blue at all.
func bands(w, h int, cropStart, cropEnd int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			c := color.RGBA{R: 220, G: 20, B: 20, A: 255}
			if x < cropStart || x >= cropEnd {
				c = color.RGBA{R: 20, G: 20, B: 220, A: 255}
			}
			img.Set(x, y, c)
		}
	}
	return img
}

func solid(w, h int, c color.Color) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, c)
		}
	}
	return img
}

func encodeJPEG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return buf.Bytes()
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return buf.Bytes()
}

// decodeOutput asserts that out is a JPEG of exactly OutputSize x OutputSize.
func decodeOutput(t *testing.T, out []byte) image.Image {
	t.Helper()
	img, format, err := image.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("output does not decode: %v", err)
	}
	if format != "jpeg" {
		t.Fatalf("output format = %q, want jpeg", format)
	}
	b := img.Bounds()
	if b.Dx() != OutputSize || b.Dy() != OutputSize {
		t.Fatalf("output is %dx%d, want %dx%d", b.Dx(), b.Dy(), OutputSize, OutputSize)
	}
	return img
}

func TestProcessShapes(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"wide jpeg", encodeJPEG(t, bands(400, 200, 100, 300))},
		{"tall png", encodePNG(t, solid(200, 400, color.RGBA{R: 10, G: 200, B: 10, A: 255}))},
		{"square png", encodePNG(t, solid(300, 300, color.RGBA{R: 10, G: 10, B: 200, A: 255}))},
		{"already the output size", encodePNG(t, solid(OutputSize, OutputSize, color.White))},
		{"smaller than the output size", encodePNG(t, solid(40, 60, color.RGBA{R: 200, G: 200, B: 10, A: 255}))},
		{"one pixel", encodePNG(t, solid(1, 1, color.RGBA{R: 200, G: 10, B: 200, A: 255}))},
		{"at the dimension cap", encodePNG(t, solid(MaxDimension, 8, color.White))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := Process(tc.data)
			if err != nil {
				t.Fatalf("Process: %v", err)
			}
			decodeOutput(t, out)
			if len(out) > MaxUploadBytes {
				t.Errorf("output is %d bytes, larger than the input cap", len(out))
			}
		})
	}
}

func near(t *testing.T, got color.Color, want color.RGBA, tolerance int, what string) {
	t.Helper()
	r, g, b, _ := got.RGBA()
	gr, gg, gb := int(r>>8), int(g>>8), int(b>>8)
	if abs(gr-int(want.R)) > tolerance || abs(gg-int(want.G)) > tolerance || abs(gb-int(want.B)) > tolerance {
		t.Errorf("%s: got rgb(%d,%d,%d), want ~rgb(%d,%d,%d)", what, gr, gg, gb, want.R, want.G, want.B)
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// TestCentreCrop is the one that catches a crop anchored at the origin: the sides of a
// wide image are blue and must not appear anywhere in the output.
func TestCentreCrop(t *testing.T) {
	red := color.RGBA{R: 220, G: 20, B: 20, A: 255}
	t.Run("wide", func(t *testing.T) {
		out, err := Process(encodeJPEG(t, bands(400, 200, 100, 300)))
		if err != nil {
			t.Fatal(err)
		}
		img := decodeOutput(t, out)
		for _, p := range []image.Point{{X: 2, Y: 2}, {X: 64, Y: 64}, {X: 125, Y: 125}, {X: 2, Y: 125}, {X: 125, Y: 2}} {
			near(t, img.At(p.X, p.Y), red, 24, "wide crop at "+p.String())
		}
	})
	t.Run("tall", func(t *testing.T) {
		// The same test rotated: bands across the height instead of the width.
		src := image.NewRGBA(image.Rect(0, 0, 200, 400))
		for y := range 400 {
			c := color.RGBA{R: 20, G: 20, B: 220, A: 255}
			if y >= 100 && y < 300 {
				c = red
			}
			for x := range 200 {
				src.Set(x, y, c)
			}
		}
		out, err := Process(encodePNG(t, src))
		if err != nil {
			t.Fatal(err)
		}
		img := decodeOutput(t, out)
		for _, p := range []image.Point{{X: 2, Y: 2}, {X: 64, Y: 64}, {X: 125, Y: 125}} {
			near(t, img.At(p.X, p.Y), red, 24, "tall crop at "+p.String())
		}
	})
}

// TestTransparentPNGFlattensToWhite: JPEG has no alpha, and the naive path turns a
// transparent avatar black.
func TestTransparentPNGFlattensToWhite(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 200, 200)) // zero value: fully transparent
	out, err := Process(encodePNG(t, src))
	if err != nil {
		t.Fatal(err)
	}
	img := decodeOutput(t, out)
	near(t, img.At(64, 64), color.RGBA{R: 255, G: 255, B: 255}, 8, "transparent centre")
}

// TestDownscaleAverages checks the filter actually averages rather than sampling: a
// one-pixel checkerboard of black and white must come back mid-grey, not one or the
// other.
func TestDownscaleAverages(t *testing.T) {
	const n = 512
	src := image.NewRGBA(image.Rect(0, 0, n, n))
	for y := range n {
		for x := range n {
			c := color.RGBA{A: 255}
			if (x+y)%2 == 0 {
				c = color.RGBA{R: 255, G: 255, B: 255, A: 255}
			}
			src.Set(x, y, c)
		}
	}
	out, err := Process(encodePNG(t, src))
	if err != nil {
		t.Fatal(err)
	}
	img := decodeOutput(t, out)
	near(t, img.At(64, 64), color.RGBA{R: 128, G: 128, B: 128}, 12, "checkerboard average")
}

func TestRejects(t *testing.T) {
	var gifBuf bytes.Buffer
	if err := gif.Encode(&gifBuf, solid(200, 200, color.RGBA{R: 1, G: 2, B: 3, A: 255}), nil); err != nil {
		t.Fatalf("encode gif fixture: %v", err)
	}
	valid := encodePNG(t, solid(64, 64, color.White))

	tests := []struct {
		name string
		data []byte
		want error
	}{
		{"empty", nil, ErrUnsupportedFormat},
		{"gif", gifBuf.Bytes(), ErrUnsupportedFormat},
		{"svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"></svg>`), ErrUnsupportedFormat},
		{"svg with an xml prolog", []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"/>`), ErrUnsupportedFormat},
		{"html", []byte("<!doctype html><html></html>"), ErrUnsupportedFormat},
		{"webp", append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), make([]byte, 32)...), ErrUnsupportedFormat},
		{"jpeg signature with no image behind it", append([]byte{0xFF, 0xD8, 0xFF}, make([]byte, 64)...), ErrUnsupportedFormat},
		{"png signature with no image behind it", append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...), ErrUnsupportedFormat},
		{"truncated png", valid[:len(valid)-20], ErrUnsupportedFormat},
		{"one byte over the cap", append(bytes.Repeat([]byte{0xFF}, MaxUploadBytes), 0x00), ErrTooLarge},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := Process(tc.data)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if !errors.Is(err, domain.ErrInvalid) {
				t.Errorf("err does not wrap domain.ErrInvalid, so the HTTP layer would return 500: %v", err)
			}
			if out != nil {
				t.Errorf("got %d bytes back alongside the error", len(out))
			}
		})
	}
}

// TestTooLargeIsCheckedFirst: the size check must not depend on the content being
// parseable, and must reject before anything is decoded.
func TestTooLargeIsCheckedFirst(t *testing.T) {
	big := encodePNG(t, solid(2000, 2000, color.RGBA{R: 5, G: 5, B: 5, A: 255}))
	for len(big) <= MaxUploadBytes {
		// A flat PNG compresses too well to exceed the cap; pad with incompressible
		// noise until it does. The bytes after IEND are ignored by any decoder,
		// which is exactly the point: only the length matters here.
		pad := make([]byte, MaxUploadBytes)
		for i := range pad {
			pad[i] = byte(i*i*31 + i)
		}
		big = append(big, pad...)
	}
	if _, err := Process(big); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if _, err := Process(big[:MaxUploadBytes]); errors.Is(err, ErrTooLarge) {
		t.Error("exactly MaxUploadBytes was rejected as too large; the cap is inclusive")
	}
}

// pngHeader builds a valid PNG signature plus a CRC-correct IHDR chunk declaring
// w x h, and nothing else. image.DecodeConfig reads it happily; image.Decode cannot,
// because there is no image data — which is what makes it a probe for whether Process
// rejects on the header or only after decoding.
func pngHeader(w, h uint32) []byte {
	var ihdr bytes.Buffer
	ihdr.WriteString("IHDR")
	binary.Write(&ihdr, binary.BigEndian, w)
	binary.Write(&ihdr, binary.BigEndian, h)
	ihdr.Write([]byte{8, 2, 0, 0, 0}) // 8-bit truecolour, no interlace

	out := bytes.NewBuffer([]byte("\x89PNG\r\n\x1a\n"))
	binary.Write(out, binary.BigEndian, uint32(ihdr.Len()-4)) // length excludes the type
	out.Write(ihdr.Bytes())
	binary.Write(out, binary.BigEndian, crc32.ChecksumIEEE(ihdr.Bytes()))
	return out.Bytes()
}

// TestOversizedDeclaredDimensions is the decompression-bomb case: a few hundred bytes
// claiming 20000x20000 must cost a header read, not 1.6 GB of pixels.
func TestOversizedDeclaredDimensions(t *testing.T) {
	tests := []struct {
		name string
		w, h uint32
	}{
		{"both", 20000, 20000},
		{"width only", MaxDimension + 1, 8},
		{"height only", 8, MaxDimension + 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data := pngHeader(tc.w, tc.h)
			if len(data) > 256 {
				t.Fatalf("fixture grew to %d bytes; it is meant to be a bare header", len(data))
			}

			// The fixture must be genuinely undecodable: if Process still rejects it
			// with ErrDimensions, the rejection can only have come from DecodeConfig.
			if _, _, err := image.Decode(bytes.NewReader(data)); err == nil {
				t.Fatal("fixture decoded; it can no longer prove the check precedes decoding")
			}
			cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("fixture header is not readable, so the test proves nothing: %v", err)
			}
			if cfg.Width != int(tc.w) || cfg.Height != int(tc.h) {
				t.Fatalf("fixture declares %dx%d, want %dx%d", cfg.Width, cfg.Height, tc.w, tc.h)
			}

			_, err = Process(data)
			if !errors.Is(err, ErrDimensions) {
				t.Fatalf("err = %v, want ErrDimensions", err)
			}
			if !errors.Is(err, domain.ErrInvalid) {
				t.Errorf("err does not wrap domain.ErrInvalid: %v", err)
			}
		})
	}
}

// TestOversizedRealImage covers the same cap with an image that really is that big:
// long and thin, so the fixture stays small enough to generate in a test.
func TestOversizedRealImage(t *testing.T) {
	data := encodePNG(t, solid(MaxDimension+1, 4, color.RGBA{R: 7, G: 7, B: 7, A: 255}))
	if len(data) > MaxUploadBytes {
		t.Fatalf("fixture is %d bytes, over the upload cap: it would fail for the wrong reason", len(data))
	}
	if _, err := Process(data); !errors.Is(err, ErrDimensions) {
		t.Fatalf("err = %v, want ErrDimensions", err)
	}
}

// TestOutputIsReprocessable: the pipeline is idempotent enough to run on its own
// output, which is what happens when an avatar is re-uploaded from a synced device.
func TestOutputIsReprocessable(t *testing.T) {
	first, err := Process(encodeJPEG(t, bands(400, 200, 100, 300)))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Process(first)
	if err != nil {
		t.Fatalf("re-processing a processed avatar: %v", err)
	}
	decodeOutput(t, second)
}
