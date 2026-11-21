package linkedin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/Seeyko/relay/internal/providers"
)

func jpegBytes(t *testing.T, w, h int, gray bool) []byte {
	t.Helper()
	var img image.Image
	if gray {
		g := image.NewGray(image.Rect(0, 0, w, h))
		for i := range g.Pix {
			g.Pix[i] = uint8(i)
		}
		img = g
	} else {
		c := image.NewRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				c.Set(x, y, color.RGBA{uint8(x), uint8(y), 99, 255})
			}
		}
		img = c
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 128 // half transparent grey
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func memFile(name, mime string, data []byte) providers.MediaFile {
	return providers.MediaFile{Filename: name, Mime: mime, Size: int64(len(data)), Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }}
}

type pdfPage struct {
	box           [2]float64 // MediaBox width, height
	width, height int        // image XObject size
	colorSpace    string
	jpeg          image.Config
}

var (
	objRe    = regexp.MustCompile(`(?s)(\d+) 0 obj\n(.*?)\nendobj\n`)
	refRe    = regexp.MustCompile(`/(\w+) (\d+) 0 R`)
	lengthRe = regexp.MustCompile(`/Length (\d+)`)
)

// readPDF parses the PDFs Relay writes, checking the structure a reader relies on: the header, every
// xref offset pointing at its object, startxref pointing at the xref table, the page tree and each
// page's image (decoded back from its DCTDecode stream).
func readPDF(t *testing.T, b []byte) []pdfPage {
	t.Helper()
	if !bytes.HasPrefix(b, []byte("%PDF-1.4\n")) || !bytes.HasSuffix(b, []byte("%%EOF\n")) {
		t.Fatalf("bad header or trailer")
	}
	m := regexp.MustCompile(`startxref\n(\d+)\n%%EOF\n$`).FindSubmatch(b)
	if m == nil {
		t.Fatal("no startxref")
	}
	xref, _ := strconv.Atoi(string(m[1]))
	if !bytes.HasPrefix(b[xref:], []byte("xref\n0 ")) {
		t.Fatalf("startxref %d does not point at the xref table", xref)
	}
	var count int
	fmt.Sscanf(string(b[xref:]), "xref\n0 %d\n", &count)
	lines := strings.Split(string(b[xref:]), "\n")[2 : 2+count]
	if lines[0] != "0000000000 65535 f " {
		t.Fatalf("xref entry 0 %q", lines[0])
	}
	objs := map[int]string{}
	streams := map[int][]byte{}
	for n := 1; n < count; n++ {
		if len(lines[n]) != 19 {
			t.Fatalf("xref entry %d is %d bytes, want 20 with the newline", n, len(lines[n])+1)
		}
		off, _ := strconv.Atoi(lines[n][:10])
		head := fmt.Sprintf("%d 0 obj\n", n)
		if !bytes.HasPrefix(b[off:], []byte(head)) {
			t.Fatalf("xref offset of object %d points at %q", n, b[off:off+20])
		}
		rest := b[off+len(head):]
		dict := string(rest[:bytes.IndexByte(rest, '\n')])
		objs[n] = dict
		if lm := lengthRe.FindStringSubmatch(dict); lm != nil {
			l, _ := strconv.Atoi(lm[1])
			s := rest[len(dict)+1:]
			if !bytes.HasPrefix(s, []byte("stream\n")) {
				t.Fatalf("object %d: no stream", n)
			}
			data := s[len("stream\n"):][:l]
			if !bytes.HasPrefix(s[len("stream\n")+l:], []byte("\nendstream\nendobj\n")) {
				t.Fatalf("object %d: /Length %d does not end the stream", n, l)
			}
			streams[n] = data
		}
	}
	if !strings.Contains(string(b), fmt.Sprintf("/Size %d /Root 1 0 R", count)) || !strings.Contains(objs[1], "/Type /Catalog /Pages 2 0 R") {
		t.Fatalf("catalog %q", objs[1])
	}
	kids := regexp.MustCompile(`(\d+) 0 R`).FindAllStringSubmatch(objs[2], -1)
	var cnt int
	fmt.Sscanf(objs[2][strings.Index(objs[2], "/Count"):], "/Count %d", &cnt)
	if cnt != len(kids) {
		t.Fatalf("page tree: /Count %d, %d kids", cnt, len(kids))
	}
	var pages []pdfPage
	for _, k := range kids {
		n, _ := strconv.Atoi(k[1])
		d := objs[n]
		if !strings.Contains(d, "/Type /Page ") || !strings.Contains(d, "/Parent 2 0 R") {
			t.Fatalf("page %d: %q", n, d)
		}
		var p pdfPage
		fmt.Sscanf(d[strings.Index(d, "/MediaBox"):], "/MediaBox [0 0 %f %f]", &p.box[0], &p.box[1])
		refs := map[string]int{}
		for _, r := range refRe.FindAllStringSubmatch(d, -1) {
			refs[r[1]], _ = strconv.Atoi(r[2])
		}
		content := string(streams[refs["Contents"]])
		if content != fmt.Sprintf("q %.2f 0 0 %.2f 0 0 cm /Im0 Do Q", p.box[0], p.box[1]) {
			t.Fatalf("page %d content %q", n, content)
		}
		im := objs[refs["Im0"]]
		if !strings.Contains(im, "/Subtype /Image") || !strings.Contains(im, "/Filter /DCTDecode") || !strings.Contains(im, "/BitsPerComponent 8") {
			t.Fatalf("image dict %q", im)
		}
		fmt.Sscanf(im[strings.Index(im, "/Width"):], "/Width %d /Height %d /ColorSpace %s", &p.width, &p.height, &p.colorSpace)
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(streams[refs["Im0"]]))
		if err != nil {
			t.Fatalf("page %d: embedded image is not a JPEG: %v", n, err)
		}
		p.jpeg = cfg
		pages = append(pages, p)
	}
	return pages
}

