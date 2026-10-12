package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/PBelle451/backend-challenge-go/internal/domain/wager"
)

// TransactionRepo persiste as transações financeiras (WagerTransaction).
type TransactionRepo struct{ q DBTX }

const txColumns = `id, kind, status, wallet_id, player_id, currency, amount,
	provider_id, external_transaction_id, idempotency_key, payload_hash, round_id, game_id,
	reference_external_transaction_id, reference_transaction_id,
	failure_code, result_balance, attempts, next_attempt_at, expires_at,
	created_at, updated_at, finished_at`

func scanTransaction(row pgx.Row) (*wager.Transaction, error) {
	var (
		id, walletID, playerID, refID                   pgtype.UUID
		kind, status, currency                          string
		amount                                          int64
		provider, extID, key, round, game, refExt, fail pgtype.Text
		hash                                            []byte
		resultBalance                                   pgtype.Int8
		attempts                                        int32
		nextAt, expiresAt, finishedAt                   pgtype.Timestamptz
		createdAt, updatedAt                            time.Time
	)
	if err := row.Scan(&id, &kind, &status, &walletID, &playerID, &currency, &amount,
		&provider, &extID, &key, &hash, &round, &game, &refExt, &refID,
		&fail, &resultBalance, &attempts, &nextAt, &expiresAt,
		&createdAt, &updatedAt, &finishedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	amt, err := moneyFrom(amount, currency)
	if err != nil {
		return nil, fmt.Errorf("postgres: transaction %s has an invalid amount: %w", fromPgUUID(id), err)
	}
	p := wager.RehydrateParams{
		ID: fromPgUUID(id), Kind: wager.Kind(kind), Status: wager.Status(status),
		WalletID: fromPgUUID(walletID), PlayerID: fromPgUUID(playerID), Amount: amt,
		ReferenceID: fromPgUUID(refID), FailureCode: wager.FailureCode(fail.String),
		Attempts: int(attempts), NextAttemptAt: fromPgTime(nextAt), ExpiresAt: fromPgTime(expiresAt),
		CreatedAt: createdAt.UTC(), UpdatedAt: updatedAt.UTC(), FinishedAt: fromPgTime(finishedAt),
	}
	if resultBalance.Valid {
		if p.ResultBalance, err = moneyFrom(resultBalance.Int64, currency); err != nil {
			return nil, err
		}
	}
	if provider.Valid { // transações externas
		o := wager.Origin{
			ProviderID: provider.String, ExternalTransactionID: extID.String, IdempotencyKey: key.String,
			RoundID: round.String, GameID: game.String, ReferenceExternalTransactionID: refExt.String,
		}
		if len(hash) != len(o.PayloadHash) {
			return nil, fmt.Errorf("postgres: transaction %s has a payload hash of %d bytes", fromPgUUID(id), len(hash))
		}
		copy(o.PayloadHash[:], hash)
		p.Origin = &o
	}
	return wager.Rehydrate(p)
}

// Insert grava uma transação nova. Violações viram erros tipados:
// ErrDuplicateExternalID, ErrDuplicateIdempotencyKey, ErrOpeningExists e
// ErrWalletMismatch (carteira inexistente ou jogador/moeda divergentes).
func (r *TransactionRepo) Insert(ctx context.Context, t *wager.Transaction) error {
	var (
		provider, extID, key, round, game, refExt pgtype.Text
		hash                                      []byte
	)
	if o, ok := t.Origin(); ok {
		provider, extID, key = pgText(o.ProviderID), pgText(o.ExternalTransactionID), pgText(o.IdempotencyKey)
		round, game, refExt = pgText(o.RoundID), pgText(o.GameID), pgText(o.ReferenceExternalTransactionID)
		hash = append([]byte(nil), o.PayloadHash[:]...)
	}
	refID, _ := t.ReferenceID()
	result, _ := t.ResultBalance()

	_, err := r.q.Exec(ctx,
		`INSERT INTO wager_transactions (`+txColumns+`) VALUES
		 ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)`,
		pgUUID(t.ID()), string(t.Kind()), string(t.Status()), pgUUID(t.WalletID()), pgUUID(t.PlayerID()),
		string(t.Amount().Currency()), t.Amount().Minor(),
		provider, extID, key, hash, round, game, refExt, pgUUID(refID),
		pgText(string(t.FailureCode())), pgMoneyMinor(result), int32(t.Attempts()),
		pgTime(t.NextAttemptAt()), pgTime(t.ExpiresAt()),
		t.CreatedAt(), t.UpdatedAt(), pgTime(t.FinishedAt()))
	return mapError(err)
}

// Save grava a mudança de estado de uma transação ainda pendente. O UPDATE só
// atinge linhas PENDING ou PENDING_REFERENCE: se a transação já terminou (por
// exemplo, outra instância a concluiu), devolve ErrStateConflict.
func (r *TransactionRepo) Save(ctx context.Context, t *wager.Transaction) error {
	refID, _ := t.ReferenceID()
	result, _ := t.ResultBalance()
	tag, err := r.q.Exec(ctx,
		`UPDATE wager_transactions
		    SET status = $2, reference_transaction_id = $3, failure_code = $4, result_balance = $5,
		        attempts = $6, next_attempt_at = $7, expires_at = $8, updated_at = $9, finished_at = $10
		  WHERE id = $1 AND status IN ('PENDING', 'PENDING_REFERENCE')`,
		pgUUID(t.ID()), string(t.Status()), pgUUID(refID), pgText(string(t.FailureCode())), pgMoneyMinor(result),
		int32(t.Attempts()), pgTime(t.NextAttemptAt()), pgTime(t.ExpiresAt()), t.UpdatedAt(), pgTime(t.FinishedAt()))
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", ErrStateConflict, t.ID())
	}
	return nil
}

