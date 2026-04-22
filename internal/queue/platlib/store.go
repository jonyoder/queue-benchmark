package platlib

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	plqueue "github.com/rstudio/platform-lib/v3/pkg/rsqueue/queue"
	"github.com/rstudio/platform-lib/v3/pkg/rsqueue/permit"
)

//go:embed schema.sql
var schemaSQL string

// applySchema idempotently creates the rsqueue tables in our Postgres DB.
// Called once from New before the DatabaseQueue is constructed.
func applySchema(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}

// pgNotifier is the minimal interface the store needs to emit in-process
// notifications. Abstracted so tests can inspect them if needed.
type pgNotifier interface {
	Notify(channel string, n any) error
}

// pgStore implements plqueue.QueueStore against Postgres using pgx. It is
// non-transactional by default; BeginTransactionQueue returns a *pgStore
// that shares a pgx transaction. The example's GORM-based store is the
// reference; this reimplementation uses pgx directly and skips GORM.
type pgStore struct {
	pool     *pgxpool.Pool
	tx       pgx.Tx // non-nil when this store is inside a transaction
	notifier pgNotifier

	// pendingNotifications are queued during transactions and flushed
	// on CompleteTransaction(nil).
	pendingNotifications []pendingNote
}

type pendingNote struct {
	channel string
	payload any
}

// newPgStore returns a top-level (non-transactional) store.
func newPgStore(pool *pgxpool.Pool, notifier pgNotifier) *pgStore {
	return &pgStore{pool: pool, notifier: notifier}
}

// --- helpers ---

func (s *pgStore) exec(ctx context.Context, sqlStr string, args ...any) (pgconn.CommandTag, error) {
	if s.tx != nil {
		return s.tx.Exec(ctx, sqlStr, args...)
	}
	return s.pool.Exec(ctx, sqlStr, args...)
}

func (s *pgStore) query(ctx context.Context, sqlStr string, args ...any) (pgx.Rows, error) {
	if s.tx != nil {
		return s.tx.Query(ctx, sqlStr, args...)
	}
	return s.pool.Query(ctx, sqlStr, args...)
}

func (s *pgStore) queryRow(ctx context.Context, sqlStr string, args ...any) pgx.Row {
	if s.tx != nil {
		return s.tx.QueryRow(ctx, sqlStr, args...)
	}
	return s.pool.QueryRow(ctx, sqlStr, args...)
}

// queueDbNote is the notification payload for the "work ready" signal.
// The DatabaseQueue's internal broadcaster identifies this by the
// NotifyType field. The example uses a dedicated type; we mirror its
// wire shape here so the broadcaster's matcher recognizes it.
type queueDbNote struct {
	NotifyType uint8 `json:"NotifyType"`
}

func (q queueDbNote) Type() uint8 { return q.NotifyType }
func (queueDbNote) Guid() string  { return "" }

// --- QueueStore implementation ---

// CompleteTransaction flushes pending notifications on success and rolls
// back otherwise. Called by DatabaseQueue after any operation that began
// a transaction via BeginTransactionQueue.
func (s *pgStore) CompleteTransaction(errp *error) {
	if s.tx == nil {
		return
	}
	ctx := context.Background()
	if errp != nil && *errp != nil {
		_ = s.tx.Rollback(ctx)
		s.tx = nil
		s.pendingNotifications = nil
		return
	}
	if commitErr := s.tx.Commit(ctx); commitErr != nil {
		if errp != nil {
			*errp = commitErr
		}
		s.tx = nil
		s.pendingNotifications = nil
		return
	}
	s.tx = nil
	// Flush pending notifications post-commit.
	if s.notifier != nil {
		for _, n := range s.pendingNotifications {
			_ = s.notifier.Notify(n.channel, n.payload)
		}
	}
	s.pendingNotifications = nil
}

