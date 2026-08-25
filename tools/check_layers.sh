#!/usr/bin/env bash
# check_layers.sh — mechanical enforcement of the §4.1 layer rules
# (docs/design/feature-inventory.md). Run as `make check-layers`.
#
# Layers:
#   handler  = internal/handler/...
#   service  = internal/service/...
#   external = internal/external/...
#   app      = internal/app/...
#   cmd      = cmd/...
#
# Rules enforced (production imports only; _test.go files may cross layers, and
# `go list -f .Imports` already excludes them):
#   1. handler must not import internal/external/*  (§4.1 rule 1/3: handlers
#      see service interfaces only)
#   2. service must not import internal/external/*  (§4.1 rule 2: services
#      declare their own ports; external satisfies them structurally)
#   3. external must not import handler, app, or a service ORCHESTRATION
#      package. Exception (§4.1 rule 4 — domain value types may cross layers):
#      adapters may import the packages whose domain values they persist or
#      serve — service/episode (Record, Query, Hit), service/knowledge
#      (Node, Edge, Graph), service/rehydrate (Manifest, Plane),
#      service/document (the sha256 content-address format). consolidate is
#      pure orchestration and stays banned.
#   4. nothing imports internal/app except app itself and cmd (the composition
#      root is a leaf: only the process edge may reach it)
set -euo pipefail

cd "$(dirname "$0")/.."

MODULE="github.com/drakejin/memory-mcp"

# Service packages whose DOMAIN VALUE TYPES external adapters may import
# (rule 3 exception). Keep this list short and justified.
DOMAIN_ALLOW=(
  "$MODULE/internal/service/episode"
  "$MODULE/internal/service/knowledge"
  "$MODULE/internal/service/rehydrate"
  "$MODULE/internal/service/document"
)

violations=0

report() {
  echo "LAYER VIOLATION [$1]: $2 imports $3" >&2
  violations=$((violations + 1))
}

allowed_domain() {
  local imp="$1"
  for a in "${DOMAIN_ALLOW[@]}"; do
    if [ "$imp" = "$a" ]; then
      return 0
    fi
  done
  return 1
}

while read -r pkg imports; do
  for imp in $imports; do
    case "$imp" in
      "$MODULE"/*) ;; # only in-module imports are layer-relevant
      *) continue ;;
    esac

    case "$pkg" in
      "$MODULE"/internal/handler/*)
        case "$imp" in
          "$MODULE"/internal/external/*) report "handler->external" "$pkg" "$imp" ;;
        esac
        ;;
      "$MODULE"/internal/service/*)
        case "$imp" in
          "$MODULE"/internal/external/*) report "service->external" "$pkg" "$imp" ;;
        esac
        ;;
      "$MODULE"/internal/external/*)
        case "$imp" in
          "$MODULE"/internal/handler/*) report "external->handler" "$pkg" "$imp" ;;
          "$MODULE"/internal/service/*)
            if ! allowed_domain "$imp"; then
              report "external->service" "$pkg" "$imp"
            fi
            ;;
        esac
        ;;
    esac

    # Rule 4: internal/app is reachable from app itself and cmd only.
    case "$imp" in
      "$MODULE"/internal/app/*)
        case "$pkg" in
          "$MODULE"/internal/app/* | "$MODULE"/cmd/*) ;;
          *) report "app-leak" "$pkg" "$imp" ;;
        esac
        ;;
    esac
  done
done < <(go list -f '{{.ImportPath}} {{range .Imports}}{{.}} {{end}}' ./cmd/... ./internal/...)

if [ "$violations" -gt 0 ]; then
  echo "check-layers: $violations violation(s)" >&2
  exit 1
fi
echo "check-layers: OK"
