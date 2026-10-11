package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/PBelle451/backend-challenge-go/internal/domain/wallet"
)

// LedgerRepo persiste os lançamentos do ledger (append-only).
type LedgerRepo struct{ q DBTX }

const ledgerColumns = `id, wallet_id, transaction_id, direction, currency, amount, balance_before, balance_after, wallet_version, created_at`

// Insert grava um lançamento. Só existe INSERT: o schema bloqueia UPDATE, DELETE e TRUNCATE.
func (r *LedgerRepo) Insert(ctx context.Context, e wallet.LedgerEntry) error {
	_, err := r.q.Exec(ctx,
		`INSERT INTO wallet_ledger_entries (`+ledgerColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		pgUUID(e.ID()), pgUUID(e.WalletID()), pgUUID(e.TransactionID()), string(e.Direction()),
		string(e.Amount().Currency()), e.Amount().Minor(), e.BalanceBefore().Minor(), e.BalanceAfter().Minor(),
		e.WalletVersion(), e.CreatedAt())
	return mapError(err)
}

// List devolve os lançamentos de uma carteira em ordem crescente de versão, a
// partir de afterVersion (exclusivo). A versão serve de cursor estável de
// paginação: o próximo cursor é a WalletVersion do último item devolvido.
func (r *LedgerRepo) List(ctx context.Context, walletID uuid.UUID, afterVersion int64, limit int) ([]wallet.LedgerEntry, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("postgres: ledger list limit must be positive")
	}
	rows, err := r.q.Query(ctx,
		`SELECT `+ledgerColumns+` FROM wallet_ledger_entries
		  WHERE wallet_id = $1 AND wallet_version > $2
		  ORDER BY wallet_version ASC LIMIT $3`,
		pgUUID(walletID), afterVersion, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []wallet.LedgerEntry
	for rows.Next() {
		var (
			id, wid, txid                  pgtype.UUID
			direction, currency            string
			amount, before, after, version int64
			createdAt                      time.Time
		)
		if err := rows.Scan(&id, &wid, &txid, &direction, &currency, &amount, &before, &after, &version, &createdAt); err != nil {
			return nil, err
		}
		am, err := moneyFrom(amount, currency)
		if err != nil {
			return nil, err
		}
		bb, err := moneyFrom(before, currency)
		if err != nil {
			return nil, err
		}
		ba, err := moneyFrom(after, currency)
		if err != nil {
			return nil, err
		}
		entry, err := wallet.RehydrateLedgerEntry(wallet.LedgerEntryParams{
			ID: fromPgUUID(id), WalletID: fromPgUUID(wid), TransactionID: fromPgUUID(txid),
			Direction: wallet.Direction(direction), Amount: am, BalanceBefore: bb, BalanceAfter: ba,
			WalletVersion: version, CreatedAt: createdAt,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}
