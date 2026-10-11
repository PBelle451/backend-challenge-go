// Package wager contém a WagerTransaction e as regras de negócio dos seis tipos de
// transação, a máquina de estados, os códigos de falha e o hash de idempotência.
//
// Máquina de estados (apenas estas transições existem):
//
//	PENDING ──────────────► PROCESSED   (terminal)
//	   │ ──────────────────► REJECTED    (terminal)
//	   │ ──────────────────► FAILED      (terminal)
//	   └─► PENDING_REFERENCE ─► PROCESSED / REJECTED / FAILED
//	PENDING e PENDING_REFERENCE também podem permanecer no mesmo estado ao registrar
//	uma nova tentativa (Retry). Estados terminais não aceitam nenhuma transição.
//
// Falhas transitórias x permanentes:
//   - TRANSITÓRIA (PostgreSQL ou SQS indisponível, deadlock, timeout de lock,
//     conflito de serialização): NÃO muda o estado de negócio. A transação continua
//     PENDING (ou nem foi persistida) e é retomada com backoff (Retry).
//   - REJEIÇÃO DE NEGÓCIO (saldo insuficiente, referência incoerente, reversão
//     duplicada, referência que não chegou a tempo): vai para REJECTED, com um
//     FailureCode estável, e é um desfecho definitivo.
//   - FALHA PERMANENTE de infraestrutura (tentativas esgotadas, erro inesperado
//     irrecuperável): vai para FAILED, com FailureRetriesExhausted ou
//     FailureInternalError, registrada para auditoria.
//
// As transições são atômicas no agregado: se uma transição falha, a transação
// permanece exatamente como estava.
package wager

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/PBelle451/backend-challenge-go/internal/domain/money"
)

// Erros de domínio, classificáveis com errors.Is.
var (
	ErrInvalidKind        = errors.New("wager: invalid kind")
	ErrOpeningNotAllowed  = errors.New("wager: OPENING is reserved for internal wallet opening")
	ErrInvalidAmount      = errors.New("wager: invalid amount")
	ErrInvalidReference   = errors.New("wager: invalid reference")
	ErrInvalidTransition  = errors.New("wager: invalid state transition")
	ErrInvalidTransaction = errors.New("wager: invalid transaction")
)

// Transaction é a WagerTransaction: raiz de uma operação financeira.
type Transaction struct {
	id            uuid.UUID
	kind          Kind
	status        Status
	walletID      uuid.UUID
	playerID      uuid.UUID
	amount        money.Money
	origin        *Origin // nil apenas em OPENING
	referenceID   uuid.UUID
	failureCode   FailureCode
	resultBalance money.Money
	attempts      int
	nextAttemptAt time.Time
	expiresAt     time.Time
	createdAt     time.Time
	updatedAt     time.Time
	finishedAt    time.Time
}

// NewExternalParams reúne os dados de uma operação externa recém-recebida.
type NewExternalParams struct {
	ID       uuid.UUID
	Kind     Kind
	WalletID uuid.UUID
	PlayerID uuid.UUID
	Amount   money.Money
	Origin   Origin
	Now      time.Time
}

// NewExternal cria uma operação externa no estado PENDING. OPENING é rejeitado
// com ErrOpeningNotAllowed: esse tipo só existe por abertura interna de carteira.
func NewExternal(p NewExternalParams) (*Transaction, error) {
	if p.Kind == KindOpening {
		return nil, ErrOpeningNotAllowed
	}
	if !p.Kind.IsExternal() {
		return nil, fmt.Errorf("%w: %q", ErrInvalidKind, p.Kind)
	}
	if p.Now.IsZero() {
		return nil, fmt.Errorf("%w: creation time is required", ErrInvalidTransaction)
	}
	origin := p.Origin
	now := p.Now.UTC()
	t := &Transaction{
		id: p.ID, kind: p.Kind, status: StatusPending,
		walletID: p.WalletID, playerID: p.PlayerID, amount: p.Amount,
		origin: &origin, createdAt: now, updatedAt: now,
		nextAttemptAt: now, // PENDING sempre tem retomada agendada
	}
	if err := t.checkInvariants(); err != nil {
		return nil, err
	}
	return t, nil
}

// NewOpeningParams reúne os dados da abertura interna de uma carteira.
type NewOpeningParams struct {
	ID       uuid.UUID
	WalletID uuid.UUID
	PlayerID uuid.UUID
	Amount   money.Money // saldo inicial, positivo
	Now      time.Time
}

