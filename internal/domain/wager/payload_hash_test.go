package wager_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/PBelle451/backend-challenge-go/internal/domain/wager"
)

func baseInput(t *testing.T) wager.HashInput {
	return wager.HashInput{
		ProviderID: "provider-a", ExternalTransactionID: "transaction-123",
		PlayerID: uuid.MustParse("0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1"),
		WalletID: uuid.MustParse("0192f291-27dd-7d3f-8071-5f8685deef37"),
		RoundID:  "round-987", GameID: "fortune-chimp", Kind: wager.KindBet,
		Money: amt(t, "25.00", "BRL"),
	}
}

// Fixa o algoritmo: qualquer mudança nos campos, na ordenação ou na normalização
// altera este hash e quebra o teste, o que é desejável (idempotência persistida
// depende de o hash ser estável entre versões).
func TestComputePayloadHash_Golden(t *testing.T) {
	h, err := wager.ComputePayloadHash(baseInput(t))
	if err != nil {
		t.Fatal(err)
	}
	// SHA-256 de (conferido de forma independente, em Python):
	// {"externalTransactionId":"transaction-123","gameId":"fortune-chimp","kind":"BET",
	//  "money":{"amount":"25.00","currency":"BRL"},"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
	//  "providerId":"provider-a","roundId":"round-987","walletId":"0192f291-27dd-7d3f-8071-5f8685deef37"}
	const want = "629836932b79106b99523d06a1e7fa80689b0ea1e1c47aa3f0a5a2c87d0c4344"
	if h.String() != want {
		t.Errorf("hash = %s, want %s", h, want)
	}
}

func TestComputePayloadHash_DeterministicAndSensitive(t *testing.T) {
	base, err := wager.ComputePayloadHash(baseInput(t))
	if err != nil {
		t.Fatal(err)
	}
	again, _ := wager.ComputePayloadHash(baseInput(t))
	if base != again {
		t.Fatal("the same input must always produce the same hash")
	}
	if base.IsZero() {
		t.Fatal("hash must not be zero")
	}

	mutations := map[string]func(in *wager.HashInput){
		"providerId":            func(in *wager.HashInput) { in.ProviderID = "provider-b" },
		"externalTransactionId": func(in *wager.HashInput) { in.ExternalTransactionID = "transaction-124" },
		"playerId":              func(in *wager.HashInput) { in.PlayerID = uid(9) },
		"walletId":              func(in *wager.HashInput) { in.WalletID = uid(9) },
		"roundId":               func(in *wager.HashInput) { in.RoundID = "round-988" },
		"gameId":                func(in *wager.HashInput) { in.GameID = "other-game" },
		"kind":                  func(in *wager.HashInput) { in.Kind = wager.KindWin },
		"amount":                func(in *wager.HashInput) { in.Money = amt(t, "25.01", "BRL") },
		"currency":              func(in *wager.HashInput) { in.Money = amt(t, "25.00", "USD") },
		"reference presente":    func(in *wager.HashInput) { in.ReferenceExternalTransactionID = "transaction-1" },
	}
	seen := map[wager.PayloadHash]string{base: "base"}
	for name, mutate := range mutations {
		in := baseInput(t)
		mutate(&in)
		h, err := wager.ComputePayloadHash(in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if other, dup := seen[h]; dup {
			t.Errorf("changing %s produced the same hash as %s", name, other)
		}
		seen[h] = name
	}
}

// HTTP e SQS devem produzir o mesmo hash para a mesma operação, mesmo que o UUID
// chegue com grafias diferentes: o parser os reduz ao mesmo valor canônico.
func TestComputePayloadHash_EquivalentSpellingsOfUUIDs(t *testing.T) {
	lower := baseInput(t)
	upper := baseInput(t)
	upper.PlayerID = uuid.MustParse("0192F28F-5DC0-7D58-BDB2-814AD6A0F4A1")
	upper.WalletID = uuid.MustParse("{0192f291-27dd-7d3f-8071-5f8685deef37}")

	a, _ := wager.ComputePayloadHash(lower)
	b, err := wager.ComputePayloadHash(upper)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Error("equivalent UUID spellings must hash equally")
	}
}

