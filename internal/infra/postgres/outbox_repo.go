package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// OutboxRepo grava eventos junto do fato que os originou e coordena os publishers.
type OutboxRepo struct{ q DBTX }

const maxLastErrorLength = 1000

// OutboxEvent é um evento de saída. O envelope completo é remontado a partir destes
// campos; Payload é o "data" do envelope, um objeto JSON imutável após a gravação.
type OutboxEvent struct {
	EventID       uuid.UUID
	Seq           int64 // atribuído pelo banco; ordena a publicação
	EventType     string
	EventVersion  int
	AggregateType string
	AggregateID   uuid.UUID
	CorrelationID string
	CausationID   string
	Payload       json.RawMessage
	OccurredAt    time.Time
	CreatedAt     time.Time

	// Estado de publicação.
	Attempts      int
	NextAttemptAt time.Time // na gravação, zero = disponível imediatamente (CreatedAt)
	LockedBy      string
	LockedUntil   time.Time
	LastError     string
	PublishedAt   time.Time
}

const outboxColumns = `event_id, seq, event_type, event_version, aggregate_type, aggregate_id,
	correlation_id, causation_id, payload, occurred_at, created_at,
	attempts, next_attempt_at, locked_by, locked_until, last_error, published_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanOutbox(row rowScanner) (OutboxEvent, error) {
	var (
		e                       OutboxEvent
		id, agg                 pgtype.UUID
		version, attempts       int32
		causation, lockedBy, le pgtype.Text
		payload                 []byte
		occurred, created, next time.Time
		lockedUntil, published  pgtype.Timestamptz
	)
	if err := row.Scan(&id, &e.Seq, &e.EventType, &version, &e.AggregateType, &agg,
		&e.CorrelationID, &causation, &payload, &occurred, &created,
		&attempts, &next, &lockedBy, &lockedUntil, &le, &published); err != nil {
		return OutboxEvent{}, err
	}
	e.EventID, e.AggregateID = fromPgUUID(id), fromPgUUID(agg)
	e.EventVersion, e.Attempts = int(version), int(attempts)
	e.CausationID, e.LockedBy, e.LastError = causation.String, lockedBy.String, le.String
	e.Payload = json.RawMessage(payload)
	e.OccurredAt, e.CreatedAt, e.NextAttemptAt = occurred.UTC(), created.UTC(), next.UTC()
	e.LockedUntil, e.PublishedAt = fromPgTime(lockedUntil), fromPgTime(published)
	return e, nil
}

// Insert grava um evento. Deve ser chamado na MESMA transação SQL do fato que o
// originou: se a transação for desfeita, o evento desaparece junto; se for confirmada,
// o evento existe e será publicado, mesmo que a instância caia logo em seguida.
func (r *OutboxRepo) Insert(ctx context.Context, e OutboxEvent) error {
	if e.EventID == uuid.Nil || e.AggregateID == uuid.Nil || e.EventType == "" || e.AggregateType == "" ||
		e.CorrelationID == "" || e.EventVersion < 1 || e.OccurredAt.IsZero() || e.CreatedAt.IsZero() {
		return fmt.Errorf("%w: missing required fields", ErrInvalidEvent)
	}
	var obj map[string]json.RawMessage
	if len(e.Payload) == 0 || json.Unmarshal(e.Payload, &obj) != nil || obj == nil {
		return fmt.Errorf("%w: payload must be a JSON object", ErrInvalidEvent)
	}
	next := e.NextAttemptAt
	if next.IsZero() {
		next = e.CreatedAt
	}
	_, err := r.q.Exec(ctx,
		`INSERT INTO outbox_events (event_id, event_type, event_version, aggregate_type, aggregate_id,
		                            correlation_id, causation_id, payload, occurred_at, created_at, next_attempt_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		pgUUID(e.EventID), e.EventType, int32(e.EventVersion), e.AggregateType, pgUUID(e.AggregateID),
		e.CorrelationID, pgText(e.CausationID), []byte(e.Payload), e.OccurredAt.UTC(), e.CreatedAt.UTC(), next.UTC())
	return mapError(err)
}

// Get lê um evento pelo identificador.
func (r *OutboxRepo) Get(ctx context.Context, eventID uuid.UUID) (OutboxEvent, error) {
	e, err := scanOutbox(r.q.QueryRow(ctx, `SELECT `+outboxColumns+` FROM outbox_events WHERE event_id = $1`, pgUUID(eventID)))
	if err != nil && isNoRows(err) {
		return OutboxEvent{}, ErrNotFound
	}
	return e, err
}