func TestBuildPDF(t *testing.T) {
	rgb := jpegBytes(t, 120, 80, false)
	gray := jpegBytes(t, 40, 60, true)
	pngImg := pngBytes(t, 30, 50)
	pdf, err := buildPDF([]providers.MediaFile{
		memFile("a.jpg", "image/jpeg", rgb), memFile("b.png", "image/png", pngImg), memFile("c.jpg", "image/jpeg", gray),
	}, maxDocument)
	if err != nil {
		t.Fatal(err)
	}
	pages := readPDF(t, pdf)
	if len(pages) != 3 {
		t.Fatalf("%d pages", len(pages))
	}
	want := []struct {
		w, h int
		cs   string
	}{{120, 80, "/DeviceRGB"}, {30, 50, "/DeviceRGB"}, {40, 60, "/DeviceGray"}}
	for i, w := range want {
		p := pages[i]
		if p.box != [2]float64{float64(w.w), float64(w.h)} || p.width != w.w || p.height != w.h || p.colorSpace != w.cs ||
			p.jpeg.Width != w.w || p.jpeg.Height != w.h {
			t.Errorf("page %d: %+v, want %dx%d %s", i+1, p, w.w, w.h, w.cs)
		}
	}
	// JPEGs are embedded byte for byte; the PNG was converted.
	if !bytes.Contains(pdf, rgb) || !bytes.Contains(pdf, gray) || bytes.Contains(pdf, pngImg) {
		t.Fatal("JPEGs must be embedded as they are")
	}

	// The 100 MB cap is enforced while writing.
	if _, err := buildPDF([]providers.MediaFile{memFile("a.jpg", "image/jpeg", rgb), memFile("a.jpg", "image/jpeg", rgb)}, int64(len(rgb))); !errors.Is(err, errTooBig) {
		t.Fatalf("size cap: %v", err)
	}
	// Not an image.
	if _, err := buildPDF([]providers.MediaFile{memFile("x.jpg", "image/jpeg", []byte("nope"))}, maxDocument); !errors.Is(err, providers.ErrInvalidPost) {
		t.Fatalf("bad image: %v", err)
	}
}

