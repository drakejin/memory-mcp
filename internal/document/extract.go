package document

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
)

// pdfMagic is the byte signature every PDF starts with; it overrides the
// filename extension so misnamed PDFs still hit the text-layer path.
var pdfMagic = []byte("%PDF-")

// Extractor produces deterministic plain text from a document (§6 step 3).
// Supported: PDF text layer, markdown, plain text. Anything without a text
// layer returns extractable=false with no error — honesty over guessing.
type Extractor interface {
	// Extract reads all of data and returns extracted text. extractable is
	// false when the format is recognized but has no deterministic text
	// (e.g. scanned PDF); err is reserved for IO/corruption failures.
	Extract(ctx context.Context, filename string, data io.Reader) (text string, extractable bool, err error)
}

// DefaultExtractor dispatches on filename extension / magic bytes: PDF via
// github.com/ledongthuc/pdf, .md/.txt passed through as UTF-8.
type DefaultExtractor struct{}

// Compile-time contract check.
var _ Extractor = DefaultExtractor{}

// Extract implements Extractor.
func (DefaultExtractor) Extract(ctx context.Context, filename string, data io.Reader) (string, bool, error) {
	buf, err := io.ReadAll(data)
	if err != nil {
		return "", false, fmt.Errorf("document: read %s: %w", filename, err)
	}

	if bytes.HasPrefix(buf, pdfMagic) || strings.EqualFold(filepath.Ext(filename), ".pdf") {
		return extractPDF(bytes.NewReader(buf), int64(len(buf)))
	}

	switch strings.ToLower(filepath.Ext(filename)) {
	case ".md", ".markdown", ".txt", ".text":
		if !utf8.Valid(buf) {
			// Recognized text format but not decodable text — report honestly
			// instead of guessing an encoding.
			return "", false, nil
		}
		return string(buf), true, nil
	default:
		// Unrecognized format: no deterministic extraction exists (§6 step 3).
		return "", false, nil
	}
}

// extractPDF pulls the text layer from a PDF held in ra. It returns
// ("", false, nil) when pages contain no text layer (e.g. scanned pages);
// err is reserved for parse/corruption failures.
func extractPDF(ra io.ReaderAt, size int64) (text string, extractable bool, err error) {
	// ledongthuc/pdf panics on some malformed inputs; convert to an error so
	// a corrupt upload never takes the server down.
	defer func() {
		if r := recover(); r != nil {
			text, extractable = "", false
			err = fmt.Errorf("document: pdf parse panic: %v", r)
		}
	}()

	reader, err := pdf.NewReader(ra, size)
	if err != nil {
		return "", false, fmt.Errorf("document: pdf open: %w", err)
	}

	var b strings.Builder
	for pageNum := 1; pageNum <= reader.NumPage(); pageNum++ {
		page := reader.Page(pageNum)
		if page.V.IsNull() {
			continue
		}
		pageText, err := page.GetPlainText(nil)
		if err != nil {
			// A page that fails text extraction is a page without a usable
			// text layer; skip it rather than fabricate content.
			continue
		}
		if pageText == "" {
			continue
		}
		b.WriteString(pageText)
		b.WriteByte('\n')
	}

	out := strings.TrimSpace(b.String())
	if out == "" {
		return "", false, nil
	}
	return out, true, nil
}