// Claim reivindica até limit eventos para publicação por publisherID, em uma única
// instrução (não precisa de transação longa): grava um lease (locked_by/locked_until)
// e conta a tentativa.
//
// Um evento é reivindicável quando: não foi publicado; sua próxima tentativa venceu; não
// há lease ativo (ou o lease venceu, que é trabalho abandonado por um publisher que
// caiu); e TODOS os eventos anteriores do mesmo agregado já foram publicados, o que
// preserva a ordem por agregado. Publishers concorrentes recebem lotes disjuntos
// (FOR NO KEY UPDATE SKIP LOCKED). O resultado vem em ordem de seq.
func (r *OutboxRepo) Claim(ctx context.Context, publisherID string, now time.Time, lease time.Duration, limit int) ([]OutboxEvent, error) {
	if publisherID == "" || lease <= 0 || limit <= 0 {
		return nil, fmt.Errorf("postgres: Claim requires a publisher id, a positive lease and a positive limit")
	}
	rows, err := r.q.Query(ctx,
		`WITH due AS (
		     SELECT e.event_id
		       FROM outbox_events e
		      WHERE e.published_at IS NULL
		        AND e.next_attempt_at <= $1
		        AND (e.locked_until IS NULL OR e.locked_until < $1)
		        AND NOT EXISTS (SELECT 1 FROM outbox_events p
		                         WHERE p.aggregate_id = e.aggregate_id
		                           AND p.published_at IS NULL
		                           AND p.seq < e.seq)
		      ORDER BY e.next_attempt_at, e.seq
		      LIMIT $2
		        FOR NO KEY UPDATE OF e SKIP LOCKED)
		 UPDATE outbox_events o
		    SET locked_by = $3, locked_until = $4, attempts = o.attempts + 1
		   FROM due
		  WHERE o.event_id = due.event_id
		  RETURNING `+prefixed("o", outboxColumns),
		now.UTC(), limit, publisherID, now.UTC().Add(lease))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []OutboxEvent
	for rows.Next() {
		e, err := scanOutbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

// MarkPublished registra a publicação e libera o lease. Só vale para quem ainda é o
// dono do lease: se outro publisher já reivindicou o evento (porque o lease venceu),
// devolve ErrLeaseLost. Nesse caso o evento pode ter sido publicado duas vezes, o que
// é aceitável: o event_id estável e a deduplicação da FIFO absorvem a repetição.
func (r *OutboxRepo) MarkPublished(ctx context.Context, eventID uuid.UUID, publisherID string, now time.Time) error {
	tag, err := r.q.Exec(ctx,
		`UPDATE outbox_events
		    SET published_at = $3, locked_by = NULL, locked_until = NULL, last_error = NULL
		  WHERE event_id = $1 AND locked_by = $2 AND published_at IS NULL`,
		pgUUID(eventID), publisherID, now.UTC())
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: event %s", ErrLeaseLost, eventID)
	}
	return nil
}

// MarkFailed registra uma tentativa que falhou: libera o lease, guarda o erro
// (truncado) e agenda a próxima tentativa em nextAttemptAt (backoff calculado por quem chama).
func (r *OutboxRepo) MarkFailed(ctx context.Context, eventID uuid.UUID, publisherID string, nextAttemptAt time.Time, cause error) error {
	msg := ""
	if cause != nil {
		msg = truncateUTF8(cause.Error(), maxLastErrorLength)
	}
	tag, err := r.q.Exec(ctx,
		`UPDATE outbox_events
		    SET locked_by = NULL, locked_until = NULL, last_error = $3, next_attempt_at = $4
		  WHERE event_id = $1 AND locked_by = $2 AND published_at IS NULL`,
		pgUUID(eventID), publisherID, pgText(msg), nextAttemptAt.UTC())
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: event %s", ErrLeaseLost, eventID)
	}
	return nil
}

// PendingStats alimenta métricas de atraso: quantos eventos aguardam publicação e o
// instante de ocorrência do mais antigo (zero se não houver).
func (r *OutboxRepo) PendingStats(ctx context.Context) (pending int64, oldestOccurredAt time.Time, err error) {
	var oldest pgtype.Timestamptz
	err = r.q.QueryRow(ctx,
		`SELECT count(*), min(occurred_at) FROM outbox_events WHERE published_at IS NULL`).Scan(&pending, &oldest)
	return pending, fromPgTime(oldest), err
}

func isNoRows(err error) bool { return errors.Is(err, errNoRows) }

func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for !utf8.ValidString(s) { // não corta no meio de um caractere
		s = s[:len(s)-1]
	}
	return s
}
