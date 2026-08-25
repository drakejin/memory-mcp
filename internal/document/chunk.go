package document

import (
	"unicode/utf8"

	"github.com/drakejin/memory-mcp/internal/config"
)

// uncapped is the maxChunks value that disables the cap.
const uncapped = 0

// chunkDocument splits extracted text with the deterministic spec limits of §6
// step 4, so no caller restates them.
func chunkDocument(text string) ([]string, Truncation) {
	return chunkText(text, config.DocumentChunkBytes, config.MaxDocumentChunks)
}

// chunkText splits text into ~chunkBytes chunks (UTF-8 safe: a rune is never
// split) capped at maxChunks. The returned Truncation always carries the true
// total, which is what makes the honesty report of §6 possible; maxChunks
// <= uncapped means no cap.
func chunkText(text string, chunkBytes, maxChunks int) ([]string, Truncation) {
	if text == "" {
		return nil, Truncation{}
	}
	if chunkBytes <= 0 {
		chunkBytes = config.DocumentChunkBytes
	}

	var chunks []string
	total := 0
	start := 0
	for i := 0; i < len(text); {
		_, size := utf8.DecodeRuneInString(text[i:])
		if i > start && (i-start)+size > chunkBytes {
			total++
			if maxChunks <= uncapped || len(chunks) < maxChunks {
				chunks = append(chunks, text[start:i])
			}
			start = i
		}
		i += size
	}
	// Final partial chunk (start < len(text) always holds for non-empty text).
	total++
	if maxChunks <= uncapped || len(chunks) < maxChunks {
		chunks = append(chunks, text[start:])
	}

	return chunks, Truncation{Total: total, Indexed: len(chunks)}
}
