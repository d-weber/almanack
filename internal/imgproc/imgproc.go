// Package imgproc turns an uploaded avatar into a small square JPEG.
//
// This is the only thing standing between a phone camera — or an attacker — and the
// database, so the order of operations here is deliberate: size, then format, then
// declared dimensions, and only then pixels. Decoding is the expensive step, and a
// 200-byte PNG is allowed to claim 20000x20000, so the header is checked before the
// decoder is ever given the chance to allocate that.
//
// It uses no image library: stdlib decodes and encodes, and the scaler below is the
// forty lines the standard library does not ship.
package imgproc

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/png" // avatars from Android and from screenshots are usually PNG

	"almanack/internal/domain"
)

const (
	// MaxUploadBytes bounds the request body. The client already resizes with a
	// canvas before uploading, so anything near this is either an old client or
	// someone poking at the endpoint.
	MaxUploadBytes = 1 << 20 // 1 MiB
	// OutputSize is the stored avatar's side in pixels. Retina-sized for the 64 px
	// the UI draws, and small enough that avatars stay a rounding error in backups.
	OutputSize = 128
	// MaxDimension caps the declared width and height. 4096 covers any phone camera
	// after the client-side resize; beyond it, decoding would allocate more memory
	// than a family server should ever hand to an unauthenticated-shaped input.
	MaxDimension = 4096
	// jpegQuality is a visible-quality/size compromise measured on faces at 128 px.
	jpegQuality = 85
)

// Errors returned by Process. All wrap domain.ErrInvalid, so the HTTP layer maps them
// to 400 through the same path as any other bad request — a rejected avatar is a
// client mistake, never a server fault.
var (
	ErrTooLarge          = fmt.Errorf("image is larger than %d bytes: %w", MaxUploadBytes, domain.ErrInvalid)
	ErrUnsupportedFormat = fmt.Errorf("image must be JPEG or PNG: %w", domain.ErrInvalid)
	ErrDimensions        = fmt.Errorf("image is larger than %dx%d pixels: %w", MaxDimension, MaxDimension, domain.ErrInvalid)
)

// Process validates data and returns a square OutputSize x OutputSize JPEG: the image
// is centre-cropped to a square, box-averaged down, and re-encoded. Re-encoding is the
// point — the stored bytes are ones this package produced, so no EXIF, no colour
// profile, no trailing payload, and nothing the original file smuggled in survives.
func Process(data []byte) ([]byte, error) {
	if len(data) > MaxUploadBytes {
		return nil, ErrTooLarge
	}
	if !sniff(data) {
		return nil, ErrUnsupportedFormat
	}

	// DecodeConfig reads only the header, so this costs a few hundred bytes even for
	// an image claiming to be enormous.
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("read image header: %w: %w", err, ErrUnsupportedFormat)
	}
	// The sniff above already agreed, but image.DecodeConfig consults every decoder
	// registered anywhere in the binary; this keeps the guarantee local rather than
	// dependent on which packages the rest of the program happens to import.
	if format != "jpeg" && format != "png" {
		return nil, ErrUnsupportedFormat
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, ErrDimensions
	}
	if cfg.Width > MaxDimension || cfg.Height > MaxDimension {
		return nil, ErrDimensions
	}

	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode image: %w: %w", err, ErrUnsupportedFormat)
	}
	square := centreSquare(src)
	if square.Bounds().Dx() == 0 {
		return nil, ErrDimensions
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, boxScale(square, OutputSize), &jpeg.Options{Quality: jpegQuality}); err != nil {
		return nil, fmt.Errorf("encode avatar: %w", err)
	}
	return buf.Bytes(), nil
}

// sniff reports whether data starts with a JPEG or PNG signature. Content type and
// filename come from the client and are worth nothing; the first bytes are the only
// claim about a file that the file itself makes.
func sniff(data []byte) bool {
	switch {
	case bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}):
		return true
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return true
	}
	return false
}

// centreSquare crops src to the largest centred square and flattens it onto white.
// Flattening happens here because JPEG has no alpha: a transparent PNG composited
// against the zero value would come back as a black avatar.
func centreSquare(src image.Image) *image.RGBA {
	b := src.Bounds()
	side := min(b.Dx(), b.Dy())
	origin := image.Pt(b.Min.X+(b.Dx()-side)/2, b.Min.Y+(b.Dy()-side)/2)

	dst := image.NewRGBA(image.Rect(0, 0, side, side))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(dst, dst.Bounds(), src, origin, draw.Over)
	return dst
}

// boxScale resizes a square RGBA image to size x size by averaging each destination
// pixel over the source pixels it covers. A box filter is the right trade here: it is
// a dozen lines, it has no ringing, and unlike nearest-neighbour it does not turn a
// downscaled face into aliased noise. Enlarging a source smaller than size degenerates
// to nearest-neighbour, which is what anyone uploading a 40 px avatar deserves.
func boxScale(src *image.RGBA, size int) *image.RGBA {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	for dy := range size {
		y0 := dy * sh / size
		y1 := max((dy+1)*sh/size, y0+1)
		for dx := range size {
			x0 := dx * sw / size
			x1 := max((dx+1)*sw/size, x0+1)
			var r, g, bl, n uint32
			for y := y0; y < y1; y++ {
				i := src.PixOffset(b.Min.X+x0, b.Min.Y+y)
				for x := x0; x < x1; x++ {
					r += uint32(src.Pix[i])
					g += uint32(src.Pix[i+1])
					bl += uint32(src.Pix[i+2])
					n++
					i += 4
				}
			}
			o := dst.PixOffset(dx, dy)
			dst.Pix[o] = avg(r, n)
			dst.Pix[o+1] = avg(g, n)
			dst.Pix[o+2] = avg(bl, n)
			dst.Pix[o+3] = 0xFF // centreSquare already flattened the alpha
		}
	}
	return dst
}

func avg(sum, n uint32) uint8 { return uint8((sum + n/2) / n) }

// ContentType is the media type of what Process produces, for the handler that serves
// the stored blob back.
const ContentType = "image/jpeg"
