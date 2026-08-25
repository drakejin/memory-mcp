package document

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/drakejin/memory-mcp/internal/x/errs"
)

// assemblePDF builds a minimal but well-formed PDF from numbered object
// bodies, computing the xref table offsets at runtime.
func assemblePDF(objects []string) []byte {
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects)+1)
	for i, obj := range objects {
		offsets[i+1] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xrefPos := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", len(objects)+1)
	buf.WriteString("0000000000 65535 f \n")
	for i := 1; i <= len(objects); i++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xrefPos)
	return buf.Bytes()
}

// textPDF returns a one-page PDF whose text layer says "Hello memory world".
func textPDF() []byte {
	stream := "BT /F1 12 Tf 72 720 Td (Hello memory world) Tj ET"
	return assemblePDF([]string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	})
}

// noTextPDF returns a valid one-page PDF with no content stream at all —
// the deterministic stand-in for a scanned document.
func noTextPDF() []byte {
	return assemblePDF([]string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>",
	})
}

// errReader fails mid-stream, standing in for a truncated upload.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("stream reset") }

// TestTextExtractorReadFailure: an unreadable upload is a failure, never an
// empty "unextractable" document — that would silently index nothing.
func TestTextExtractorReadFailure(t *testing.T) {
	text, extractable, err := textExtractor{}.Extract(context.Background(), "note.txt", errReader{})
	if !errors.Is(err, errs.ErrInternal) {
		t.Fatalf("err = %v, want internal", err)
	}
	if text != "" || extractable {
		t.Errorf("got text=%q extractable=%v, want empty and false", text, extractable)
	}
}

func TestTextExtractorExtract(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name            string
		filename        string
		data            []byte
		wantText        string // substring match; "" means exact empty
		wantExtractable bool
		wantErr         bool
	}{
		{
			name:            "plain text passthrough",
			filename:        "note.txt",
			data:            []byte("메모리 서버 설계 노트"),
			wantText:        "메모리 서버 설계 노트",
			wantExtractable: true,
		},
		{
			name:            "markdown passthrough",
			filename:        "README.md",
			data:            []byte("# 제목\n본문"),
			wantText:        "# 제목",
			wantExtractable: true,
		},
		{
			name:            "invalid utf8 text reported unextractable",
			filename:        "broken.txt",
			data:            []byte{0xff, 0xfe, 0x00, 0x81},
			wantExtractable: false,
		},
		{
			name:            "unknown format reported unextractable",
			filename:        "image.png",
			data:            []byte{0x89, 'P', 'N', 'G'},
			wantExtractable: false,
		},
		{
			name:            "pdf with text layer",
			filename:        "doc.pdf",
			data:            textPDF(),
			wantText:        "Hello",
			wantExtractable: true,
		},
		{
			name:            "pdf magic overrides wrong extension",
			filename:        "doc.bin",
			data:            textPDF(),
			wantText:        "Hello",
			wantExtractable: true,
		},
		{
			name:            "scanned pdf without text layer",
			filename:        "scan.pdf",
			data:            noTextPDF(),
			wantExtractable: false,
		},
		{
			name:     "corrupt pdf is an error",
			filename: "corrupt.pdf",
			data:     []byte("%PDF-1.4 this is not a real pdf"),
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, extractable, err := textExtractor{}.Extract(ctx, tt.filename, bytes.NewReader(tt.data))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got text=%q extractable=%v", text, extractable)
				}
				// A parse failure is ours to log, not the client's to read:
				// it must not be reported as invalid input or not-found.
				if !errors.Is(err, errs.ErrInternal) {
					t.Fatalf("err = %v, want internal", err)
				}
				var domain *errs.Error
				if errors.As(err, &domain); domain.Op != opExtract {
					t.Errorf("op = %q, want %q", domain.Op, opExtract)
				}
				return
			}
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			if extractable != tt.wantExtractable {
				t.Errorf("extractable = %v, want %v (text=%q)", extractable, tt.wantExtractable, text)
			}
			if tt.wantExtractable && !strings.Contains(text, tt.wantText) {
				t.Errorf("text = %q, want substring %q", text, tt.wantText)
			}
			if !tt.wantExtractable && text != "" {
				t.Errorf("unextractable input must yield empty text, got %q", text)
			}
		})
	}
}