// NewOpening cria a transação OPENING, que nasce PROCESSED e sem metadados externos.
func NewOpening(p NewOpeningParams) (*Transaction, error) {
	if p.Now.IsZero() {
		return nil, fmt.Errorf("%w: creation time is required", ErrInvalidTransaction)
	}
	now := p.Now.UTC()
	t := &Transaction{
		id: p.ID, kind: KindOpening, status: StatusProcessed,
		walletID: p.WalletID, playerID: p.PlayerID, amount: p.Amount,
		resultBalance: p.Amount, createdAt: now, updatedAt: now, finishedAt: now,
	}
	if err := t.checkInvariants(); err != nil {
		return nil, err
	}
	return t, nil
}

// RehydrateParams reúne o estado persistido de uma transação.
type RehydrateParams struct {
	ID            uuid.UUID
	Kind          Kind
	Status        Status
	WalletID      uuid.UUID
	PlayerID      uuid.UUID
	Amount        money.Money
	Origin        *Origin // nil em OPENING
	ReferenceID   uuid.UUID
	FailureCode   FailureCode
	ResultBalance money.Money
	Attempts      int
	NextAttemptAt time.Time
	ExpiresAt     time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
	FinishedAt    time.Time
}

// Rehydrate reconstrói uma transação persistida. Não executa transições, não
// reaplica movimentações e não produz eventos; apenas revalida as invariantes.
func Rehydrate(p RehydrateParams) (*Transaction, error) {
	t := &Transaction{
		id: p.ID, kind: p.Kind, status: p.Status,
		walletID: p.WalletID, playerID: p.PlayerID, amount: p.Amount,
		referenceID: p.ReferenceID, failureCode: p.FailureCode, resultBalance: p.ResultBalance,
		attempts:      p.Attempts,
		nextAttemptAt: utc(p.NextAttemptAt), expiresAt: utc(p.ExpiresAt),
		createdAt: utc(p.CreatedAt), updatedAt: utc(p.UpdatedAt), finishedAt: utc(p.FinishedAt),
	}
	if p.Origin != nil {
		o := *p.Origin
		t.origin = &o
	}
	if err := t.checkInvariants(); err != nil {
		return nil, err
	}
	return t, nil
}

func utc(t time.Time) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	return t.UTC()
}

// inv monta um erro de invariante que satisfaz ErrInvalidTransaction e, quando
// informado, também o erro específico.
func inv(specific error, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if specific == nil {
		return fmt.Errorf("%w: %s", ErrInvalidTransaction, msg)
	}
	return fmt.Errorf("%w: %w: %s", ErrInvalidTransaction, specific, msg)
}

