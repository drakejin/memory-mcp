package document

import "regexp"

// shaPattern is a full lowercase-hex sha256 — the §6 content-address format.
var shaPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidSHA reports whether sha is a well-formed content address: 64 lowercase
// hex characters. It is pure, so it needs no Service.
//
// It lives in this package because the document domain owns the
// content-address format (P8, §6): the HTTP boundary rejects a malformed
// {sha} path parameter before any pipeline call, and the blob cache adapter
// refuses to embed a malformed sha in a filesystem path — both against this
// one pattern, never a second copy (code-standards §4). Mirrors the
// ulid.Valid precedent.
func ValidSHA(sha string) bool {
	return shaPattern.MatchString(sha)
}