func (r *TransactionRepo) getOne(ctx context.Context, where, lock string, args ...any) (*wager.Transaction, error) {
	return scanTransaction(r.q.QueryRow(ctx, `SELECT `+txColumns+` FROM wager_transactions WHERE `+where+lock, args...))
}

// GetByID lê uma transação pelo identificador interno.
func (r *TransactionRepo) GetByID(ctx context.Context, id uuid.UUID) (*wager.Transaction, error) {
	return r.getOne(ctx, `id = $1`, ``, pgUUID(id))
}

// GetByProviderExternalID busca pela identidade externa (provedor + id da transação).
func (r *TransactionRepo) GetByProviderExternalID(ctx context.Context, providerID, externalID string) (*wager.Transaction, error) {
	return r.getOne(ctx, `provider_id = $1 AND external_transaction_id = $2`, ``, providerID, externalID)
}

// GetByProviderExternalIDForUpdate faz o mesmo travando a linha (FOR NO KEY UPDATE, que
// não conflita com as chaves estrangeiras de ledger e referências): use ao processar a
// transação ou ao avaliar uma referência que não pode mudar de estado no meio da operação.
func (r *TransactionRepo) GetByProviderExternalIDForUpdate(ctx context.Context, providerID, externalID string) (*wager.Transaction, error) {
	return r.getOne(ctx, `provider_id = $1 AND external_transaction_id = $2`, ` FOR NO KEY UPDATE`, providerID, externalID)
}

// GetByProviderIdempotencyKey busca pela chave de idempotência do provedor.
func (r *TransactionRepo) GetByProviderIdempotencyKey(ctx context.Context, providerID, key string) (*wager.Transaction, error) {
	return r.getOne(ctx, `provider_id = $1 AND idempotency_key = $2`, ``, providerID, key)
}

// HasProcessedReversal informa se a transação já recebeu uma reversão (REFUND ou
// ROLLBACK) PROCESSED. O banco reforça a regra com um índice único; esta consulta
// serve para devolver o failureCode correto antes de tentar gravar.
func (r *TransactionRepo) HasProcessedReversal(ctx context.Context, referenceID uuid.UUID) (bool, error) {
	var exists bool
	err := r.q.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM wager_transactions
		                 WHERE reference_transaction_id = $1 AND status = 'PROCESSED' AND kind IN ('REFUND', 'ROLLBACK'))`,
		pgUUID(referenceID)).Scan(&exists)
	return exists, err
}

// ClaimDue reivindica até limit transações pendentes cuja próxima tentativa já
// venceu, travando-as com FOR NO KEY UPDATE SKIP LOCKED: workers de instâncias diferentes
// recebem lotes DISJUNTOS e nenhum espera pelo outro. As linhas ficam travadas até
// o fim da transação em curso, então o worker deve processar e confirmar na mesma
// transação. Se uma instância cair, a transação SQL é desfeita, o lock some e a
// retomada acontece em outra instância.
func (r *TransactionRepo) ClaimDue(ctx context.Context, now time.Time, limit int) ([]*wager.Transaction, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("postgres: claim limit must be positive")
	}
	rows, err := r.q.Query(ctx,
		`SELECT `+txColumns+` FROM wager_transactions
		  WHERE status IN ('PENDING', 'PENDING_REFERENCE') AND next_attempt_at <= $1
		  ORDER BY next_attempt_at, id
		  LIMIT $2
		  FOR NO KEY UPDATE SKIP LOCKED`,
		now.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*wager.Transaction
	for rows.Next() {
		t, err := scanTransaction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
