package knowledgemem

import (
	"context"
	"errors"
	"log/slog"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"

	"github.com/drakejin/memory-mcp/internal/x/errs"
)

// runner is the narrow consumer-side view of the Neo4j driver this package
// needs (code-standards §1.1): probe connectivity, run one statement inside a
// managed transaction, release the pool. Unit tests substitute a fake; the live
// and blackbox suites exercise boltRunner against the real container.
type runner interface {
	Verify(ctx context.Context) error
	Run(ctx context.Context, write bool, query string, params map[string]any) ([]*neo4j.Record, error)
	Close(ctx context.Context) error
}

// boltRunner is the production runner backed by the bolt driver.
type boltRunner struct {
	driver neo4j.DriverWithContext
	log    *slog.Logger
}

// Verify implements runner.
func (b *boltRunner) Verify(ctx context.Context) error {
	return b.driver.VerifyConnectivity(ctx)
}

// Close implements runner.
func (b *boltRunner) Close(ctx context.Context) error {
	return b.driver.Close(ctx)
}

// Run implements runner: one auto-commit managed transaction, all records
// collected. Driver errors are returned untouched — mapErr classifies them.
func (b *boltRunner) Run(ctx context.Context, write bool, query string, params map[string]any) ([]*neo4j.Record, error) {
	mode := neo4j.AccessModeRead
	if write {
		mode = neo4j.AccessModeWrite
	}
	session := b.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: mode})
	defer func() {
		if err := session.Close(ctx); err != nil {
			b.log.DebugContext(ctx, "graph session close failed", "error", err)
		}
	}()

	work := func(tx neo4j.ManagedTransaction) (any, error) {
		result, err := tx.Run(ctx, query, params)
		if err != nil {
			return nil, err
		}
		return result.Collect(ctx)
	}
	var out any
	var err error
	if write {
		out, err = session.ExecuteWrite(ctx, work)
	} else {
		out, err = session.ExecuteRead(ctx, work)
	}
	if err != nil {
		return nil, err
	}
	records, _ := out.([]*neo4j.Record)
	return records, nil
}

// mapErr gives a driver failure its semantic kind (§2.1). Lost or refused
// connections and expired deadlines are KindUnavailable — the degraded-mode
// signal (§5); anything else is an unclassified KindInternal. An error that
// already carries a Kind keeps it and only gains this op.
func mapErr(op string, err error) error {
	if err == nil {
		return nil
	}
	var domain *errs.Error
	if errors.As(err, &domain) {
		return errs.Wrap(op, err)
	}
	if isUnavailable(err) {
		return errs.Unavailable(op, err)
	}
	return errs.Internal(op, err)
}

// isUnavailable classifies driver failures that mean "the store is not there
// right now". Beyond direct connectivity errors and expired deadlines, the
// managed-transaction path returns *TransactionExecutionLimit once its ~30s
// retry budget is exhausted against a dead database — a wrapper that carries
// its attempts in Errors without implementing Unwrap, so errors.Is/As cannot
// see through it. Without this case a downed Neo4j surfaced as KindInternal
// (HTTP 500) instead of the §5 degraded 503 ("graph unavailable"), breaking
// the F18 read contract e2e P6 asserts. The last attempt is the one the
// wrapper's own Error() string reports; it decides the classification.
func isUnavailable(err error) bool {
	if neo4j.IsConnectivityError(err) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var limit *neo4j.TransactionExecutionLimit
	if !errors.As(err, &limit) || len(limit.Errors) == 0 {
		return false
	}
	last := limit.Errors[len(limit.Errors)-1]
	return neo4j.IsConnectivityError(last) || errors.Is(last, context.DeadlineExceeded)
}
