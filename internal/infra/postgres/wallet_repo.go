package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/PBelle451/backend-challenge-go/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/PBelle451/backend-challenge-go/internal/domain/wallet"
)

// WalletRepo persiste carteiras.
type WalletRepo struct{ q migrations.DBTX }

const walletColumns = `id, player_id, currency, balance, version, created_at, updated_at`

func scanWallet(row pgx.Row) (*wallet.Wallet, error) {
	var (
		id, playerID         pgtype.UUID
		currency             string
		balance, version     int64
		createdAt, updatedAt time.Time
	)
	if err := row.Scan(&id, &playerID, &currency, &balance, &version, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	bal, err := moneyFrom(balance, currency)
	if err != nil {
		return nil, fmt.Errorf("postgres: wallet %s has an invalid balance: %w", fromPgUUID(id), err)
	}
	return wallet.Rehydrate(wallet.RehydrateParams{
		ID: fromPgUUID(id), PlayerID: fromPgUUID(playerID), Balance: bal, Version: version,
		CreatedAt: createdAt, UpdatedAt: updatedAt,
	})
}

// Insert grava uma carteira nova (versão 1, ainda não persistida). Deve acontecer
// na mesma transação da transação OPENING e do lançamento de abertura, quando o
// saldo inicial é positivo: o schema confere saldo x ledger no commit.
func (r *WalletRepo) Insert(ctx context.Context, w *wallet.Wallet) error {
	if w.LoadedVersion() != 0 {
		return fmt.Errorf("postgres: wallet %s is already persisted", w.ID())
	}
	_, err := r.q.Exec(ctx,
		`INSERT INTO wallets (`+walletColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		pgUUID(w.ID()), pgUUID(w.PlayerID()), string(w.Currency()), w.Balance().Minor(), w.Version(),
		w.CreatedAt(), w.UpdatedAt())
	if err != nil {
		return mapError(err)
	}
	w.MarkPersisted()
	return nil
}

// GetByID lê uma carteira sem travar a linha (leituras e consultas).
func (r *WalletRepo) GetByID(ctx context.Context, id uuid.UUID) (*wallet.Wallet, error) {
	return scanWallet(r.q.QueryRow(ctx, `SELECT `+walletColumns+` FROM wallets WHERE id = $1`, pgUUID(id)))
}

// GetForUpdate lê a carteira TRAVANDO a linha até o fim da transação. É a
// coordenação entre escritores concorrentes: o segundo escritor espera o primeiro
// confirmar e então enxerga o saldo atualizado. Se a espera passar do lock_timeout,
// o erro é transitório (IsTransient).
//
// Usa FOR NO KEY UPDATE, que exclui outros escritores da mesma forma que FOR UPDATE,
// mas NÃO conflita com o FOR KEY SHARE que o PostgreSQL toma na linha da carteira
// ao inserir transações e lançamentos (checagem de chave estrangeira). Com FOR UPDATE,
// duas transações que já tivessem inserido suas linhas entrariam em deadlock.
func (r *WalletRepo) GetForUpdate(ctx context.Context, id uuid.UUID) (*wallet.Wallet, error) {
	return scanWallet(r.q.QueryRow(ctx, `SELECT `+walletColumns+` FROM wallets WHERE id = $1 FOR NO KEY UPDATE`, pgUUID(id)))
}

// GetByPlayer busca a carteira de um jogador em uma moeda.
func (r *WalletRepo) GetByPlayer(ctx context.Context, playerID uuid.UUID, currency string) (*wallet.Wallet, error) {
	return scanWallet(r.q.QueryRow(ctx,
		`SELECT `+walletColumns+` FROM wallets WHERE player_id = $1 AND currency = $2`, pgUUID(playerID), currency))
}

// Save grava a alteração de saldo/versão feita pelo agregado, com um UPDATE
// condicional (WHERE version = <versão lida>) como segunda barreira, além do lock
// de linha. Se outro escritor já avançou a versão, devolve ErrVersionConflict.
//
// O schema aceita exatamente UMA movimentação por UPDATE (a versão sobe de 1 em 1);
// o caso de uso aplica uma movimentação por carteira lida.
func (r *WalletRepo) Save(ctx context.Context, w *wallet.Wallet) error {
	if w.LoadedVersion() == 0 {
		return fmt.Errorf("postgres: wallet %s was never persisted: use Insert", w.ID())
	}
	if w.Version() == w.LoadedVersion() {
		return nil // nada mudou
	}
	tag, err := r.q.Exec(ctx,
		`UPDATE wallets SET balance = $2, version = $3, updated_at = $4 WHERE id = $1 AND version = $5`,
		pgUUID(w.ID()), w.Balance().Minor(), w.Version(), w.UpdatedAt(), w.LoadedVersion())
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: wallet %s was read at version %d", ErrVersionConflict, w.ID(), w.LoadedVersion())
	}
	w.MarkPersisted()
	return nil
}
