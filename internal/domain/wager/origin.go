package wager

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/PBelle451/backend-challenge-go/internal/domain/money"
)

// ErrInvalidField indica um campo de texto (identificador) inválido.
var ErrInvalidField = errors.New("wager: invalid field")

const (
	maxIDLength             = 128
	maxIdempotencyKeyLength = 255
)

// PayloadHash é o SHA-256 do JSON canônico dos campos de negócio de uma operação.
type PayloadHash [sha256.Size]byte

func (h PayloadHash) IsZero() bool   { return h == PayloadHash{} }
func (h PayloadHash) String() string { return hex.EncodeToString(h[:]) }

// Origin reúne os metadados de uma operação EXTERNA (HTTP ou SQS).
type Origin struct {
	ProviderID                     string
	ExternalTransactionID          string
	IdempotencyKey                 string
	PayloadHash                    PayloadHash
	RoundID                        string
	GameID                         string
	ReferenceExternalTransactionID string // obrigatório em REFUND/ROLLBACK, opcional em WIN, ausente nos demais
}

// validateToken rejeita, sem normalizar, identificadores vazios, longos demais,
// com bytes inválidos, caracteres de controle ou espaços nas pontas.
func validateToken(name, s string, max int) error {
	switch {
	case s == "":
		return fmt.Errorf("%w: %s is required", ErrInvalidField, name)
	case len(s) > max:
		return fmt.Errorf("%w: %s exceeds %d bytes", ErrInvalidField, name, max)
	case !utf8.ValidString(s):
		return fmt.Errorf("%w: %s is not valid UTF-8", ErrInvalidField, name)
	case s[0] == ' ' || s[len(s)-1] == ' ' || unicode.IsSpace(rune(s[0])) || unicode.IsSpace(rune(s[len(s)-1])):
		return fmt.Errorf("%w: %s has leading or trailing whitespace", ErrInvalidField, name)
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: %s contains a control character", ErrInvalidField, name)
		}
	}
	return nil
}

func (o Origin) validate() error {
	for _, f := range []struct {
		name, value string
		max         int
	}{
		{"providerId", o.ProviderID, maxIDLength},
		{"externalTransactionId", o.ExternalTransactionID, maxIDLength},
		{"idempotencyKey", o.IdempotencyKey, maxIdempotencyKeyLength},
		{"roundId", o.RoundID, maxIDLength},
		{"gameId", o.GameID, maxIDLength},
	} {
		if err := validateToken(f.name, f.value, f.max); err != nil {
			return err
		}
	}
	if o.ReferenceExternalTransactionID != "" {
		if err := validateToken("referenceExternalTransactionId", o.ReferenceExternalTransactionID, maxIDLength); err != nil {
			return err
		}
	}
	if o.PayloadHash.IsZero() {
		return fmt.Errorf("%w: payloadHash is required", ErrInvalidField)
	}
	return nil
}

// HashInput reúne os campos de negócio que entram no hash de idempotência.
type HashInput struct {
	ProviderID                     string
	ExternalTransactionID          string
	PlayerID                       uuid.UUID
	WalletID                       uuid.UUID
	RoundID                        string
	GameID                         string
	Kind                           Kind
	Money                          money.Money
	ReferenceExternalTransactionID string
}

// ComputePayloadHash calcula o hash determinístico usado para distinguir um replay
// legítimo de uma reutilização indevida da chave de idempotência.
//
// Algoritmo: SHA-256 sobre o JSON canônico (UTF-8, sem espaços, chaves em ordem
// lexicográfica) do objeto:
//
//	{"externalTransactionId", "gameId", "kind",
//	 "money":{"amount","currency"}, "playerId", "providerId",
//	 "referenceExternalTransactionId" (omitido quando vazio),
//	 "roundId", "walletId"}
//
// Ficam FORA do cálculo: a chave de idempotência (header ou data.idempotencyKey) e
// todo metadado de transporte (messageId, occurredAt, type, headers, correlationId).
// Por isso HTTP e SQS produzem o mesmo hash para a mesma operação.
//
// Normalizações: UUIDs entram na forma canônica minúscula com hífens (qualquer
// grafia aceita pelo parser resulta no mesmo texto); money.amount já tem uma única
// forma canônica (ver money.Parse). Textos NÃO são normalizados: formas inválidas
// (espaços nas pontas, caracteres de controle) são rejeitadas antes.
func ComputePayloadHash(in HashInput) (PayloadHash, error) {
	if !in.Kind.IsExternal() {
		return PayloadHash{}, fmt.Errorf("%w: kind %q cannot be hashed as an external operation", ErrInvalidKind, in.Kind)
	}
	if in.PlayerID == uuid.Nil || in.WalletID == uuid.Nil {
		return PayloadHash{}, fmt.Errorf("%w: playerId and walletId are required", ErrInvalidField)
	}
	if !in.Money.IsValid() || in.Money.IsNegative() {
		return PayloadHash{}, fmt.Errorf("%w: money must be initialized and non-negative", ErrInvalidAmount)
	}
	for _, f := range []struct{ name, value string }{
		{"providerId", in.ProviderID}, {"externalTransactionId", in.ExternalTransactionID},
		{"roundId", in.RoundID}, {"gameId", in.GameID},
	} {
		if err := validateToken(f.name, f.value, maxIDLength); err != nil {
			return PayloadHash{}, err
		}
	}
	if in.ReferenceExternalTransactionID != "" {
		if err := validateToken("referenceExternalTransactionId", in.ReferenceExternalTransactionID, maxIDLength); err != nil {
			return PayloadHash{}, err
		}
	}

	doc := map[string]any{
		"providerId":            in.ProviderID,
		"externalTransactionId": in.ExternalTransactionID,
		"playerId":              in.PlayerID.String(),
		"walletId":              in.WalletID.String(),
		"roundId":               in.RoundID,
		"gameId":                in.GameID,
		"kind":                  string(in.Kind),
		"money": map[string]any{
			"amount":   in.Money.Amount(),
			"currency": string(in.Money.Currency()),
		},
	}
	if in.ReferenceExternalTransactionID != "" {
		doc["referenceExternalTransactionId"] = in.ReferenceExternalTransactionID
	}

	// encoding/json ordena as chaves de mapas; o escape de HTML é desligado para
	// que o texto canônico não dependa de caracteres como <, > e &.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return PayloadHash{}, fmt.Errorf("wager: canonical json: %w", err)
	}
	return PayloadHash(sha256.Sum256(bytes.TrimRight(buf.Bytes(), "\n"))), nil
}
