package postgres

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/PBelle451/backend-challenge-go/internal/domain/wager"
)

// InboxRepo deduplica mensagens recebidas (SQS) por (consumidor, messageId).
type InboxRepo struct{ q DBTX }

// InboxMessage identifica uma mensagem recebida por um consumidor.
type InboxMessage struct {
	ID           uuid.UUID
	ConsumerName string
	MessageID    string
	PayloadHash  wager.PayloadHash
	ReceivedAt   time.Time
	// CompletedAt: na entrada por SQS a linha é gravada JÁ CONCLUÍDA, na mesma
	// transação SQL do efeito de domínio. Como tudo é confirmado junto, a existência
	// da linha significa "efeito aplicado". Zero deixa a coluna NULL.
	CompletedAt time.Time
}

// InboxOutcome é o resultado de registrar uma mensagem.
type InboxOutcome string

const (
	// InboxNew: primeira vez; o chamador deve aplicar o efeito na MESMA transação.
	InboxNew InboxOutcome = "NEW"
	// InboxDuplicate: já registrada com o mesmo conteúdo; não reprocessar (o efeito já
	// foi confirmado), apenas remover a mensagem da fila.
	InboxDuplicate InboxOutcome = "DUPLICATE"
	// InboxHashMismatch: o mesmo messageId voltou com conteúdo diferente: anomalia
	// (reuso indevido de identificador). Não reprocessar; tratar como mensagem inválida.
	InboxHashMismatch InboxOutcome = "HASH_MISMATCH"
)

// Register grava a mensagem com INSERT ... ON CONFLICT DO NOTHING.
//
// Duas instâncias que recebem o mesmo messageId ao mesmo tempo: a segunda espera a
// primeira terminar; se ela confirmar, a segunda vê o conflito e devolve
// InboxDuplicate; se desfizer, a segunda grava e processa. Assim o efeito de domínio
// é aplicado no máximo uma vez, mesmo com entrega duplicada ou concorrente.
func (r *InboxRepo) Register(ctx context.Context, m InboxMessage) (InboxOutcome, error) {
	if m.ID == uuid.Nil || m.ConsumerName == "" || m.MessageID == "" || m.PayloadHash.IsZero() || m.ReceivedAt.IsZero() {
		return "", fmt.Errorf("postgres: inbox message requires id, consumer, messageId, payloadHash and receivedAt")
	}
	tag, err := r.q.Exec(ctx,
		`INSERT INTO inbox_messages (id, consumer_name, message_id, payload_hash, received_at, completed_at)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (consumer_name, message_id) DO NOTHING`,
		pgUUID(m.ID), m.ConsumerName, m.MessageID, m.PayloadHash[:], m.ReceivedAt.UTC(), pgTime(m.CompletedAt))
	if err != nil {
		return "", mapError(err)
	}
	if tag.RowsAffected() == 1 {
		return InboxNew, nil
	}

	var stored []byte
	if err := r.q.QueryRow(ctx,
		`SELECT payload_hash FROM inbox_messages WHERE consumer_name = $1 AND message_id = $2`,
		m.ConsumerName, m.MessageID).Scan(&stored); err != nil {
		return "", err
	}
	if bytes.Equal(stored, m.PayloadHash[:]) {
		return InboxDuplicate, nil
	}
	return InboxHashMismatch, nil
}
