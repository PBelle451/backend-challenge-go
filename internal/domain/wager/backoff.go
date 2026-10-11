package wager

import "time"

// Backoff devolve a espera exponencial antes da tentativa seguinte:
// base * 2^(attempt-1), limitada a max. attempt é a quantidade de tentativas já
// feitas (1 = a primeira falhou). Nunca estoura em duração. O jitter, quando
// desejado, é aplicado por quem agenda (workers), mantendo esta função determinística.
func Backoff(attempt int, base, max time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}
	if max < base {
		max = base
	}
	d := base
	for i := 1; i < attempt; i++ {
		if d >= max/2 {
			return max
		}
		d *= 2
	}
	if d > max {
		return max
	}
	return d
}

// ReferenceExhausted informa se uma espera por referência chegou ao fim: o número
// máximo de tentativas foi atingido OU o prazo (TTL) expirou.
func ReferenceExhausted(attempts, maxAttempts int, now, expiresAt time.Time) bool {
	return attempts >= maxAttempts || !now.Before(expiresAt)
}