func TestPublishImagesAsDocument(t *testing.T) {
	var pdf []byte
	var post string
	f := newFake(t, nil)
	f.h = func(w http.ResponseWriter, r *http.Request, b []byte) bool {
		switch {
		case r.Method == "POST" && r.URL.Path == "/rest/documents" && r.URL.RawQuery == "action=initializeUpload":
			w.Write([]byte(`{"value":{"uploadUrl":"` + f.srv.URL + `/upload/doc","document":"urn:li:document:D7"}}`))
		case r.Method == "PUT" && r.URL.Path == "/upload/doc":
			if r.Header.Get("Content-Type") != "application/pdf" {
				t.Errorf("content type %q", r.Header.Get("Content-Type"))
			}
			pdf = b
		case r.Method == "GET" && r.URL.EscapedPath() == "/rest/documents/urn%3Ali%3Adocument%3AD7":
			w.Write([]byte(`{"status":"AVAILABLE"}`))
		case r.Method == "POST" && r.URL.Path == "/rest/posts":
			post = content(t, b)
			w.Header().Set("x-restli-id", "urn:li:share:5")
			w.WriteHeader(201)
		default:
			return false // no image upload: the images only travel inside the PDF
		}
		return true
	}
	c := f.client(t, nil, nil)
	imgs := []providers.MediaFile{memFile("1.jpg", "image/jpeg", jpegBytes(t, 108, 135, false)), memFile("2.png", "image/png", pngBytes(t, 108, 135)),
		memFile("3.jpg", "image/jpeg", jpegBytes(t, 108, 135, false))}
	title := strings.Repeat("t", 120)
	req := providers.PublishRequest{Parts: []string{title + "\nsecond line"}, Media: imgs, Options: map[string]any{"images_as_document": true}}
	if errs := validate(req); len(errs) != 0 {
		t.Fatalf("valid document refused: %v", errs)
	}
	res, err := c.Publish(context.Background(), req)
	if err != nil || res.RemoteID != "urn:li:share:5" {
		t.Fatalf("%v %+v", err, res)
	}
	if want := `{"media":{"id":"urn:li:document:D7","title":"` + strings.Repeat("t", 100) + `"}}`; post != want {
		t.Fatalf("content %s\nwant %s", post, want)
	}
	if pages := readPDF(t, pdf); len(pages) != 3 || pages[1].width != 108 || pages[1].height != 135 {
		t.Fatalf("pages %+v", pages)
	}
	// document_title wins.
	req.Options["document_title"] = "Spots de la semaine"
	if _, err := c.Publish(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(post, `"title":"Spots de la semaine"`) {
		t.Fatalf("content %s", post)
	}
}

func TestValidateImagesAsDocument(t *testing.T) {
	img := memFile("a.jpg", "image/jpeg", []byte("x"))
	many := make([]providers.MediaFile, 301)
	for i := range many {
		many[i] = img
	}
	big := img
	big.Size = 101 << 20
	cases := []struct {
		req  providers.PublishRequest
		want string
	}{
		{providers.PublishRequest{Parts: []string{"a"}, Media: []providers.MediaFile{img}}, "needs 2 to 300 images (got 1)"},
		{providers.PublishRequest{Parts: []string{"a"}, Media: many}, "needs 2 to 300 images (got 301)"},
		{providers.PublishRequest{Parts: []string{"a"}, Media: []providers.MediaFile{img, memFile("v.mp4", "video/mp4", []byte("v"))}}, "takes JPEG/PNG/GIF images only"},
		{providers.PublishRequest{Parts: []string{"a"}, Media: []providers.MediaFile{img, big}}, "exceed LinkedIn's 100 MB document limit"},
		{providers.PublishRequest{Parts: []string{"a"}, Media: []providers.MediaFile{img, img}, Options: map[string]any{"link": "https://x.example"}}, "cannot be combined with options.link"},
		{providers.PublishRequest{Parts: []string{"a"}, Media: []providers.MediaFile{img, img}, Options: map[string]any{"document_title": strings.Repeat("é", 101)}}, "document_title is limited to 100 characters"},
	}
	for _, c := range cases {
		if c.req.Options == nil {
			c.req.Options = map[string]any{}
		}
		c.req.Options["images_as_document"] = true
		if errs := strings.Join(validate(c.req), "; "); !strings.Contains(errs, c.want) {
			t.Errorf("want %q, got %v", c.want, errs)
		}
	}
	// More than 20 images are fine as a document, not as a multi-image post.
	ok := providers.PublishRequest{Parts: []string{"a"}, Media: many[:30], Options: map[string]any{"images_as_document": true}}
	if errs := validate(ok); len(errs) != 0 {
		t.Errorf("30 images as a document: %v", errs)
	}
	// Publish refuses too few images even without the pre-flight check (force).
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, b []byte) bool { return false })
	_, err := f.client(t, nil, nil).Publish(context.Background(), providers.PublishRequest{Parts: []string{"a"}, Media: []providers.MediaFile{img}, Options: map[string]any{"images_as_document": true}})
	if !errors.Is(err, providers.ErrInvalidPost) {
		t.Fatalf("publish: %v", err)
	}
}