// BeginTransactionQueue returns a store view bound to a fresh pgx.Tx.
// The description is logged-but-ignored.
func (s *pgStore) BeginTransactionQueue(ctx context.Context, description string) (plqueue.QueueStore, error) {
	if s.tx != nil {
		return nil, errors.New("platlib store: nested transactions not supported")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	return &pgStore{
		pool:     s.pool,
		tx:       tx,
		notifier: s.notifier,
	}, nil
}

// NotifyExtend is a no-op for the benchmark — we don't implement the
// leader-coordinated permit-timeout recovery path. Permits are released
// when work completes or when the process restarts.
func (s *pgStore) NotifyExtend(ctx context.Context, p uint64) error {
	return nil
}

// QueuePush inserts a new (unaddressed) work row.
func (s *pgStore) QueuePush(ctx context.Context, name string, groupID sql.NullInt64, priority, workType uint64, work any, carrier []byte) error {
	payload, err := json.Marshal(work)
	if err != nil {
		return fmt.Errorf("marshal work: %w", err)
	}
	_, err = s.exec(ctx, `
		INSERT INTO queue (name, priority, item, group_id, type, address, carrier)
		VALUES ($1, $2, $3, $4, $5, NULL, $6)
	`, name, int64(priority), payload, groupID, int64(workType), carrier)
	if err != nil {
		return fmt.Errorf("queue push: %w", err)
	}
	s.queueNotify(queueReadyChannel)
	return nil
}

// QueuePushAddressed inserts a uniquely-addressed row. On a unique
// violation on the address column, returns plqueue.ErrDuplicateAddressedPush
// so rscache's DuplicateMatcher can recognize the dedup.
func (s *pgStore) QueuePushAddressed(ctx context.Context, name string, groupID sql.NullInt64, priority, workType uint64, address string, work any, carrier []byte) error {
	payload, err := json.Marshal(work)
	if err != nil {
		return fmt.Errorf("marshal work: %w", err)
	}
	_, err = s.exec(ctx, `
		INSERT INTO queue (name, priority, item, group_id, type, address, carrier)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, name, int64(priority), payload, groupID, int64(workType), address, carrier)
	if err != nil {
		if isUniqueViolation(err) {
			return plqueue.ErrDuplicateAddressedPush
		}
		return fmt.Errorf("queue push addressed: %w", err)
	}
	s.queueNotify(queueReadyChannel)
	return nil
}

// QueuePop claims one row. Returns sql.ErrNoRows when nothing is available —
// this exact sentinel is what DatabaseQueue.Get checks.
//
// Implementation (single-tx, Postgres CTE + FOR UPDATE):
//  1. Reserve a permit id (INSERT ... RETURNING).
//  2. Claim the highest-priority unclaimed row (UPDATE ... WHERE id = (SELECT ... FOR UPDATE SKIP LOCKED)).
//  3. Return its payload + permit.
//
// Uses SKIP LOCKED so concurrent workers don't block each other. The example's
// commented-out Postgres branch uses the same pattern.
func (s *pgStore) QueuePop(ctx context.Context, name string, maxPriority uint64, types []uint64) (*plqueue.QueueWork, error) {
	// Fast-path idle check outside the transaction (avoids lock churn
	// when the queue is empty). This mirrors the example's cheap-count
	// short-circuit.
	if len(types) == 0 {
		return nil, sql.ErrNoRows
	}
	intTypes := make([]int64, len(types))
	for i, t := range types {
		intTypes[i] = int64(t)
	}

	var cnt int
	err := s.queryRow(ctx, `
		SELECT COUNT(*) FROM queue
		WHERE name = $1 AND priority <= $2 AND permit = 0 AND type = ANY($3::bigint[])
	`, name, int64(maxPriority), intTypes).Scan(&cnt)
	if err != nil {
		return nil, fmt.Errorf("queue pop precount: %w", err)
	}
	if cnt == 0 {
		return nil, sql.ErrNoRows
	}

	// Do the claim inside a transaction so INSERT permit + UPDATE queue
	// is atomic.
	var work plqueue.QueueWork
	beginner := s
	closer := func(err *error) {}
	if s.tx == nil {
		tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			return nil, fmt.Errorf("queue pop begin: %w", err)
		}
		beginner = &pgStore{pool: s.pool, tx: tx, notifier: s.notifier}
		closer = func(err *error) {
			if err != nil && *err != nil {
				_ = tx.Rollback(ctx)
				return
			}
			_ = tx.Commit(ctx)
		}
	}
	var finalErr error
	defer closer(&finalErr)

	var permitID int64
	if err := beginner.queryRow(ctx, `
		INSERT INTO queue_permits DEFAULT VALUES RETURNING id
	`).Scan(&permitID); err != nil {
		finalErr = fmt.Errorf("queue pop permit: %w", err)
		return nil, finalErr
	}

	var (
		addr    sql.NullString
		wtype   int64
		item    []byte
		carrier []byte
	)
	err = beginner.queryRow(ctx, `
		UPDATE queue
		SET permit = $1, updated_at = NOW()
		WHERE id = (
			SELECT id FROM queue
			WHERE name = $2 AND priority <= $3 AND permit = 0 AND type = ANY($4::bigint[])
			ORDER BY priority ASC, id ASC
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING COALESCE(address, ''), type, item, COALESCE(carrier, '\x'::bytea)
	`, permitID, name, int64(maxPriority), intTypes).Scan(&addr, &wtype, &item, &carrier)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Another worker took the last available row between our
			// precount and the claim. Release the reserved permit.
			_, _ = beginner.exec(ctx, `DELETE FROM queue_permits WHERE id = $1`, permitID)
			finalErr = sql.ErrNoRows
			return nil, finalErr
		}
		finalErr = fmt.Errorf("queue pop claim: %w", err)
		return nil, finalErr
	}

	work = plqueue.QueueWork{
		Permit:   permit.Permit(uint64(permitID)),
		Address:  addr.String,
		WorkType: uint64(wtype),
		Work:     item,
		Carrier:  carrier,
	}
	return &work, nil
}

// QueueDelete removes the claimed row and its permit. Called by the agent
// on successful completion.
func (s *pgStore) QueueDelete(ctx context.Context, p permit.Permit) error {
	if _, err := s.exec(ctx, `DELETE FROM queue WHERE permit = $1`, int64(p)); err != nil {
		return fmt.Errorf("queue delete row: %w", err)
	}
	if _, err := s.exec(ctx, `DELETE FROM queue_permits WHERE id = $1`, int64(p)); err != nil {
		return fmt.Errorf("queue delete permit: %w", err)
	}
	// Notify so waiting workers re-check capacity.
	s.queueNotify(queueReadyChannel)
	return nil
}

// QueuePermits returns all active permits for a queue. Used by leader
// reconciliation paths; benchmark returns an empty set.
func (s *pgStore) QueuePermits(ctx context.Context, name string) ([]plqueue.QueuePermit, error) {
	return []plqueue.QueuePermit{}, nil
}

// QueuePermitDelete removes a permit (used during reconciliation).
func (s *pgStore) QueuePermitDelete(ctx context.Context, p permit.Permit) error {
	_, err := s.exec(ctx, `DELETE FROM queue_permits WHERE id = $1`, int64(p))
	return err
}

// QueuePeek returns unclaimed work snapshots for the given types.
func (s *pgStore) QueuePeek(ctx context.Context, types ...uint64) ([]plqueue.QueueWork, error) {
	if len(types) == 0 {
		return []plqueue.QueueWork{}, nil
	}
	intTypes := make([]int64, len(types))
	for i, t := range types {
		intTypes[i] = int64(t)
	}
	rows, err := s.query(ctx, `
		SELECT COALESCE(address, ''), type, item
		FROM queue
		WHERE permit = 0 AND type = ANY($1::bigint[])
	`, intTypes)
	if err != nil {
		return nil, fmt.Errorf("queue peek: %w", err)
	}
	defer rows.Close()
	var out []plqueue.QueueWork
	for rows.Next() {
		var (
			addr  sql.NullString
			wtype int64
			item  []byte
		)
		if err := rows.Scan(&addr, &wtype, &item); err != nil {
			return nil, fmt.Errorf("queue peek scan: %w", err)
		}
		out = append(out, plqueue.QueueWork{
			Address:  addr.String,
			WorkType: uint64(wtype),
			Work:     item,
		})
	}
	return out, rows.Err()
}

// IsQueueAddressComplete returns true only when no queue row exists for
// the address. Surfaces persisted queue_failure errors as returned errors.
func (s *pgStore) IsQueueAddressComplete(ctx context.Context, address string) (bool, error) {
	// First check for persisted failure.
	var errText sql.NullString
	err := s.queryRow(ctx, `SELECT error FROM queue_failure WHERE address = $1 LIMIT 1`, address).Scan(&errText)
	switch {
	case err == nil && errText.Valid:
		// Return a queue.QueueError so PollAddress surfaces it as a typed error.
		var qe plqueue.QueueError
		if jerr := json.Unmarshal([]byte(errText.String), &qe); jerr == nil && qe.Message != "" {
			return true, &qe
		}
		return true, errors.New(errText.String)
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return false, fmt.Errorf("queue is-complete failure check: %w", err)
	}

	// Then check if the row is still present.
	var cnt int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM queue WHERE address = $1`, address).Scan(&cnt); err != nil {
		return false, fmt.Errorf("queue is-complete row check: %w", err)
	}
	return cnt == 0, nil
}

// IsQueueAddressInProgress returns true if the address is currently in the queue.
func (s *pgStore) IsQueueAddressInProgress(ctx context.Context, address string) (bool, error) {
	var cnt int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM queue WHERE address = $1`, address).Scan(&cnt); err != nil {
		return false, fmt.Errorf("queue in-progress check: %w", err)
	}
	return cnt > 0, nil
}

// QueueAddressedComplete records or clears an addressed-work failure.
// Called by the agent after a runner completes (nil clears; non-nil records).
func (s *pgStore) QueueAddressedComplete(ctx context.Context, address string, failure error) error {
	// Always clear any existing failure first so we never leave both an
	// old and a new row.
	if _, err := s.exec(ctx, `DELETE FROM queue_failure WHERE address = $1`, address); err != nil {
		return fmt.Errorf("queue addressed-complete clear: %w", err)
	}
	if failure == nil {
		return nil
	}
	qe := plqueue.QueueError{Message: failure.Error()}
	raw, err := json.Marshal(qe)
	if err != nil {
		return fmt.Errorf("queue addressed-complete marshal: %w", err)
	}
	if _, err := s.exec(ctx, `INSERT INTO queue_failure (address, error) VALUES ($1, $2)`, address, string(raw)); err != nil {
		return fmt.Errorf("queue addressed-complete insert: %w", err)
	}
	return nil
}

// queueNotify enqueues (during transactions) or sends (outside transactions)
// a "work ready" notification. The payload carries the queue-ready NotifyType
// so the broadcaster's matcher routes it to the DatabaseQueue's internal
// subscribers.
func (s *pgStore) queueNotify(channel string) {
	note := queueDbNote{NotifyType: notifyTypeQueue}
	if s.tx != nil {
		s.pendingNotifications = append(s.pendingNotifications, pendingNote{channel: channel, payload: note})
		return
	}
	if s.notifier != nil {
		_ = s.notifier.Notify(channel, note)
	}
}

// isUniqueViolation returns true if err is a Postgres 23505 (unique_violation).
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return true
	}
	// Defensive text match in case the error type is wrapped further.
	return strings.Contains(err.Error(), "SQLSTATE 23505")
}
