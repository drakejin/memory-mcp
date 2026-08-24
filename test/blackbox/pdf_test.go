package blackbox

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/ledongthuc/pdf"
)

// TestMakeMinimalPDF proves the generated fixture has a real text layer
// readable by ledongthuc/pdf — the same library the document extractor uses
// (§6 step 3). If this fails, scenario 4 would fail for fixture reasons
// rather than product reasons, so it is guarded here as a plain unit test.
func TestMakeMinimalPDF(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  []string
	}{
		{
			name:  "single line marker",
			lines: []string{"memorymcp blackbox marker seoulnine"},
			want:  []string{"seoulnine"},
		},
		{
			name: "multi line with specials",
			lines: []string{
				"memory-mcp acceptance fixture (v2)",
				"backslash \\ and parens () survive escaping",
				"unique token seoulnine again",
			},
			want: []string{"acceptance fixture", "survive escaping", "seoulnine"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange / Act
			raw := makeMinimalPDF(tt.lines)
			r, err := pdf.NewReader(bytes.NewReader(raw), int64(len(raw)))
			if err != nil {
				t.Fatalf("pdf.NewReader: %v", err)
			}
			plain, err := r.GetPlainText()
			if err != nil {
				t.Fatalf("GetPlainText: %v", err)
			}
			extracted, err := io.ReadAll(plain)
			if err != nil {
				t.Fatalf("read plain text: %v", err)
			}

			// Assert
			text := string(extracted)
			for _, want := range tt.want {
				if !strings.Contains(text, want) {
					t.Errorf("extracted text missing %q; got: %q", want, text)
				}
			}
		})
	}
}
