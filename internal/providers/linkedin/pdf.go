package linkedin

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif" // decoders for the conversion to JPEG
	"image/jpeg"
	_ "image/png"
	"io"

	"github.com/Seeyko/relay/internal/providers"
)

// Document carousel: LinkedIn has no swipeable image carousel in its API, but a PDF document post is
// shown as one. Relay builds the PDF itself: one page per image, page size = image size (1 px = 1 pt),
// each page drawing one JPEG embedded as is (DCTDecode). Other formats are re-encoded to JPEG first.

const (
	minDocumentImages = 2
	maxDocumentImages = 300 // LinkedIn documents: 300 pages
	// pdfMaxSide is the largest page side most readers accept (Acrobat: 200 inches = 14400 pt).
	pdfMaxSide = 14400
)

var errTooBig = errors.New("the PDF would be larger than LinkedIn's 100 MB document limit")

// pdfImage is one JPEG ready to embed.
type pdfImage struct {
	data          []byte
	width, height int
	colorSpace    string
}

// jpegFor returns the file as an embeddable JPEG: baseline or progressive RGB/gray JPEGs pass through,
// anything else (PNG, GIF, CMYK JPEG) is decoded and re-encoded.
func jpegFor(b []byte) (pdfImage, error) {
	if cfg, err := jpeg.DecodeConfig(bytes.NewReader(b)); err == nil {
		switch cfg.ColorModel {
		case color.YCbCrModel:
			return pdfImage{b, cfg.Width, cfg.Height, "/DeviceRGB"}, nil
		case color.GrayModel:
			return pdfImage{b, cfg.Width, cfg.Height, "/DeviceGray"}, nil
		}
	}
	src, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return pdfImage{}, err
	}
	// Flatten transparency on white, like a viewer would show it.
	rgb := image.NewRGBA(src.Bounds())
	draw.Draw(rgb, rgb.Bounds(), image.White, image.Point{}, draw.Src)
	draw.Draw(rgb, rgb.Bounds(), src, src.Bounds().Min, draw.Over)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, rgb, &jpeg.Options{Quality: 90}); err != nil {
		return pdfImage{}, err
	}
	return pdfImage{out.Bytes(), rgb.Bounds().Dx(), rgb.Bounds().Dy(), "/DeviceRGB"}, nil
}

// limitedBuffer fails once more than max bytes are written.
type limitedBuffer struct {
	bytes.Buffer
	max int64
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if int64(l.Len()+len(p)) > l.max {
		return 0, errTooBig
	}
	return l.Buffer.Write(p)
}

// buildPDF writes a PDF with one page per image. It reads the images one by one; the PDF is kept in
// memory and refused past limit bytes.
func buildPDF(images []providers.MediaFile, limit int64) ([]byte, error) {
	out := &limitedBuffer{max: limit}
	var offsets []int // byte offset of object n+1
	obj := func(body string, stream []byte) error {
		offsets = append(offsets, out.Len())
		if _, err := fmt.Fprintf(out, "%d 0 obj\n%s\n", len(offsets), body); err != nil {
			return err
		}
		if stream != nil {
			if _, err := fmt.Fprintf(out, "stream\n"); err != nil {
				return err
			}
			if _, err := out.Write(stream); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(out, "\nendstream\n"); err != nil {
				return err
			}
		}
		_, err := fmt.Fprintf(out, "endobj\n")
		return err
	}
	if _, err := out.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n"); err != nil {
		return nil, err
	}
	// Objects: 1 catalog, 2 page tree, then per page k (from 0): page 3+3k, content 4+3k, image 5+3k.
	kids := make([]byte, 0, len(images)*8)
	for k := range images {
		kids = fmt.Appendf(kids, "%d 0 R ", 3+3*k)
	}
	if err := obj("<< /Type /Catalog /Pages 2 0 R >>", nil); err != nil {
		return nil, err
	}
	if err := obj(fmt.Sprintf("<< /Type /Pages /Kids [ %s] /Count %d >>", kids, len(images)), nil); err != nil {
		return nil, err
	}
	for k, m := range images {
		raw, err := readAll(m)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", m.Filename, err)
		}
		img, err := jpegFor(raw)
		if err != nil {
			return nil, providers.Invalid(fmt.Sprintf("linkedin-page: %s is not a readable image: %v", m.Filename, err))
		}
		w, h := float64(img.width), float64(img.height)
		if s := float64(pdfMaxSide) / max(w, h); s < 1 {
			w, h = w*s, h*s
		}
		content := fmt.Sprintf("q %.2f 0 0 %.2f 0 0 cm /Im0 Do Q", w, h)
		if err := obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %.2f %.2f] /Resources << /XObject << /Im0 %d 0 R >> >> /Contents %d 0 R >>",
			w, h, 5+3*k, 4+3*k), nil); err != nil {
			return nil, err
		}
		if err := obj(fmt.Sprintf("<< /Length %d >>", len(content)), []byte(content)); err != nil {
			return nil, err
		}
		if err := obj(fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace %s /BitsPerComponent 8 /Filter /DCTDecode /Length %d >>",
			img.width, img.height, img.colorSpace, len(img.data)), img.data); err != nil {
			return nil, err
		}
	}
	xref := out.Len()
	if _, err := fmt.Fprintf(out, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1); err != nil {
		return nil, err
	}
	for _, o := range offsets {
		if _, err := fmt.Fprintf(out, "%010d 00000 n \n", o); err != nil {
			return nil, err
		}
	}
	if _, err := fmt.Fprintf(out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xref); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func readAll(m providers.MediaFile) ([]byte, error) {
	f, err := m.Open()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// documentFromImages turns the images of the post into one PDF media file.
func documentFromImages(images []providers.MediaFile) (providers.MediaFile, error) {
	if len(images) < minDocumentImages || len(images) > maxDocumentImages {
		return providers.MediaFile{}, providers.Invalid(fmt.Sprintf("linkedin-page: images_as_document needs %d to %d images (got %d)", minDocumentImages, maxDocumentImages, len(images)))
	}
	for _, m := range images {
		if kindOf(m) != kindImage {
			return providers.MediaFile{}, providers.Invalid(fmt.Sprintf("linkedin-page: images_as_document takes images only (%s is %s)", m.Filename, m.Mime))
		}
	}
	pdf, err := buildPDF(images, maxDocument)
	if errors.Is(err, errTooBig) {
		return providers.MediaFile{}, providers.Invalid("linkedin-page: " + err.Error())
	}
	if err != nil {
		return providers.MediaFile{}, err
	}
	return providers.MediaFile{Filename: "carousel.pdf", Mime: "application/pdf", Size: int64(len(pdf)),
		Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(pdf)), nil }}, nil
}
