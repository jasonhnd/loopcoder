package admission

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

func ensureTx(ctx context.Context, tx *sql.Tx) error {
	for _, stmt := range ddl {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("admission ensure: %w", err)
		}
	}
	return nil
}

func insertReservation(ctx context.Context, tx *sql.Tx, r Reservation) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO admission_reservations(
		reservation_id, generation, project_id, job_id, attempt_id, role, state,
		processes, rss_bytes, cpu_rate, lease_until, idempotency_key, created_at, updated_at, attention_reason
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.Generation, r.ProjectID, r.JobID, r.AttemptID, string(r.Role), string(r.State),
		r.Processes, r.RSSBytes, r.CPURate, formatTime(r.LeaseUntil), r.IdempotencyKey,
		formatTime(r.CreatedAt), formatTime(r.UpdatedAt), r.AttentionReason,
	)
	if err != nil {
		return fmt.Errorf("admission insert: %w", err)
	}
	return nil
}

func updateReservation(ctx context.Context, tx *sql.Tx, r Reservation) error {
	_, err := tx.ExecContext(ctx, `UPDATE admission_reservations SET
		generation=?, state=?, processes=?, rss_bytes=?, cpu_rate=?, lease_until=?,
		updated_at=?, attention_reason=?
		WHERE reservation_id=?`,
		r.Generation, string(r.State), r.Processes, r.RSSBytes, r.CPURate,
		formatTime(r.LeaseUntil), formatTime(r.UpdatedAt), r.AttentionReason, r.ID,
	)
	if err != nil {
		return fmt.Errorf("admission update: %w", err)
	}
	return nil
}

func loadByID(ctx context.Context, tx *sql.Tx, id string) (Reservation, bool, error) {
	row := tx.QueryRowContext(ctx, `SELECT reservation_id, generation, project_id, job_id, attempt_id, role, state,
		processes, rss_bytes, cpu_rate, lease_until, idempotency_key, created_at, updated_at, attention_reason
		FROM admission_reservations WHERE reservation_id=?`, id)
	res, err := scanResRow(row)
	if err == sql.ErrNoRows {
		return Reservation{}, false, nil
	}
	if err != nil {
		return Reservation{}, false, err
	}
	return res, true, nil
}

func loadByIdempotency(ctx context.Context, tx *sql.Tx, key string) (Reservation, bool, error) {
	row := tx.QueryRowContext(ctx, `SELECT reservation_id, generation, project_id, job_id, attempt_id, role, state,
		processes, rss_bytes, cpu_rate, lease_until, idempotency_key, created_at, updated_at, attention_reason
		FROM admission_reservations WHERE idempotency_key=?`, key)
	res, err := scanResRow(row)
	if err == sql.ErrNoRows {
		return Reservation{}, false, nil
	}
	if err != nil {
		return Reservation{}, false, err
	}
	return res, true, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanResRow(row rowScanner) (Reservation, error) {
	var r Reservation
	var role, state, lease, created, updated string
	err := row.Scan(
		&r.ID, &r.Generation, &r.ProjectID, &r.JobID, &r.AttemptID, &role, &state,
		&r.Processes, &r.RSSBytes, &r.CPURate, &lease, &r.IdempotencyKey, &created, &updated, &r.AttentionReason,
	)
	if err != nil {
		return Reservation{}, err
	}
	r.Role = Role(role)
	r.State = State(state)
	r.LeaseUntil = parseTime(lease)
	r.CreatedAt = parseTime(created)
	r.UpdatedAt = parseTime(updated)
	return r, nil
}

type rowsIface interface {
	Scan(dest ...any) error
}

func scanRes(rows rowsIface) (Reservation, error) {
	return scanResRow(rows)
}

func sumActive(ctx context.Context, tx *sql.Tx, now time.Time) (ResourceView, error) {
	_ = now
	rows, err := tx.QueryContext(ctx, `SELECT role, processes, rss_bytes, cpu_rate
		FROM admission_reservations WHERE state='active'`)
	if err != nil {
		return ResourceView{}, err
	}
	defer rows.Close()
	var v ResourceView
	for rows.Next() {
		var role string
		var proc int
		var rss int64
		var cpu float64
		if err := rows.Scan(&role, &proc, &rss, &cpu); err != nil {
			return ResourceView{}, err
		}
		v.Processes += proc
		v.RSSBytes += rss
		v.CPURate += cpu
		switch Role(role) {
		case RoleWorker:
			v.Workers++
		case RoleVerifier:
			v.Verifiers++
		case RoleLocalTest:
			v.LocalTests++
		}
	}
	return v, rows.Err()
}

func sumAttention(ctx context.Context, tx *sql.Tx) (ResourceView, error) {
	rows, err := tx.QueryContext(ctx, `SELECT role, processes, rss_bytes, cpu_rate
		FROM admission_reservations WHERE state='attention_required'`)
	if err != nil {
		return ResourceView{}, err
	}
	defer rows.Close()
	var v ResourceView
	for rows.Next() {
		var role string
		var proc int
		var rss int64
		var cpu float64
		if err := rows.Scan(&role, &proc, &rss, &cpu); err != nil {
			return ResourceView{}, err
		}
		v.Processes += proc
		v.RSSBytes += rss
		v.CPURate += cpu
		switch Role(role) {
		case RoleWorker:
			v.Workers++
		case RoleVerifier:
			v.Verifiers++
		case RoleLocalTest:
			v.LocalTests++
		}
	}
	return v, rows.Err()
}

func insertEnforcementOnce(ctx context.Context, tx *sql.Tx, reservationID, key string, metric Metric, observed, threshold float64, now time.Time) (EnforcementRequest, bool, error) {
	id := fmt.Sprintf("enf_%s_%s", reservationID, key)
	if len(id) > 120 {
		id = id[:120]
	}
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO admission_enforcement_requests(
		request_id, reservation_id, transition_key, metric, observed, threshold, created_at
	) VALUES (?,?,?,?,?,?,?)`,
		id, reservationID, key, string(metric), observed, threshold, formatTime(now),
	)
	if err != nil {
		return EnforcementRequest{}, false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return EnforcementRequest{}, false, nil
	}
	return EnforcementRequest{
		ID:            id,
		ReservationID: reservationID,
		TransitionKey: key,
		Metric:        metric,
		Observed:      observed,
		Threshold:     threshold,
		CreatedAt:     now,
	}, true, nil
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t, _ = time.Parse(time.RFC3339, s)
	}
	return t
}
