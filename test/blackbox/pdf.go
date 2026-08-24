// Package blackbox holds the §10 acceptance scenarios (architecture-v2.md).
//
// This file is untagged on purpose: makeMinimalPDF is a pure fixture generator
// verified by a normal unit test (pdf_test.go), while the scenario drivers are
// behind the `blackbox` build tag.
package blackbox

import (
	"bytes"
	"fmt"
	"strings"
)

// makeMinimalPDF builds a deterministic single-page PDF with a real text
// layer (ASCII, Helvetica, uncompressed content stream) so the document
// pipeline's ledongthuc/pdf extractor finds honest extractable text.
// Korean morphology is covered by the episode fixtures, not the PDF (§10.4).
func makeMinimalPDF(lines []string) []byte {
	var content strings.Builder
	content.WriteString("BT\n/F1 12 Tf\n16 TL\n72 720 Td\n")
	for _, line := range lines {
		content.WriteString("(" + escapePDFText(line) + ") Tj\nT*\n")
	}
	content.WriteString("ET")

	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] " +
			"/Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", content.Len(), content.String()),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>",
	}

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects)+1)
	for i, obj := range objects {
		offsets[i+1] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xrefPos := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", len(objects)+1)
	// Each xref entry must be exactly 20 bytes: 10-digit offset, space,
	// 5-digit generation, space, type char, space, newline.
	buf.WriteString("0000000000 65535 f \n")
	for i := 1; i <= len(objects); i++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		len(objects)+1, xrefPos)
	return buf.Bytes()
}

// escapePDFText escapes the three characters that are special inside a PDF
// literal string. Fixture text is ASCII by design.
func escapePDFText(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `(`, `\(`, `)`, `\)`)
	return r.Replace(s)
}
