package wager

import (
	"fmt"

	"github.com/google/uuid"

	"github.com/PBelle451/backend-challenge-go/internal/domain/wallet"
)

// ReferenceOutcome é o resultado da avaliação de uma referência.
type ReferenceOutcome string

const (
	// OutcomeApply: a referência é válida; a operação pode ser aplicada na direção indicada.
	OutcomeApply ReferenceOutcome = "APPLY"
	// OutcomeWait: a referência ainda não chegou ou ainda está pendente; tentar de novo depois.
	OutcomeWait ReferenceOutcome = "WAIT"
	// OutcomeReject: a operação deve ser rejeitada com o FailureCode informado.
	OutcomeReject ReferenceOutcome = "REJECT"
)

// ReferenceDecision é o veredito de EvaluateReference.
type ReferenceDecision struct {
	Outcome     ReferenceOutcome
	Failure     FailureCode      // preenchido em OutcomeReject
	Direction   wallet.Direction // preenchido em OutcomeApply
	ReferenceID uuid.UUID        // preenchido em OutcomeApply
}

func wait() ReferenceDecision { return ReferenceDecision{Outcome: OutcomeWait} }
func reject(c FailureCode) ReferenceDecision {
	return ReferenceDecision{Outcome: OutcomeReject, Failure: c}
}

// referenceKindAllowed define quais tipos podem ser referenciados:
//
//	WIN      -> BET
//	REFUND   -> BET
//	ROLLBACK -> BET, WIN ou REFUND
func referenceKindAllowed(op, ref Kind) bool {
	switch op {
	case KindWin, KindRefund:
		return ref == KindBet
	case KindRollback:
		return ref == KindBet || ref == KindWin || ref == KindRefund
	}
	return false
}

// EvaluateReference aplica as regras de referência a uma operação que informou
// referenceExternalTransactionId (REFUND, ROLLBACK ou WIN com referência).
//
// ref é a transação encontrada por (providerId, referenceExternalTransactionId),
// ou nil se ainda não existe. alreadyReversed informa se ref já recebeu uma
// reversão (REFUND ou ROLLBACK) PROCESSED; o banco reforça essa regra com um
// índice único, então a consulta aqui serve para devolver o código correto.
//
// Ordem das verificações (a primeira que falhar decide):
//  1. ref ausente ou ainda pendente      -> WAIT
//  2. ref REJECTED ou FAILED             -> REJECT REFERENCE_NOT_PROCESSED
//  3. tipo da referência incompatível    -> REJECT INVALID_REFERENCE_KIND
//  4. provedor, jogador, carteira, moeda ou rodada diferentes -> REJECT REFERENCE_MISMATCH
//  5. reversão com valor diferente       -> REJECT AMOUNT_MISMATCH
//  6. ref já revertida                   -> REJECT ALREADY_REVERSED
//  7. caso contrário                     -> APPLY (com a direção da movimentação)
//
// Direção: WIN e REFUND creditam. ROLLBACK faz o movimento contrário ao original:
// credita ao desfazer uma BET e debita ao desfazer um WIN ou um REFUND.
//
// Combinações sobre a mesma aposta: uma transação referenciada recebe no máximo UMA
// reversão bem-sucedida, seja REFUND ou ROLLBACK. Assim o débito de uma BET nunca é
// devolvido duas vezes. Uma reversão pode, ela mesma, ser desfeita por um ROLLBACK
// (ROLLBACK de um REFUND), mas isso não reabre a BET original para outra reversão.
func (t *Transaction) EvaluateReference(ref *Transaction, alreadyReversed bool) (ReferenceDecision, error) {
	if t.origin == nil || t.origin.ReferenceExternalTransactionID == "" {
		return ReferenceDecision{}, fmt.Errorf("%w: %s %s has no reference to evaluate", ErrInvalidReference, t.kind, t.id)
	}
	if t.status.IsTerminal() {
		return ReferenceDecision{}, fmt.Errorf("%w: cannot evaluate a reference of a %s transaction", ErrInvalidTransition, t.status)
	}

	// 1 e 2: existência e estado da referência.
	if ref == nil {
		return wait(), nil
	}
	switch {
	case ref.status.IsPending():
		return wait(), nil
	case ref.status == StatusRejected || ref.status == StatusFailed:
		return reject(FailureReferenceNotProcessed), nil
	}

	// 3: tipo da referência.
	if ref.id == t.id || ref.origin == nil || !referenceKindAllowed(t.kind, ref.kind) {
		return reject(FailureInvalidReferenceKind), nil
	}

	// 4: coerência de provedor, jogador, carteira, moeda e rodada.
	if ref.origin.ProviderID != t.origin.ProviderID ||
		ref.playerID != t.playerID ||
		ref.walletID != t.walletID ||
		ref.amount.Currency() != t.amount.Currency() ||
		ref.origin.RoundID != t.origin.RoundID {
		return reject(FailureReferenceMismatch), nil
	}

	// 5 e 6: valor e reversão duplicada (somente REFUND e ROLLBACK).
	if t.kind.RequiresReference() {
		if !t.amount.Equal(ref.amount) {
			return reject(FailureAmountMismatch), nil
		}
		if alreadyReversed {
			return reject(FailureAlreadyReversed), nil
		}
	}

	// 7: direção da movimentação.
	dir := wallet.DirectionCredit
	if t.kind == KindRollback && ref.kind != KindBet {
		dir = wallet.DirectionDebit
	}
	return ReferenceDecision{Outcome: OutcomeApply, Direction: dir, ReferenceID: ref.id}, nil
}
