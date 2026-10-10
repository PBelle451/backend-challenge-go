package money_test

import (
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/PBelle451/backend-challenge-go/internal/domain/money"
)

const (
	maxAmount = "92233720368547758.07" // math.MaxInt64 em centavos
	minAmount = "-92233720368547758.08"
)

func mustMoney(t *testing.T, minor int64, cur string) money.Money {
	t.Helper()
	c, err := money.NewCurrency(cur)
	if err != nil {
		t.Fatalf("NewCurrency(%q): %v", cur, err)
	}
	m, err := money.New(minor, c)
	if err != nil {
		t.Fatalf("New(%d, %s): %v", minor, cur, err)
	}
	return m
}

func TestParse_Valid(t *testing.T) {
	tests := []struct {
		in    string
		minor int64
	}{
		{"0.00", 0},
		{"0.01", 1},
		{"0.50", 50},
		{"1.00", 100},
		{"25.00", 2500},
		{"1000.50", 100050},
		{"80.00", 8000},
		{maxAmount, math.MaxInt64},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			m, err := money.Parse(tc.in, "BRL")
			if err != nil {
				t.Fatalf("Parse(%q) error: %v", tc.in, err)
			}
			if m.Minor() != tc.minor {
				t.Errorf("Minor() = %d, want %d", m.Minor(), tc.minor)
			}
			if m.Amount() != tc.in {
				t.Errorf("Amount() = %q, want %q", m.Amount(), tc.in)
			}
			if m.Currency() != "BRL" {
				t.Errorf("Currency() = %q, want BRL", m.Currency())
			}
		})
	}
}

func TestParse_Invalid(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want error
	}{
		{"vazio", "", money.ErrInvalidAmount},
		{"so espaco", " ", money.ErrInvalidAmount},
		{"sem casas decimais", "25", money.ErrInvalidAmount},
		{"uma casa decimal", "25.5", money.ErrInvalidAmount},
		{"escala excedente", "25.001", money.ErrInvalidAmount},
		{"escala excedente com zeros", "25.000", money.ErrInvalidAmount},
		{"sem parte inteira", ".50", money.ErrInvalidAmount},
		{"ponto sem decimais", "25.", money.ErrInvalidAmount},
		{"virgula decimal", "25,00", money.ErrInvalidAmount},
		{"separador de milhar", "1,000.00", money.ErrInvalidAmount},
		{"sublinhado", "1_000.00", money.ErrInvalidAmount},
		{"sinal positivo", "+25.00", money.ErrInvalidAmount},
		{"notacao cientifica", "1e3", money.ErrInvalidAmount},
		{"notacao cientifica com ponto", "1.00e3", money.ErrInvalidAmount},
		{"notacao cientifica maiuscula", "1E3", money.ErrInvalidAmount},
		{"NaN", "NaN", money.ErrInvalidAmount},
		{"Infinity", "Infinity", money.ErrInvalidAmount},
		{"-Infinity", "-Infinity", money.ErrNegativeAmount},
		{"hexadecimal", "0x10.00", money.ErrInvalidAmount},
		{"espaco no inicio", " 25.00", money.ErrInvalidAmount},
		{"espaco no fim", "25.00 ", money.ErrInvalidAmount},
		{"quebra de linha", "25.00\n", money.ErrInvalidAmount},
		{"espaco no meio", "2 5.00", money.ErrInvalidAmount},
		{"zeros a esquerda", "025.00", money.ErrInvalidAmount},
		{"zeros a esquerda no zero", "00.00", money.ErrInvalidAmount},
		{"digitos arabes", "٢٥.٠٠", money.ErrInvalidAmount},
		{"digitos fullwidth", "２５.００", money.ErrInvalidAmount},
		{"negativo", "-1.00", money.ErrNegativeAmount},
		{"menos zero", "-0.00", money.ErrNegativeAmount},
		{"overflow por 1 centavo", "92233720368547758.08", money.ErrOverflow},
		{"overflow grande", "99999999999999999999.99", money.ErrOverflow},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := money.Parse(tc.in, "BRL")
			if err == nil {
				t.Fatalf("Parse(%q) = %v, want error", tc.in, m)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("Parse(%q) error = %v, want errors.Is %v", tc.in, err, tc.want)
			}
			if m.IsValid() {
				t.Errorf("Parse(%q) returned a valid Money alongside an error", tc.in)
			}
		})
	}
}

