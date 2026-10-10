// Package money implementa o value object Money: valor monetário imutável com moeda.
//
// Representação: int64 em unidades mínimas (centavos), com escala fixa de duas casas
// decimais para todas as moedas. Nenhum float32/float64 é usado em parsing, cálculo,
// formatação ou serialização.
//
// Limites: o valor representável vai de -92233720368547758.08 a 92233720368547758.07
// (math.MinInt64 e math.MaxInt64 em centavos). Todas as operações detectam overflow
// e devolvem ErrOverflow em vez de gerar um valor errado.
//
// O zero value de Money{} NÃO é válido (não tem moeda): toda operação sobre ele
// devolve ErrUninitialized. Use Zero(moeda) para obter um zero válido.
package money

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
)

// Erros de domínio, classificáveis com errors.Is.
var (
	ErrUninitialized    = errors.New("money: uninitialized value")
	ErrInvalidAmount    = errors.New("money: invalid amount")
	ErrNegativeAmount   = errors.New("money: negative amount not allowed")
	ErrInvalidCurrency  = errors.New("money: invalid currency")
	ErrCurrencyMismatch = errors.New("money: currency mismatch")
	ErrOverflow         = errors.New("money: arithmetic overflow")
)

const (
	scale       = 100 // unidades mínimas por unidade principal (duas casas decimais)
	scaleDigits = 2
)

// Currency é um código de moeda no formato ISO 4217 (três letras maiúsculas).
// Apenas o formato é validado; não há lista fechada de moedas.
type Currency string

// NewCurrency valida o formato do código e o devolve como Currency.
func NewCurrency(code string) (Currency, error) {
	if len(code) != 3 {
		return "", fmt.Errorf("%w: %q must have exactly 3 letters", ErrInvalidCurrency, code)
	}
	for i := 0; i < len(code); i++ {
		if code[i] < 'A' || code[i] > 'Z' {
			return "", fmt.Errorf("%w: %q must be uppercase A-Z", ErrInvalidCurrency, code)
		}
	}
	return Currency(code), nil
}

func (c Currency) String() string { return string(c) }

// Money é imutável: os campos são privados e toda operação devolve um novo valor.
type Money struct {
	minor    int64
	currency Currency
}

// New cria um Money a partir de unidades mínimas (por exemplo, 2500 = 25.00).
// Aceita valores negativos, que existem em diferenças e cálculos internos.
func New(minor int64, currency Currency) (Money, error) {
	if _, err := NewCurrency(string(currency)); err != nil {
		return Money{}, err
	}
	return Money{minor: minor, currency: currency}, nil
}

// Zero devolve o valor zero na moeda informada.
func Zero(currency Currency) (Money, error) { return New(0, currency) }

// Parse cria um Money a partir de uma string decimal vinda de uma entrada externa.
//
// Forma aceita (única, canônica): dígitos, ponto e exatamente duas casas decimais,
// sem sinal e sem zeros à esquerda desnecessários, por exemplo "0.00", "25.00",
// "1000.50". Como existe uma única forma textual por valor, nenhuma normalização é
// necessária antes do hash de idempotência.
//
// São rejeitados, sem arredondar: vazio, espaços, "NaN", "Infinity", notação
// científica, separador de milhar, escala diferente de duas casas, sinal (+ ou -),
// dígitos não ASCII e valores acima do limite de int64.
func Parse(amount, currency string) (Money, error) {
	cur, err := NewCurrency(currency)
	if err != nil {
		return Money{}, err
	}
	minor, err := parseAmount(amount)
	if err != nil {
		return Money{}, err
	}
	return Money{minor: minor, currency: cur}, nil
}

func parseAmount(s string) (int64, error) {
	if s == "" {
		return 0, fmt.Errorf("%w: empty", ErrInvalidAmount)
	}
	if s[0] == '-' {
		return 0, fmt.Errorf("%w: %q", ErrNegativeAmount, s)
	}

	dot := strings.IndexByte(s, '.')
	if dot < 0 {
		return 0, fmt.Errorf("%w: %q must have exactly %d decimal places", ErrInvalidAmount, s, scaleDigits)
	}
	intPart, fracPart := s[:dot], s[dot+1:]
	if len(fracPart) != scaleDigits {
		return 0, fmt.Errorf("%w: %q must have exactly %d decimal places", ErrInvalidAmount, s, scaleDigits)
	}
	if intPart == "" {
		return 0, fmt.Errorf("%w: %q has no integer part", ErrInvalidAmount, s)
	}
	if len(intPart) > 1 && intPart[0] == '0' {
		return 0, fmt.Errorf("%w: %q has leading zeros", ErrInvalidAmount, s)
	}

	var frac int64
	for i := 0; i < len(fracPart); i++ {
		d, ok := digit(fracPart[i])
		if !ok {
			return 0, fmt.Errorf("%w: %q contains a non-digit", ErrInvalidAmount, s)
		}
		frac = frac*10 + d
	}

	var whole int64
	for i := 0; i < len(intPart); i++ {
		d, ok := digit(intPart[i])
		if !ok {
			return 0, fmt.Errorf("%w: %q contains a non-digit", ErrInvalidAmount, s)
		}
		if whole > (math.MaxInt64-d)/10 {
			return 0, fmt.Errorf("%w: %q exceeds the supported range", ErrOverflow, s)
		}
		whole = whole*10 + d
	}
	if whole > (math.MaxInt64-frac)/scale {
		return 0, fmt.Errorf("%w: %q exceeds the supported range", ErrOverflow, s)
	}
	return whole*scale + frac, nil
}