// checkInvariants espelha, no domínio, as regras que o schema também impõe.
func (t *Transaction) checkInvariants() error {
	if t.id == uuid.Nil || t.walletID == uuid.Nil || t.playerID == uuid.Nil {
		return inv(nil, "id, walletId and playerId are required")
	}
	if !t.kind.Valid() {
		return inv(ErrInvalidKind, "%q", t.kind)
	}
	if !t.status.Valid() {
		return inv(nil, "unknown status %q", t.status)
	}

	// Valor: LOSS exige zero; todos os demais, valor positivo.
	if !t.amount.IsValid() || t.amount.IsNegative() {
		return inv(ErrInvalidAmount, "amount must be initialized and non-negative")
	}
	if t.kind == KindLoss && !t.amount.IsZero() {
		return inv(ErrInvalidAmount, "LOSS requires amount 0.00, got %v", t.amount)
	}
	if t.kind != KindLoss && !t.amount.IsPositive() {
		return inv(ErrInvalidAmount, "%s requires a positive amount, got %v", t.kind, t.amount)
	}

	// Origem: OPENING é interno; os demais exigem todos os metadados externos.
	if t.kind == KindOpening {
		if t.origin != nil {
			return inv(nil, "OPENING cannot carry external metadata")
		}
		if t.status != StatusProcessed {
			return inv(nil, "OPENING must be PROCESSED")
		}
	} else {
		if t.origin == nil {
			return inv(nil, "%s requires external metadata", t.kind)
		}
		if err := t.origin.validate(); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidTransaction, err)
		}
		hasRef := t.origin.ReferenceExternalTransactionID != ""
		if t.kind.RequiresReference() && !hasRef {
			return inv(ErrInvalidReference, "%s requires referenceExternalTransactionId", t.kind)
		}
		if !t.kind.AllowsReference() && hasRef {
			return inv(ErrInvalidReference, "%s does not accept a reference", t.kind)
		}
	}

	// Referência interna resolvida: só em transação processada que aceita referência.
	if t.referenceID != uuid.Nil && (t.status != StatusProcessed || !t.kind.AllowsReference()) {
		return inv(ErrInvalidReference, "resolved reference is only valid on a processed %s", t.kind)
	}
	if t.status == StatusProcessed && t.kind.RequiresReference() && t.referenceID == uuid.Nil {
		return inv(ErrInvalidReference, "a processed %s requires its resolved reference", t.kind)
	}

	// failureCode existe se, e somente se, a transação foi rejeitada ou falhou.
	switch t.status {
	case StatusRejected:
		if !t.failureCode.IsRejection() {
			return inv(nil, "REJECTED requires a rejection code, got %q", t.failureCode)
		}
	case StatusFailed:
		if !t.failureCode.IsFailure() {
			return inv(nil, "FAILED requires a failure code, got %q", t.failureCode)
		}
	default:
		if t.failureCode != "" {
			return inv(nil, "failureCode is only valid on REJECTED or FAILED")
		}
	}

	// Resultado financeiro: só em PROCESSED, na moeda da transação, nunca negativo.
	if t.status == StatusProcessed {
		if !t.resultBalance.IsValid() || t.resultBalance.IsNegative() ||
			t.resultBalance.Currency() != t.amount.Currency() {
			return inv(nil, "PROCESSED requires a non-negative result balance in %s", t.amount.Currency())
		}
	} else if t.resultBalance.IsValid() {
		return inv(nil, "result balance is only valid on PROCESSED")
	}

	// Retomada durável: pendente <=> próxima tentativa agendada.
	if t.status.IsPending() != !t.nextAttemptAt.IsZero() {
		return inv(nil, "next attempt must be scheduled exactly while the transaction is pending")
	}
	switch t.status {
	case StatusPending:
		if !t.expiresAt.IsZero() {
			return inv(nil, "PENDING has no reference deadline")
		}
	case StatusPendingReference:
		if t.expiresAt.IsZero() || t.origin == nil || t.origin.ReferenceExternalTransactionID == "" {
			return inv(nil, "PENDING_REFERENCE requires a reference and a deadline")
		}
	}
	if t.attempts < 0 {
		return inv(nil, "attempts cannot be negative")
	}

	// Instantes.
	if t.createdAt.IsZero() || t.updatedAt.IsZero() || t.updatedAt.Before(t.createdAt) {
		return inv(nil, "createdAt and updatedAt are required and updatedAt >= createdAt")
	}
	if t.status.IsTerminal() != !t.finishedAt.IsZero() {
		return inv(nil, "finishedAt must be set exactly when the transaction is terminal")
	}
	return nil
}

// ---- transições -----------------------------------------------------------

func (t *Transaction) guard(to Status, now time.Time, from ...Status) error {
	if now.IsZero() {
		return fmt.Errorf("%w: transition time is required", ErrInvalidTransition)
	}
	for _, s := range from {
		if t.status == s {
			return nil
		}
	}
	return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, t.status, to)
}

// touch devolve o novo updatedAt: nunca anterior ao atual, o que absorve
// pequenas diferenças de relógio entre instâncias (o schema exige updated_at >= created_at).
func (t *Transaction) touch(now time.Time) time.Time {
	n := now.UTC()
	if n.Before(t.updatedAt) {
		return t.updatedAt
	}
	return n
}

func (t *Transaction) commit(next Transaction) error {
	if err := next.checkInvariants(); err != nil {
		return err
	}
	*t = next
	return nil
}

// AwaitReference move PENDING -> PENDING_REFERENCE quando a referência ainda não
// está disponível. nextAttemptAt agenda a próxima tentativa e expiresAt é o prazo (TTL).
func (t *Transaction) AwaitReference(now, nextAttemptAt, expiresAt time.Time) error {
	if err := t.guard(StatusPendingReference, now, StatusPending); err != nil {
		return err
	}
	if t.origin == nil || t.origin.ReferenceExternalTransactionID == "" {
		return fmt.Errorf("%w: a %s without referenceExternalTransactionId has no reference to wait for", ErrInvalidTransition, t.kind)
	}
	if nextAttemptAt.IsZero() || !expiresAt.After(now) {
		return fmt.Errorf("%w: AwaitReference requires a next attempt and a deadline after now", ErrInvalidTransition)
	}
	next := *t
	next.status = StatusPendingReference
	next.nextAttemptAt = nextAttemptAt.UTC()
	next.expiresAt = expiresAt.UTC()
	next.updatedAt = t.touch(now)
	return t.commit(next)
}

// Retry registra uma nova tentativa sem mudar o estado (PENDING ou PENDING_REFERENCE).
func (t *Transaction) Retry(now, nextAttemptAt time.Time) error {
	if err := t.guard(t.status, now, StatusPending, StatusPendingReference); err != nil {
		return err
	}
	if nextAttemptAt.IsZero() {
		return fmt.Errorf("%w: Retry requires the next attempt time", ErrInvalidTransition)
	}
	next := *t
	next.attempts++
	next.nextAttemptAt = nextAttemptAt.UTC()
	next.updatedAt = t.touch(now)
	return t.commit(next)
}

