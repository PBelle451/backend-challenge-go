package postgres

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/PBelle451/backend-challenge-go/internal/domain/money"
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

// LedgerSummary resume o ledger de uma carteira para a reconciliação.
type LedgerSummary struct {
	Currency    money.Currency
	Credits     money.Money // soma de todos os créditos
	Debits      money.Money // soma de todos os débitos
	EntryCount  int64
	LastVersion int64 // maior wallet_version lançada (0 se não houver lançamentos)
}

// Summary calcula, no banco, os totais do ledger da carteira. As somas são feitas em
// numeric (sem overflow) e só convertidas para Money depois de checadas; se um total
// ultrapassar o limite de Money, devolve ErrIntegrity em vez de um valor errado.
func (r *LedgerRepo) Summary(ctx context.Context, walletID uuid.UUID) (LedgerSummary, error) {
	var (
		currency, credits, debits string
		count, lastVersion        int64
	)
	err := r.q.QueryRow(ctx,
		`SELECT w.currency,
		        COALESCE(SUM(l.amount) FILTER (WHERE l.direction = 'CREDIT'), 0)::text,
		        COALESCE(SUM(l.amount) FILTER (WHERE l.direction = 'DEBIT'), 0)::text,
		        COUNT(l.id),
		        COALESCE(MAX(l.wallet_version), 0)
		   FROM wallets w
		   LEFT JOIN wallet_ledger_entries l ON l.wallet_id = w.id
		  WHERE w.id = $1
		  GROUP BY w.currency`, pgUUID(walletID)).Scan(&currency, &credits, &debits, &count, &lastVersion)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return LedgerSummary{}, ErrNotFound
		}
		return LedgerSummary{}, err
	}
	cr, err := moneyFromNumeric(credits, currency)
	if err != nil {
		return LedgerSummary{}, err
	}
	db, err := moneyFromNumeric(debits, currency)
	if err != nil {
		return LedgerSummary{}, err
	}
	return LedgerSummary{Currency: money.Currency(currency), Credits: cr, Debits: db, EntryCount: count, LastVersion: lastVersion}, nil
}

func moneyFromNumeric(text, currency string) (money.Money, error) {
	n, ok := new(big.Int).SetString(text, 10)
	if !ok || !n.IsInt64() {
		return money.Money{}, fmt.Errorf("%w: ledger total %q exceeds the supported range", ErrIntegrity, text)
	}
	return moneyFrom(n.Int64(), currency)
}

// FindDivergentWallets devolve até limit carteiras cujo saldo ou versão NÃO coincide
// com o que o ledger conta. Em um banco íntegro o resultado é sempre vazio: o schema
// confere isso a cada commit. Serve de verificação independente (reconciliação em lote,
// health checks e testes).
func (r *LedgerRepo) FindDivergentWallets(ctx context.Context, limit int) ([]uuid.UUID, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("postgres: limit must be positive")
	}
	rows, err := r.q.Query(ctx,
		`SELECT w.id
		   FROM wallets w
		   LEFT JOIN LATERAL (
		        SELECT COALESCE(SUM(CASE l.direction WHEN 'CREDIT' THEN l.amount ELSE -l.amount END), 0) AS net,
		               MAX(l.wallet_version) AS last_version
		          FROM wallet_ledger_entries l WHERE l.wallet_id = w.id) s ON TRUE
		  WHERE w.balance <> s.net OR w.version <> COALESCE(s.last_version, 1)
		  ORDER BY w.id
		  LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id pgtype.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, fromPgUUID(id))
	}
	return out, rows.Err()
}
