// Package postgres implementa a persistência em PostgreSQL com pgx e SQL explícito.
//
// Modelo de transação: cada caso de uso roda dentro de UMA transação SQL (TxManager.Do),
// em READ COMMITTED, com lock pessimista por carteira (SELECT ... FOR UPDATE) e
// lock_timeout. Domínio, ledger, inbox e outbox são gravados juntos e confirmados
// juntos; o schema reforça as invariantes financeiras no commit.
package migrations

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/PBelle451/backend-challenge-go/internal/infra/postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DBTX é o que os repositórios precisam: satisfeito por *pgxpool.Pool e por pgx.Tx.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Config reúne os parâmetros da conexão e das transações.
type Config struct {
	URL              string
	MaxConns         int32
	MinConns         int32
	ConnectTimeout   time.Duration
	LockTimeout      time.Duration // por transação (SET LOCAL lock_timeout)
	StatementTimeout time.Duration // por transação (SET LOCAL statement_timeout)
	MaxTxRetries     int           // repetições após deadlock/falha de serialização
}

func (c *Config) applyDefaults() {
	if c.MaxConns == 0 {
		c.MaxConns = 20
	}
	if c.ConnectTimeout == 0 {
		c.ConnectTimeout = 5 * time.Second
	}
	if c.LockTimeout == 0 {
		c.LockTimeout = 5 * time.Second
	}
	if c.StatementTimeout == 0 {
		c.StatementTimeout = 15 * time.Second
	}
	if c.MaxTxRetries == 0 {
		c.MaxTxRetries = 3
	}
}

// NewPool abre o pool de conexões e confirma que o banco responde.
func NewPool(ctx context.Context, cfg Config) (*pgxpool.Pool, error) {
	cfg.applyDefaults()
	pc, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse url: %w", err)
	}
	pc.MaxConns = cfg.MaxConns
	pc.MinConns = cfg.MinConns
	pc.ConnConfig.ConnectTimeout = cfg.ConnectTimeout

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("postgres: open pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	return pool, nil
}

// Repositories reúne os repositórios ligados à mesma transação (ou ao pool, para leituras).
type Repositories struct {
	Wallets      *postgres.WalletRepo
	Transactions *postgres.TransactionRepo
	Ledger       *postgres.LedgerRepo
}

// NewRepositories liga os repositórios a q.
func NewRepositories(q DBTX) Repositories {
	return Repositories{
		Wallets:      &postgres.WalletRepo{q: q},
		Transactions: &postgres.TransactionRepo{q: q},
		Ledger:       &postgres.LedgerRepo{q: q},
	}
}

// TxManager delimita transações SQL.
type TxManager struct {
	pool *pgxpool.Pool
	cfg  Config
}

// NewTxManager cria o gerenciador de transações sobre o pool.
func NewTxManager(pool *pgxpool.Pool, cfg Config) *TxManager {
	cfg.applyDefaults()
	return &TxManager{pool: pool, cfg: cfg}
}

// Reader devolve repositórios ligados ao pool, para leituras fora de transação.
func (m *TxManager) Reader() Repositories { return NewRepositories(m.pool) }

// Do executa fn em uma transação SQL: confirma se fn devolve nil e desfaz caso contrário.
//
// Se o servidor desfizer a transação por deadlock (40P01) ou falha de serialização
// (40001), fn é executada novamente, até MaxTxRetries vezes, com backoff curto e
// jitter. Por isso fn deve ser reentrante: só pode ter efeitos dentro do banco.
// Qualquer outro erro é devolvido sem repetir (use IsTransient para decidir o retry
// de nível superior).
func (m *TxManager) Do(ctx context.Context, fn func(ctx context.Context, r Repositories) error) error {
	var err error
	for attempt := 0; ; attempt++ {
		err = m.once(ctx, fn)
		if err == nil || !postgres.isRetryableTx(err) {
			return err
		}
		if attempt >= m.cfg.MaxTxRetries {
			return fmt.Errorf("postgres: transaction retries exhausted: %w", err)
		}
		wait := time.Duration(attempt+1)*10*time.Millisecond + time.Duration(rand.Intn(10))*time.Millisecond
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (m *TxManager) once(ctx context.Context, fn func(ctx context.Context, r Repositories) error) (err error) {
	tx, err := m.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			// o rollback precisa acontecer mesmo que ctx já tenha sido cancelado
			rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if rbErr := tx.Rollback(rbCtx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) && err == nil {
				err = rbErr
			}
		}
	}()

	// SET LOCAL vale só para esta transação. Os valores são inteiros em ms (sem injeção).
	if _, err = tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = %d; SET LOCAL statement_timeout = %d",
		m.cfg.LockTimeout.Milliseconds(), m.cfg.StatementTimeout.Milliseconds())); err != nil {
		return err
	}
	if err = fn(ctx, NewRepositories(tx)); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return postgres.mapError(err) // violações das checagens do commit (ledger x carteira) chegam aqui
	}
	committed = true
	return nil
}
