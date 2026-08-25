package episodemem

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/drakejin/memory-mcp/internal/x/errs"
)

// URL fragments — the only places the index name and the OpenSearch endpoints
// enter a request path.
const (
	rootPath       = "/"
	bulkPath       = "/_bulk"
	settingsSuffix = "/_settings"
	searchSuffix   = "/_search"
	countSuffix    = "/_count"
	// deleteByQuerySuffix removes every document matching a query — the
	// project-scoped deletion partial rehydration needs (§5).
	deleteByQuerySuffix = "/_delete_by_query"

	contentTypeJSON   = "application/json"
	contentTypeNDJSON = "application/x-ndjson"
)

// indexPath builds "/{index}{suffix}".
func (c *client) indexPath(suffix string) string { return rootPath + c.index + suffix }

// rawRequest adapts a plain HTTP call to the opensearch-go Request interface.
type rawRequest struct {
	path        string
	query       url.Values
	body        []byte
	contentType string
}

// GetRequest implements opensearch.Request.
func (r rawRequest) GetRequest(method string) (*http.Request, error) {
	target := r.path
	if len(r.query) > 0 {
		target += "?" + r.query.Encode()
	}
	var body io.Reader
	if r.body != nil {
		body = bytes.NewReader(r.body)
	}
	req, err := http.NewRequest(method, target, body)
	if err != nil {
		return nil, fmt.Errorf("build request %s %s: %w", method, r.path, err)
	}
	if r.body != nil {
		ct := r.contentType
		if ct == "" {
			ct = contentTypeJSON
		}
		req.Header.Set("Content-Type", ct)
	}
	return req, nil
}

// do executes one request and returns (status, body). Transport failures and
// 5xx responses become KindUnavailable so callers can degrade (§5); other
// error statuses are returned as (status, body, nil) for per-endpoint
// handling, because only the endpoint knows whether a 404 is a failure.
func (c *client) do(ctx context.Context, op, method string, req rawRequest) (int, []byte, error) {
	resp, err := c.os.Do(ctx, method, req, nil)
	if resp == nil {
		return 0, nil, unreachable(op, method, req.path, err)
	}
	var body []byte
	var readErr error
	if resp.Body != nil {
		body, readErr = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	switch {
	case err != nil:
		return resp.StatusCode, body, unreachable(op, method, req.path, err)
	case readErr != nil:
		return resp.StatusCode, body, unreachable(op, method, req.path, fmt.Errorf("read body: %w", readErr))
	case resp.StatusCode >= http.StatusInternalServerError:
		return resp.StatusCode, body, unreachable(op, method, req.path, statusCause(resp.StatusCode, body))
	}
	return resp.StatusCode, body, nil
}

// Ping implements Client.
func (c *client) Ping(ctx context.Context) error {
	status, body, err := c.do(ctx, opPing, http.MethodGet, rawRequest{path: rootPath})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return unreachable(opPing, http.MethodGet, rootPath, statusCause(status, body))
	}
	return nil
}

// unreachable classifies a cluster we could not reach or that answered 5xx.
// KindUnavailable is the degraded-mode signal of §5 — never use it for a
// response we simply did not expect.
func unreachable(op, method, path string, cause error) error {
	return errs.Unavailable(op, cause).
		WithField("method", method).
		WithField("path", path)
}

// indexAbsent classifies a read against an index that does not exist. It is
// KindUnavailable rather than KindNotFound because nothing the caller asked for
// is missing — the derived store itself is, which is the degraded-mode signal
// a read turns into 503 (§5).
func (c *client) indexAbsent(op string) error {
	return errs.Unavailable(op, fmt.Errorf("index %s does not exist", c.index)).
		WithField("index", c.index)
}

// unexpected classifies a response this client cannot act on: a status our own
// request should never provoke. The cause carries the response body and stays
// in the log; the client only ever sees the generic internal message.
func (c *client) unexpected(op, what string, status int, body []byte) error {
	e := errs.Internal(op, fmt.Errorf("%s: %w", what, statusCause(status, body)))
	e.Entity = entityIndex
	e.ID = c.index
	return e.WithField("status", status)
}

// malformed classifies our own encode failure or a response we could not
// parse — a bug on this side of the wire, never a caller error.
func malformed(op, what string, cause error) error {
	return errs.Internal(op, fmt.Errorf("%s: %w", what, cause))
}

// statusCause renders a status plus response body as the log-only cause of a
// semantic error.
func statusCause(status int, body []byte) error {
	return fmt.Errorf("status %d: %s", status, body)
}
