package document

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/drakejin/memory-mcp/internal/x/config"
)

func TestChunkText(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		chunkBytes int
		maxChunks  int
		want       []string
		wantTrunc  Truncation
	}{
		{
			name: "empty text yields nothing",
			text: "", chunkBytes: 4, maxChunks: 10,
			want: nil, wantTrunc: Truncation{},
		},
		{
			name: "text under one chunk",
			text: "abc", chunkBytes: 8, maxChunks: 10,
			want: []string{"abc"}, wantTrunc: Truncation{Total: 1, Indexed: 1},
		},
		{
			name: "splits on byte budget",
			text: "abcde", chunkBytes: 2, maxChunks: 10,
			want: []string{"ab", "cd", "e"}, wantTrunc: Truncation{Total: 3, Indexed: 3},
		},
		{
			name: "exact multiple has no empty tail",
			text: "abcd", chunkBytes: 2, maxChunks: 10,
			want: []string{"ab", "cd"}, wantTrunc: Truncation{Total: 2, Indexed: 2},
		},
		{
			name: "never splits a rune",
			text: "가나다", chunkBytes: 4, maxChunks: 10, // each rune is 3 bytes
			want: []string{"가", "나", "다"}, wantTrunc: Truncation{Total: 3, Indexed: 3},
		},
		{
			name: "rune wider than budget still emitted whole",
			text: "가", chunkBytes: 1, maxChunks: 10,
			want: []string{"가"}, wantTrunc: Truncation{Total: 1, Indexed: 1},
		},
		{
			name: "cap truncates but total stays honest",
			text: "abcdef", chunkBytes: 2, maxChunks: 2,
			want: []string{"ab", "cd"}, wantTrunc: Truncation{Total: 3, Indexed: 2},
		},
		{
			name: "maxChunks zero means uncapped",
			text: "abcdef", chunkBytes: 2, maxChunks: uncapped,
			want: []string{"ab", "cd", "ef"}, wantTrunc: Truncation{Total: 3, Indexed: 3},
		},
		{
			name: "non-positive chunk size falls back to the spec budget",
			text: "abcdef", chunkBytes: 0, maxChunks: 10,
			want: []string{"abcdef"}, wantTrunc: Truncation{Total: 1, Indexed: 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, trunc := chunkText(tt.text, tt.chunkBytes, tt.maxChunks)
			if !slices.Equal(got, tt.want) {
				t.Errorf("chunks = %q, want %q", got, tt.want)
			}
			if trunc != tt.wantTrunc {
				t.Errorf("truncation = %+v, want %+v", trunc, tt.wantTrunc)
			}
			for i, c := range got {
				if !utf8.ValidString(c) {
					t.Errorf("chunk %d is not valid UTF-8: %q", i, c)
				}
			}
		})
	}
}

// TestChunkDocumentUsesSpecLimits keeps the §6 budget and cap in one place: a
// drift here silently changes what gets indexed.
func TestChunkDocumentUsesSpecLimits(t *testing.T) {
	overflow := config.MaxDocumentChunks + 1
	chunks, trunc := chunkDocument(strings.Repeat("a", overflow*config.DocumentChunkBytes))

	if len(chunks) != config.MaxDocumentChunks {
		t.Errorf("chunks = %d, want the %d cap", len(chunks), config.MaxDocumentChunks)
	}
	if trunc.Total != overflow || trunc.Indexed != config.MaxDocumentChunks {
		t.Errorf("truncation = %+v, want {%d %d}", trunc, overflow, config.MaxDocumentChunks)
	}
	for i, c := range chunks {
		if len(c) != config.DocumentChunkBytes {
			t.Fatalf("chunk %d is %d bytes, want the %d budget", i, len(c), config.DocumentChunkBytes)
		}
	}
}
