package document

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"

	"github.com/drakejin/memory-mcp/internal/x/errs"
)

// opExtract names the extraction step in errors returned by the built-in
// extractor.
const opExtract = "document.Extract"

// Extensions carrying a deterministic text layer.
const (
	extPDF      = ".pdf"
	extMD       = ".md"
	extMarkdown = ".markdown"
	extTXT      = ".txt"
	extText     = ".text"
)

// pdfMagic is the byte signature every PDF starts with; it overrides the
// filename extension so misnamed PDFs still hit the text-layer path.
var pdfMagic = []byte("%PDF-")

// Extractor produces deterministic plain text from a document (§6 step 3).
// Supported by the built-in implementation: PDF text layer, markdown, plain
// text. Anything without a text layer yields extractable=false and no error —
// honesty over guessing.
type Extractor interface {
	// Extract reads all of data and returns extracted text. extractable is
	// false when the format is recognized but has no deterministic text (e.g.
	// scanned PDF); err is reserved for IO/corruption failures.
	Extract(ctx context.Context, filename string, data io.Reader) (string, bool, error)
}

// textExtractor dispatches on filename extension / magic bytes: PDF via
// github.com/ledongthuc/pdf, .md/.txt passed through as UTF-8. It is the
// default selected by New when Config.Extractor is nil.
type textExtractor struct{}

// Compile-time contract check.
var _ Extractor = textExtractor{}

// Extract implements Extractor.
func (textExtractor) Extract(_ context.Context, filename string, data io.Reader) (string, bool, error) {
	buf, err := io.ReadAll(data)
	if err != nil {
		return "", false, errs.Internal(opExtract, err).WithField("filename", filename)
	}

	if bytes.HasPrefix(buf, pdfMagic) || strings.EqualFold(filepath.Ext(filename), extPDF) {
		return extractPDF(bytes.NewReader(buf), int64(len(buf)), filename)
	}

	switch strings.ToLower(filepath.Ext(filename)) {
	case extMD, extMarkdown, extTXT, extText:
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
// ("", false, nil) when pages contain no text layer (e.g. scanned pages); err
// is reserved for parse/corruption failures.
func extractPDF(ra io.ReaderAt, size int64, filename string) (text string, extractable bool, err error) {
	// ledongthuc/pdf panics on some malformed inputs. Recovering here is the
	// one place this package tolerates a panic (code-standards §3 forbids
	// raising them): a corrupt upload must never take the server down.
	defer func() {
		if r := recover(); r != nil {
			text, extractable = "", false
			err = errs.Internal(opExtract, nil).
				WithField("filename", filename).
				WithField("panic", r)
		}
	}()

	reader, err := pdf.NewReader(ra, size)
	if err != nil {
		return "", false, errs.Internal(opExtract, err).WithField("filename", filename)
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