func TestNewCurrency(t *testing.T) {
	for _, ok := range []string{"BRL", "USD", "EUR", "JPY"} {
		if _, err := money.NewCurrency(ok); err != nil {
			t.Errorf("NewCurrency(%q) error: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "BR", "BRLL", "brl", "Brl", "B1L", "BR ", "R$", "BRĽ"} {
		_, err := money.NewCurrency(bad)
		if !errors.Is(err, money.ErrInvalidCurrency) {
			t.Errorf("NewCurrency(%q) error = %v, want ErrInvalidCurrency", bad, err)
		}
	}
	if _, err := money.Parse("1.00", "brl"); !errors.Is(err, money.ErrInvalidCurrency) {
		t.Errorf("Parse with lowercase currency: error = %v, want ErrInvalidCurrency", err)
	}
}

func TestZeroValueIsRejected(t *testing.T) {
	var z money.Money
	ok := mustMoney(t, 100, "BRL")

	if z.IsValid() || z.IsZero() || z.IsPositive() || z.IsNegative() {
		t.Error("zero value must not be valid nor report any sign")
	}
	if z.Equal(z) {
		t.Error("an uninitialized Money must not be equal to itself")
	}
	if _, err := z.Add(ok); !errors.Is(err, money.ErrUninitialized) {
		t.Errorf("Add on zero value: %v", err)
	}
	if _, err := ok.Add(z); !errors.Is(err, money.ErrUninitialized) {
		t.Errorf("Add with zero value operand: %v", err)
	}
	if _, err := ok.Sub(z); !errors.Is(err, money.ErrUninitialized) {
		t.Errorf("Sub with zero value operand: %v", err)
	}
	if _, err := z.Neg(); !errors.Is(err, money.ErrUninitialized) {
		t.Errorf("Neg on zero value: %v", err)
	}
	if _, err := z.Cmp(ok); !errors.Is(err, money.ErrUninitialized) {
		t.Errorf("Cmp on zero value: %v", err)
	}
	if _, err := json.Marshal(z); !errors.Is(err, money.ErrUninitialized) {
		t.Errorf("Marshal of zero value: %v", err)
	}
	if _, err := money.New(1, ""); !errors.Is(err, money.ErrInvalidCurrency) {
		t.Errorf("New with empty currency: %v", err)
	}
}

func TestZeroByCurrency(t *testing.T) {
	z, err := money.Zero("USD")
	if err != nil {
		t.Fatal(err)
	}
	if !z.IsValid() || !z.IsZero() || z.Currency() != "USD" || z.Amount() != "0.00" {
		t.Errorf("Zero(USD) = %v", z)
	}
	if _, err := money.Zero("usd"); !errors.Is(err, money.ErrInvalidCurrency) {
		t.Errorf("Zero with invalid currency: %v", err)
	}
}

func TestAddSubNeg(t *testing.T) {
	a := mustMoney(t, 10000, "BRL") // 100.00
	b := mustMoney(t, 8000, "BRL")  // 80.00

	sum, err := a.Add(b)
	if err != nil || sum.Minor() != 18000 {
		t.Fatalf("Add = %v, %v", sum, err)
	}
	diff, err := a.Sub(b)
	if err != nil || diff.Minor() != 2000 || diff.Amount() != "20.00" {
		t.Fatalf("Sub = %v, %v", diff, err)
	}
	neg, err := b.Sub(a) // diferenças negativas são permitidas
	if err != nil || neg.Minor() != -2000 || neg.Amount() != "-20.00" || !neg.IsNegative() {
		t.Fatalf("Sub negativo = %v, %v", neg, err)
	}
	n, err := a.Neg()
	if err != nil || n.Minor() != -10000 {
		t.Fatalf("Neg = %v, %v", n, err)
	}
	back, err := n.Neg()
	if err != nil || !back.Equal(a) {
		t.Fatalf("Neg(Neg(a)) = %v, %v", back, err)
	}
	// imutabilidade: operandos não mudam
	if a.Minor() != 10000 || b.Minor() != 8000 {
		t.Error("operands were mutated")
	}
}

func TestCurrencyMismatch(t *testing.T) {
	brl := mustMoney(t, 100, "BRL")
	usd := mustMoney(t, 100, "USD")

	if _, err := brl.Add(usd); !errors.Is(err, money.ErrCurrencyMismatch) {
		t.Errorf("Add: %v", err)
	}
	if _, err := brl.Sub(usd); !errors.Is(err, money.ErrCurrencyMismatch) {
		t.Errorf("Sub: %v", err)
	}
	if _, err := brl.Cmp(usd); !errors.Is(err, money.ErrCurrencyMismatch) {
		t.Errorf("Cmp: %v", err)
	}
	if brl.Equal(usd) {
		t.Error("same amount in different currencies must not be equal")
	}
}

func TestOverflow(t *testing.T) {
	max := mustMoney(t, math.MaxInt64, "BRL")
	min := mustMoney(t, math.MinInt64, "BRL")
	one := mustMoney(t, 1, "BRL")
	minusOne := mustMoney(t, -1, "BRL")
	zero := mustMoney(t, 0, "BRL")

	tests := []struct {
		name string
		op   func() (money.Money, error)
	}{
		{"max + 1", func() (money.Money, error) { return max.Add(one) }},
		{"min + -1", func() (money.Money, error) { return min.Add(minusOne) }},
		{"min - 1", func() (money.Money, error) { return min.Sub(one) }},
		{"max - -1", func() (money.Money, error) { return max.Sub(minusOne) }},
		{"zero - min", func() (money.Money, error) { return zero.Sub(min) }},
		{"neg(min)", func() (money.Money, error) { return min.Neg() }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := tc.op()
			if !errors.Is(err, money.ErrOverflow) {
				t.Fatalf("error = %v (value %v), want ErrOverflow", err, m)
			}
			if m.IsValid() {
				t.Error("an overflowing operation must not return a valid Money")
			}
		})
	}

	// limites exatos NÃO são overflow
	if got, err := max.Sub(one); err != nil || got.Minor() != math.MaxInt64-1 {
		t.Errorf("max - 1 = %v, %v", got, err)
	}
	if got, err := min.Add(one); err != nil || got.Minor() != math.MinInt64+1 {
		t.Errorf("min + 1 = %v, %v", got, err)
	}
	if got, err := max.Add(zero); err != nil || !got.Equal(max) {
		t.Errorf("max + 0 = %v, %v", got, err)
	}
	if got, err := max.Add(min); err != nil || got.Minor() != -1 {
		t.Errorf("max + min = %v, %v", got, err)
	}
}

func TestAmountFormatting(t *testing.T) {
	tests := []struct {
		minor int64
		want  string
	}{
		{0, "0.00"},
		{1, "0.01"},
		{-1, "-0.01"},
		{5, "0.05"},
		{-5, "-0.05"},
		{100, "1.00"},
		{2500, "25.00"},
		{-2000, "-20.00"},
		{math.MaxInt64, maxAmount},
		{math.MinInt64, minAmount}, // sem overflow ao formatar o menor valor
	}
	for _, tc := range tests {
		if got := mustMoney(t, tc.minor, "BRL").Amount(); got != tc.want {
			t.Errorf("Amount(%d) = %q, want %q", tc.minor, got, tc.want)
		}
	}
	if got := mustMoney(t, 2500, "BRL").String(); got != "25.00 BRL" {
		t.Errorf("String() = %q", got)
	}
}

func TestCmpAndSigns(t *testing.T) {
	a := mustMoney(t, 100, "BRL")
	b := mustMoney(t, 200, "BRL")
	for _, tc := range []struct {
		x, y money.Money
		want int
	}{{a, b, -1}, {b, a, 1}, {a, a, 0}} {
		got, err := tc.x.Cmp(tc.y)
		if err != nil || got != tc.want {
			t.Errorf("Cmp(%v, %v) = %d, %v; want %d", tc.x, tc.y, got, err, tc.want)
		}
	}
	if !mustMoney(t, 0, "BRL").IsZero() || !a.IsPositive() || !mustMoney(t, -1, "BRL").IsNegative() {
		t.Error("sign helpers returned unexpected results")
	}
	if !a.Equal(mustMoney(t, 100, "BRL")) {
		t.Error("equal values must be Equal")
	}
}

func TestJSON_Marshal(t *testing.T) {
	got, err := json.Marshal(mustMoney(t, 2500, "BRL"))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"amount":"25.00","currency":"BRL"}`; string(got) != want {
		t.Errorf("Marshal = %s, want %s", got, want)
	}

	// negativos podem ser serializados (ex.: diferença da reconciliação)
	got, err = json.Marshal(mustMoney(t, -5, "BRL"))
	if err != nil || string(got) != `{"amount":"-0.05","currency":"BRL"}` {
		t.Errorf("Marshal negative = %s, %v", got, err)
	}
}

func TestJSON_Unmarshal(t *testing.T) {
	var m money.Money
	if err := json.Unmarshal([]byte(`{"amount":"25.00","currency":"BRL"}`), &m); err != nil {
		t.Fatal(err)
	}
	if m.Minor() != 2500 || m.Currency() != "BRL" {
		t.Errorf("Unmarshal = %v", m)
	}

	// campos embutidos em um DTO maior
	var dto struct {
		Money money.Money `json:"money"`
	}
	if err := json.Unmarshal([]byte(`{"money":{"amount":"80.00","currency":"BRL"}}`), &dto); err != nil {
		t.Fatal(err)
	}
	if dto.Money.Minor() != 8000 {
		t.Errorf("embedded Money = %v", dto.Money)
	}

	invalid := map[string]string{
		"amount como numero JSON (float)": `{"amount":25.00,"currency":"BRL"}`,
		"amount como inteiro JSON":        `{"amount":25,"currency":"BRL"}`,
		"amount ausente":                  `{"currency":"BRL"}`,
		"currency ausente":                `{"amount":"25.00"}`,
		"objeto vazio":                    `{}`,
		"null":                            `null`,
		"string no lugar do objeto":       `"25.00 BRL"`,
		"valor negativo":                  `{"amount":"-1.00","currency":"BRL"}`,
		"escala excedente":                `{"amount":"1.001","currency":"BRL"}`,
		"notacao cientifica":              `{"amount":"1e3","currency":"BRL"}`,
		"moeda minuscula":                 `{"amount":"1.00","currency":"brl"}`,
		"moeda como numero":               `{"amount":"1.00","currency":123}`,
		"overflow":                        `{"amount":"92233720368547758.08","currency":"BRL"}`,
	}
	for name, body := range invalid {
		t.Run(name, func(t *testing.T) {
			var got money.Money
			if err := json.Unmarshal([]byte(body), &got); err == nil {
				t.Errorf("Unmarshal(%s) = %v, want error", body, got)
			}
			if got.IsValid() {
				t.Errorf("Unmarshal(%s) left a valid Money after an error", body)
			}
		})
	}
}

func TestJSON_RoundTrip(t *testing.T) {
	for _, minor := range []int64{0, 1, 99, 100, 2500, 100050, math.MaxInt64} {
		in := mustMoney(t, minor, "BRL")
		data, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		var out money.Money
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatalf("Unmarshal(%s): %v", data, err)
		}
		if !in.Equal(out) {
			t.Errorf("round trip: %v -> %s -> %v", in, data, out)
		}
	}
}

// Propriedade: (a + b) - b == a, sempre que não houver overflow.
func TestAddSubInverse(t *testing.T) {
	values := []int64{0, 1, -1, 99, 100, 123456789, -987654321, math.MaxInt64 / 2, math.MinInt64 / 2}
	for _, x := range values {
		for _, y := range values {
			a, b := mustMoney(t, x, "BRL"), mustMoney(t, y, "BRL")
			sum, err := a.Add(b)
			if err != nil {
				continue
			}
			back, err := sum.Sub(b)
			if err != nil {
				t.Errorf("(%d + %d) - %d: %v", x, y, y, err)
				continue
			}
			if !back.Equal(a) {
				t.Errorf("(%d + %d) - %d = %d, want %d", x, y, y, back.Minor(), x)
			}
		}
	}
}

// Propriedade: tudo que Parse aceita é a forma canônica e volta idêntico em Amount().
func FuzzParseCanonicalRoundTrip(f *testing.F) {
	for _, seed := range []string{"0.00", "25.00", "1000.50", maxAmount, "", "NaN", "1e3", "-1.00", "25", "025.00", "٢٥.٠٠"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		m, err := money.Parse(s, "BRL")
		if err != nil {
			if m.IsValid() {
				t.Fatalf("Parse(%q) returned an error and a valid Money", s)
			}
			return
		}
		if m.IsNegative() {
			t.Fatalf("Parse(%q) accepted a negative value", s)
		}
		if got := m.Amount(); got != s {
			t.Fatalf("Parse(%q).Amount() = %q: accepted input is not canonical", s, got)
		}
	})
}