// ProcessParams reúne o resultado de um processamento bem-sucedido.
type ProcessParams struct {
	// Saldo da carteira observado ao concluir; é o valor devolvido em replays.
	ResultBalance money.Money
	// Transação referenciada, já resolvida. Obrigatória em REFUND e ROLLBACK,
	// opcional em WIN (quando informou referência) e proibida nos demais.
	ReferenceID uuid.UUID
	Now         time.Time
}

// Process move PENDING ou PENDING_REFERENCE para PROCESSED (terminal).
func (t *Transaction) Process(p ProcessParams) error {
	if err := t.guard(StatusProcessed, p.Now, StatusPending, StatusPendingReference); err != nil {
		return err
	}
	next := *t
	next.status = StatusProcessed
	next.resultBalance = p.ResultBalance
	next.referenceID = p.ReferenceID
	next.nextAttemptAt = time.Time{}
	next.updatedAt = t.touch(p.Now)
	next.finishedAt = next.updatedAt
	return t.commit(next)
}

// Reject move PENDING ou PENDING_REFERENCE para REJECTED (terminal) com um código de rejeição.
func (t *Transaction) Reject(code FailureCode, now time.Time) error {
	if err := t.guard(StatusRejected, now, StatusPending, StatusPendingReference); err != nil {
		return err
	}
	if !code.IsRejection() {
		return fmt.Errorf("%w: %q is not a rejection code", ErrInvalidTransition, code)
	}
	next := *t
	next.status = StatusRejected
	next.failureCode = code
	next.nextAttemptAt = time.Time{}
	next.updatedAt = t.touch(now)
	next.finishedAt = next.updatedAt
	return t.commit(next)
}

// Fail move PENDING ou PENDING_REFERENCE para FAILED (terminal) com um código de falha permanente.
func (t *Transaction) Fail(code FailureCode, now time.Time) error {
	if err := t.guard(StatusFailed, now, StatusPending, StatusPendingReference); err != nil {
		return err
	}
	if !code.IsFailure() {
		return fmt.Errorf("%w: %q is not a failure code", ErrInvalidTransition, code)
	}
	next := *t
	next.status = StatusFailed
	next.failureCode = code
	next.nextAttemptAt = time.Time{}
	next.updatedAt = t.touch(now)
	next.finishedAt = next.updatedAt
	return t.commit(next)
}

// ---- consultas ------------------------------------------------------------

// MatchesPayload informa se o hash coincide com o da operação original: igual =
// replay legítimo; diferente = conflito de idempotência.
func (t *Transaction) MatchesPayload(h PayloadHash) bool {
	return t.origin != nil && t.origin.PayloadHash == h
}

func (t *Transaction) ID() uuid.UUID            { return t.id }
func (t *Transaction) Kind() Kind               { return t.kind }
func (t *Transaction) Status() Status           { return t.status }
func (t *Transaction) WalletID() uuid.UUID      { return t.walletID }
func (t *Transaction) PlayerID() uuid.UUID      { return t.playerID }
func (t *Transaction) Amount() money.Money      { return t.amount }
func (t *Transaction) FailureCode() FailureCode { return t.failureCode }
func (t *Transaction) Attempts() int            { return t.attempts }
func (t *Transaction) NextAttemptAt() time.Time { return t.nextAttemptAt }
func (t *Transaction) ExpiresAt() time.Time     { return t.expiresAt }
func (t *Transaction) CreatedAt() time.Time     { return t.createdAt }
func (t *Transaction) UpdatedAt() time.Time     { return t.updatedAt }
func (t *Transaction) FinishedAt() time.Time    { return t.finishedAt }
func (t *Transaction) IsTerminal() bool         { return t.status.IsTerminal() }

// Origin devolve uma cópia dos metadados externos; ok é falso em OPENING.
func (t *Transaction) Origin() (o Origin, ok bool) {
	if t.origin == nil {
		return Origin{}, false
	}
	return *t.origin, true
}

// ReferenceID devolve a referência interna resolvida, quando existir.
func (t *Transaction) ReferenceID() (uuid.UUID, bool) {
	return t.referenceID, t.referenceID != uuid.Nil
}

// ResultBalance devolve o saldo observado no processamento (somente PROCESSED).
func (t *Transaction) ResultBalance() (money.Money, bool) {
	return t.resultBalance, t.resultBalance.IsValid()
}