func digit(b byte) (int64, bool) {
	if b < '0' || b > '9' {
		return 0, false
	}
	return int64(b - '0'), true
}

// IsValid informa se o Money foi construído (tem moeda). O zero value não é válido.
func (m Money) IsValid() bool { return m.currency != "" }

// Minor devolve o valor em unidades mínimas (usado na persistência em BIGINT).
func (m Money) Minor() int64 { return m.minor }

// Currency devolve a moeda.
func (m Money) Currency() Currency { return m.currency }

// Amount devolve o valor como string decimal com duas casas, por exemplo "25.00" ou "-0.05".
func (m Money) Amount() string {
	neg := m.minor < 0
	abs := uint64(m.minor)
	if neg {
		abs = -abs // complemento de dois: correto inclusive para math.MinInt64
	}
	s := fmt.Sprintf("%d.%02d", abs/scale, abs%scale)
	if neg {
		return "-" + s
	}
	return s
}

func (m Money) String() string {
	if !m.IsValid() {
		return "<uninitialized money>"
	}
	return m.Amount() + " " + string(m.currency)
}

// IsZero, IsPositive e IsNegative são falsos para um Money não inicializado.
func (m Money) IsZero() bool     { return m.IsValid() && m.minor == 0 }
func (m Money) IsPositive() bool { return m.IsValid() && m.minor > 0 }
func (m Money) IsNegative() bool { return m.IsValid() && m.minor < 0 }

func (m Money) compatible(o Money) error {
	if !m.IsValid() || !o.IsValid() {
		return ErrUninitialized
	}
	if m.currency != o.currency {
		return fmt.Errorf("%w: %s vs %s", ErrCurrencyMismatch, m.currency, o.currency)
	}
	return nil
}

// Add soma dois valores da mesma moeda.
func (m Money) Add(o Money) (Money, error) {
	if err := m.compatible(o); err != nil {
		return Money{}, err
	}
	if (o.minor > 0 && m.minor > math.MaxInt64-o.minor) ||
		(o.minor < 0 && m.minor < math.MinInt64-o.minor) {
		return Money{}, ErrOverflow
	}
	return Money{minor: m.minor + o.minor, currency: m.currency}, nil
}

// Sub subtrai o de m. O resultado pode ser negativo (diferenças são permitidas;
// quem exige não negatividade, como a carteira, valida por conta própria).
func (m Money) Sub(o Money) (Money, error) {
	if err := m.compatible(o); err != nil {
		return Money{}, err
	}
	if (o.minor > 0 && m.minor < math.MinInt64+o.minor) ||
		(o.minor < 0 && m.minor > math.MaxInt64+o.minor) {
		return Money{}, ErrOverflow
	}
	return Money{minor: m.minor - o.minor, currency: m.currency}, nil
}

// Neg devolve o valor com sinal invertido.
func (m Money) Neg() (Money, error) {
	if !m.IsValid() {
		return Money{}, ErrUninitialized
	}
	if m.minor == math.MinInt64 {
		return Money{}, ErrOverflow
	}
	return Money{minor: -m.minor, currency: m.currency}, nil
}

// Cmp devolve -1, 0 ou 1. Exige moedas compatíveis.
func (m Money) Cmp(o Money) (int, error) {
	if err := m.compatible(o); err != nil {
		return 0, err
	}
	switch {
	case m.minor < o.minor:
		return -1, nil
	case m.minor > o.minor:
		return 1, nil
	}
	return 0, nil
}

// Equal é verdadeiro quando ambos são válidos, têm a mesma moeda e o mesmo valor.
func (m Money) Equal(o Money) bool {
	return m.IsValid() && o.IsValid() && m.currency == o.currency && m.minor == o.minor
}

type wireMoney struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// MarshalJSON produz {"amount":"25.00","currency":"BRL"}. Aceita valores negativos
// (por exemplo, a diferença da reconciliação). Um Money não inicializado é um erro.
func (m Money) MarshalJSON() ([]byte, error) {
	if !m.IsValid() {
		return nil, ErrUninitialized
	}
	return json.Marshal(wireMoney{Amount: m.Amount(), Currency: string(m.currency)})
}

// UnmarshalJSON é estrito e destinado a ENTRADAS: aplica as regras de Parse, então
// rejeita números JSON no lugar de string, campos ausentes, valores negativos e
// qualquer forma que não seja a canônica.
func (m *Money) UnmarshalJSON(data []byte) error {
	var raw struct {
		Amount   *string `json:"amount"`
		Currency *string `json:"currency"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidAmount, err)
	}
	if raw.Amount == nil || raw.Currency == nil {
		return fmt.Errorf("%w: both amount and currency are required", ErrInvalidAmount)
	}
	parsed, err := Parse(*raw.Amount, *raw.Currency)
	if err != nil {
		return err
	}
	*m = parsed
	return nil
}
