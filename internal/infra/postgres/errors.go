package postgres

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/jackc/pgx/v5/pgconn"
)

// Erros que a camada de persistência devolve ao domínio/aplicação. Cada um embrulha
// o erro original do driver (use errors.Is para classificar).
var (
	ErrNotFound                = errors.New("postgres: not found")
	ErrVersionConflict         = errors.New("postgres: wallet version conflict")
	ErrStateConflict           = errors.New("postgres: transaction is no longer pending")
	ErrWalletExists            = errors.New("postgres: wallet already exists for player and currency")
	ErrWalletMismatch          = errors.New("postgres: wallet does not exist or does not match player and currency")
	ErrDuplicateExternalID     = errors.New("postgres: external transaction id already used by this provider")
	ErrDuplicateIdempotencyKey = errors.New("postgres: idempotency key already used by this provider")
	ErrOpeningExists           = errors.New("postgres: wallet already has an OPENING transaction")
	ErrReversalExists          = errors.New("postgres: referenced transaction already has a processed reversal")
	ErrLedgerEntryExists       = errors.New("postgres: ledger entry already exists for this wallet and transaction")
	ErrIntegrity               = errors.New("postgres: integrity rule violated")
)

// Nomes de constraints do schema (migrations 1 a 4) que viram erros tipados.
const (
	cWalletPlayerCurrency   = "wallets_player_currency_uk"
	cTxProviderExternal     = "wager_tx_provider_external_uk"
	cTxProviderIdempotency  = "wager_tx_provider_idempotency_uk"
	cTxOneOpening           = "wager_tx_one_opening_per_wallet_uk"
	cTxOneReversal          = "wager_tx_one_processed_reversal_per_reference_uk"
	cTxWalletFK             = "wager_tx_wallet_fk"
	cLedgerWalletTx         = "ledger_wallet_transaction_uk"
	cLedgerWalletVersion    = "ledger_wallet_version_uk"
	sqlStateUniqueViolation = "23505"
	sqlStateFKViolation     = "23503"
)

// mapError traduz violações de constraint em erros tipados; qualquer outro erro
// passa intacto. Violações das regras de integridade do schema (CHECK, triggers
// com integrity_constraint_violation) viram ErrIntegrity.
func mapError(err error) error {
	var pe *pgconn.PgError
	if err == nil || !errors.As(err, &pe) {
		return err
	}
	wrap := func(target error) error { return fmt.Errorf("%w: %w", target, err) }

	switch pe.Code {
	case sqlStateUniqueViolation:
		switch pe.ConstraintName {
		case cWalletPlayerCurrency:
			return wrap(ErrWalletExists)
		case cTxProviderExternal:
			return wrap(ErrDuplicateExternalID)
		case cTxProviderIdempotency:
			return wrap(ErrDuplicateIdempotencyKey)
		case cTxOneOpening:
			return wrap(ErrOpeningExists)
		case cTxOneReversal:
			return wrap(ErrReversalExists)
		case cLedgerWalletTx:
			return wrap(ErrLedgerEntryExists)
		case cLedgerWalletVersion:
			return wrap(ErrVersionConflict)
		}
	case sqlStateFKViolation:
		if pe.ConstraintName == cTxWalletFK {
			return wrap(ErrWalletMismatch)
		}
	}
	// 23xxx: restrições de integridade (inclui nossos triggers e CHECKs).
	if len(pe.Code) == 5 && pe.Code[:2] == "23" {
		return wrap(ErrIntegrity)
	}
	return err
}

// IsTransient informa se o erro é uma falha TRANSITÓRIA de infraestrutura, que
// deve ser retomada depois (retry/backoff) sem alterar o estado de negócio:
// conexão perdida ou recusada, timeout, desligamento do servidor, falta de
// recursos, deadlock, falha de serialização e timeout de lock.
//
// Violações de integridade (classe 23) NÃO são transitórias: repetir não adianta.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	if pgconn.Timeout(err) {
		return true
	}
	var ce *pgconn.ConnectError
	if errors.As(err, &ce) {
		return true
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch {
		case len(pe.Code) < 2:
			return false
		case pe.Code[:2] == "08", // connection_exception
			pe.Code[:2] == "53": // insufficient_resources (inclui too_many_connections)
			return true
		case pe.Code == "57P01", pe.Code == "57P02", pe.Code == "57P03", // desligamento / crash / iniciando
			pe.Code == "40001", pe.Code == "40P01", // serialization_failure, deadlock_detected
			pe.Code == "55P03", pe.Code == "57014": // lock_not_available, query_canceled (statement_timeout)
			return true
		}
	}
	return false
}

// isRetryableTx: a transação foi desfeita pelo servidor e pode ser repetida
// imediatamente (deadlock ou falha de serialização).
func isRetryableTx(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && (pe.Code == "40001" || pe.Code == "40P01")
}
