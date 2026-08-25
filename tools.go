//go:build tools

// Package tools pins build-time tool dependencies (swag CLI) in go.mod so
// `make swagger` runs the repo-pinned version via `go run`.
package tools

import (
	_ "github.com/swaggo/swag/cmd/swag"
)
