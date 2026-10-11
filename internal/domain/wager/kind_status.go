package wager

import "github.com/PBelle451/backend-challenge-go/internal/domain/wallet"

// Kind é o tipo de uma transação financeira.
type Kind string

const (
	KindOpening  Kind = "OPENING"  // interno: abertura de carteira com saldo positivo
	KindBet      Kind = "BET"      // débito
	KindWin      Kind = "WIN"      // crédito
	KindLoss     Kind = "LOSS"     // sem movimentação
	KindRefund   Kind = "REFUND"   // crédito: devolve integralmente uma BET processada
	KindRollback Kind = "ROLLBACK" // movimento contrário ao de uma BET, WIN ou REFUND processada
)

// IsExternal informa se o tipo pode chegar por HTTP ou SQS (todos, exceto OPENING).
func (k Kind) IsExternal() bool {
	switch k {
	case KindBet, KindWin, KindLoss, KindRefund, KindRollback:
		return true
	}
	return false
}

// Valid informa se k é um dos seis tipos conhecidos.
func (k Kind) Valid() bool { return k == KindOpening || k.IsExternal() }

// RequiresReference: REFUND e ROLLBACK exigem referenceExternalTransactionId.
func (k Kind) RequiresReference() bool { return k == KindRefund || k == KindRollback }

// AllowsReference: só WIN (opcional) e as reversões aceitam referência.
func (k Kind) AllowsReference() bool { return k == KindWin || k.RequiresReference() }

// Direction devolve o sentido da movimentação quando ele depende apenas do tipo.
// ROLLBACK depende da transação referenciada e LOSS não movimenta: ok é falso.
func (k Kind) Direction() (d wallet.Direction, ok bool) {
	switch k {
	case KindBet:
		return wallet.DirectionDebit, true
	case KindWin, KindRefund, KindOpening:
		return wallet.DirectionCredit, true
	}
	return "", false
}

// InsufficientFundsCode devolve o failureCode correto quando um débito não cabe
// no saldo: aposta sem saldo e reversão sem saldo têm códigos distintos.
func (k Kind) InsufficientFundsCode() FailureCode {
	if k.RequiresReference() {
		return FailureReversalInsufficientFunds
	}
	return FailureInsufficientFunds
}

// Status é o estado de uma transação.
type Status string

const (
	StatusPending          Status = "PENDING"
	StatusPendingReference Status = "PENDING_REFERENCE"
	StatusProcessed        Status = "PROCESSED" // terminal
	StatusRejected         Status = "REJECTED"  // terminal
	StatusFailed           Status = "FAILED"    // terminal
)

func (s Status) Valid() bool {
	switch s {
	case StatusPending, StatusPendingReference, StatusProcessed, StatusRejected, StatusFailed:
		return true
	}
	return false
}

// IsTerminal: PROCESSED, REJECTED e FAILED não aceitam novas transições.
func (s Status) IsTerminal() bool {
	return s == StatusProcessed || s == StatusRejected || s == StatusFailed
}

// IsPending: PENDING e PENDING_REFERENCE têm retomada durável agendada.
func (s Status) IsPending() bool { return s == StatusPending || s == StatusPendingReference }

// FailureCode é um código estável, documentado e serializável de rejeição ou falha.
type FailureCode string

// FailureClass distingue entradas corrigíveis de resultados definitivos.
type FailureClass string

const (
	// ClassCorrectable: o conteúdo enviado estava incoerente com os dados existentes.
	// Corrige-se enviando uma NOVA operação, com novo externalTransactionId (o
	// identificador de uma operação rejeitada continua ocupado).
	ClassCorrectable FailureClass = "CORRECTABLE"
	// ClassDefinitive: resultado de negócio final; reenviar não muda o desfecho.
	ClassDefinitive FailureClass = "DEFINITIVE"
	// ClassInfrastructure: falha permanente de infraestrutura registrada para auditoria (FAILED).
	ClassInfrastructure FailureClass = "INFRASTRUCTURE"
)

// Códigos que levam a REJECTED.
const (
	FailureInsufficientFunds         FailureCode = "INSUFFICIENT_FUNDS"          // BET sem saldo
	FailureReversalInsufficientFunds FailureCode = "REVERSAL_INSUFFICIENT_FUNDS" // reversão que precisaria debitar mais que o saldo
	FailureAlreadyReversed           FailureCode = "ALREADY_REVERSED"            // a referência já recebeu uma reversão bem-sucedida
	FailureReferenceNotFound         FailureCode = "REFERENCE_NOT_FOUND"         // prazo/tentativas esgotados e a referência nunca chegou
	FailureReferencePendingExpired   FailureCode = "REFERENCE_PENDING_EXPIRED"   // a referência existe, mas continuou pendente até o fim do prazo
	FailureReferenceNotProcessed     FailureCode = "REFERENCE_NOT_PROCESSED"     // a referência terminou sem sucesso (REJECTED ou FAILED)
	FailureReferenceMismatch         FailureCode = "REFERENCE_MISMATCH"          // provedor, jogador, carteira, moeda ou rodada divergem
	FailureAmountMismatch            FailureCode = "AMOUNT_MISMATCH"             // reversão com valor diferente do referenciado
	FailureInvalidReferenceKind      FailureCode = "INVALID_REFERENCE_KIND"      // tipo da referência incompatível com a operação
)

// Códigos que levam a FAILED.
const (
	FailureRetriesExhausted FailureCode = "RETRIES_EXHAUSTED" // tentativas de processamento esgotadas por falhas transitórias
	FailureInternalError    FailureCode = "INTERNAL_ERROR"    // erro permanente e inesperado
)

var failureClasses = map[FailureCode]FailureClass{
	FailureInsufficientFunds:         ClassDefinitive,
	FailureReversalInsufficientFunds: ClassDefinitive,
	FailureAlreadyReversed:           ClassDefinitive,
	FailureReferenceNotFound:         ClassDefinitive,
	FailureReferencePendingExpired:   ClassDefinitive,
	FailureReferenceNotProcessed:     ClassDefinitive,
	FailureReferenceMismatch:         ClassCorrectable,
	FailureAmountMismatch:            ClassCorrectable,
	FailureInvalidReferenceKind:      ClassCorrectable,
	FailureRetriesExhausted:          ClassInfrastructure,
	FailureInternalError:             ClassInfrastructure,
}

// Class devolve a classe do código; ok é falso para códigos desconhecidos.
func (c FailureCode) Class() (class FailureClass, ok bool) {
	class, ok = failureClasses[c]
	return class, ok
}

// IsRejection: o código é válido em uma transação REJECTED.
func (c FailureCode) IsRejection() bool {
	class, ok := c.Class()
	return ok && class != ClassInfrastructure
}

// IsFailure: o código é válido em uma transação FAILED.
func (c FailureCode) IsFailure() bool {
	class, ok := c.Class()
	return ok && class == ClassInfrastructure
}