func TestComputePayloadHash_TextIsNotNormalizedButHTMLIsNotEscaped(t *testing.T) {
	a := baseInput(t)
	a.RoundID = "a<b>&c"
	h1, err := wager.ComputePayloadHash(a)
	if err != nil {
		t.Fatal(err)
	}
	h2, _ := wager.ComputePayloadHash(a)
	if h1 != h2 {
		t.Error("hash with special characters must be stable")
	}
	b := baseInput(t)
	b.RoundID = "a<b>&d"
	if h3, _ := wager.ComputePayloadHash(b); h3 == h1 {
		t.Error("different text must hash differently")
	}
}

func TestComputePayloadHash_Invalid(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(in *wager.HashInput)
		want   error
	}{
		{"OPENING nao e operacao externa", func(in *wager.HashInput) { in.Kind = wager.KindOpening }, wager.ErrInvalidKind},
		{"tipo desconhecido", func(in *wager.HashInput) { in.Kind = "TRANSFER" }, wager.ErrInvalidKind},
		{"playerId nulo", func(in *wager.HashInput) { in.PlayerID = uuid.Nil }, wager.ErrInvalidField},
		{"walletId nulo", func(in *wager.HashInput) { in.WalletID = uuid.Nil }, wager.ErrInvalidField},
		{"money nao inicializado", func(in *wager.HashInput) { in.Money = zeroMoney() }, wager.ErrInvalidAmount},
		{"providerId vazio", func(in *wager.HashInput) { in.ProviderID = "" }, wager.ErrInvalidField},
		{"externalTransactionId vazio", func(in *wager.HashInput) { in.ExternalTransactionID = "" }, wager.ErrInvalidField},
		{"roundId vazio", func(in *wager.HashInput) { in.RoundID = "" }, wager.ErrInvalidField},
		{"gameId vazio", func(in *wager.HashInput) { in.GameID = "" }, wager.ErrInvalidField},
		{"espaco no inicio", func(in *wager.HashInput) { in.ProviderID = " provider-a" }, wager.ErrInvalidField},
		{"espaco no fim", func(in *wager.HashInput) { in.RoundID = "round-987 " }, wager.ErrInvalidField},
		{"quebra de linha no fim", func(in *wager.HashInput) { in.RoundID = "round-987\n" }, wager.ErrInvalidField},
		{"caractere de controle", func(in *wager.HashInput) { in.GameID = "fortune\x00chimp" }, wager.ErrInvalidField},
		{"UTF-8 invalido", func(in *wager.HashInput) { in.GameID = "fortune\xffchimp" }, wager.ErrInvalidField},
		{"identificador longo demais", func(in *wager.HashInput) { in.ProviderID = strings.Repeat("a", 129) }, wager.ErrInvalidField},
		{"referencia invalida", func(in *wager.HashInput) { in.ReferenceExternalTransactionID = " x" }, wager.ErrInvalidField},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInput(t)
			tc.mutate(&in)
			h, err := wager.ComputePayloadHash(in)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if !h.IsZero() {
				t.Error("an error must not come with a hash")
			}
		})
	}

	neg := baseInput(t)
	neg.Money = negMoney(t)
	if _, err := wager.ComputePayloadHash(neg); !errors.Is(err, wager.ErrInvalidAmount) {
		t.Errorf("negative money: error = %v", err)
	}
	long := baseInput(t)
	long.ProviderID = strings.Repeat("a", 128)
	if _, err := wager.ComputePayloadHash(long); err != nil {
		t.Errorf("an identifier of exactly 128 bytes must be accepted: %v", err)
	}
}
