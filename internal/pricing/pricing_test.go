package pricing

import (
	"testing"
)

func TestDefaultTableHasKnownModels(t *testing.T) {
	for _, name := range []string{
		"claude-opus-5",
		"claude-haiku-4-5",
		"claude-sonnet-4",
		"claude-opus-4",
		"claude-sonnet-3-5-20241022",
	} {
		if _, ok := Default[name]; !ok {
			t.Errorf("Default não tem %q", name)
		}
	}
}

// Cost calcula o custo a partir de uma rate e contadores de tokens.
func TestCostKnownModel(t *testing.T) {
	// Opus 5: $15/M input, $75/M output, $1.50/M cache write, $0.75/M cache read
	// 1M input = $15, 1M output = $75, 1M cache write = $1.50, 1M cache read = $0.75
	cost := Default.Cost("claude-opus-5", 1_000_000, 1_000_000, 1_000_000, 1_000_000)
	want := 15.00 + 75.00 + 1.50 + 0.75 // = $92.25
	if cost != want {
		t.Errorf("Cost(opus-5, 1M/1M/1M/1M) = %.2f, queria %.2f", cost, want)
	}
}

// Modelo desconhecido devolve zero — o custo é reportado como "unknown" em vez
// de adivinhar um número que pode enganar.
func TestCostUnknownModelReturnsZero(t *testing.T) {
	if cost := Default.Cost("claude-unknown-99", 1_000_000, 1_000_000, 0, 0); cost != 0 {
		t.Errorf("modelo desconhecido devia custar 0, custou %.2f", cost)
	}
	// Case-insensitive: "CLAUDE-OPUS-5" funciona igual.
	if cost := Default.Cost("CLAUDE-OPUS-5", 1_000_000, 0, 0, 0); cost != 15.00 {
		t.Errorf("case-insensitive falhou: %.2f", cost)
	}
}

// Sem tokens, o custo é zero.
func TestCostZeroTokens(t *testing.T) {
	if cost := Default.Cost("claude-opus-5", 0, 0, 0, 0); cost != 0 {
		t.Errorf("sem tokens o custo devia ser 0, foi %.2f", cost)
	}
}

// roundCents arredonda para 2 casas decimais, sem estourar de ponto flutuante.
// math.Round usa "banker's rounding" (round to even) para halfway cases.
// NOTA: Devido à precisão de ponto flutuante, 9.995 * 100 = 999.4999... arredonda para 999 (9.99),
// enquanto 9.985 * 100 = 998.5 arredonda para 999 (9.99).
func TestRoundCents(t *testing.T) {
	cases := []struct {
		in   float64
		want float64
	}{
		{0, 0},
		{0.001, 0},
		{0.005, 0.01},
		{0.014, 0.01},
		{0.015, 0.02},
		{1.234, 1.23},
		// Devido a precisão de ponto flutuante: 9.995 * 100 = 999.4999... -> 999 (9.99)
		{9.995, 9.99},
		{9.994, 9.99},
		{9.996, 10.00},
		// 9.985 * 100 = 998.5 (exato) -> 999 (banker's: even) / 100 = 9.99
		{9.985, 9.99},
		{9.975, 9.98},
	}
	for _, c := range cases {
		if got := roundCents(c.in); got != c.want {
			t.Errorf("roundCents(%f) = %f, queria %f", c.in, got, c.want)
		}
	}
}

// FormatUSD formata um valor em dólar, com vírgula decimal e 2 casas.
func TestFormatUSD(t *testing.T) {
	cases := map[float64]string{
		0:        "$0.00",
		0.42:     "$0.42",
		1.50:     "$1.50",
		92.25:    "$92.25",
		1000.00:  "$1000.00",
	}
	for in, want := range cases {
		if got := FormatUSD(in); got != want {
			t.Errorf("FormatUSD(%f) = %q, queria %q", in, got, want)
		}
	}
}

// FormatTokens abrevia a contagem com sufixo k/M.
func TestFormatTokens(t *testing.T) {
	cases := map[int]string{
		0:        "0",
		999:      "999",
		1000:     "1.0k",
		45230:    "45.2k",
		1_000_000: "1.0M",
		1_800_000: "1.8M",
	}
	for in, want := range cases {
		if got := FormatTokens(in); got != want {
			t.Errorf("FormatTokens(%d) = %q, queria %q", in, got, want)
		}
	}
}

// Tabela customizada substitui completamente o default (não cai no default).
func TestCustomTableOverridesDefault(t *testing.T) {
	tbl := Table{
		"claude-opus-5": {Input: 99.0, Output: 99.0, CacheWrite: 99.0, CacheRead: 99.0},
	}
	// Modelo que não está na customização devolve zero (não cai no default).
	if cost := tbl.Cost("claude-haiku-4-5", 1_000_000, 0, 0, 0); cost != 0 {
		t.Errorf("modelo ausente na customização devia custar 0: %.2f", cost)
	}
	// Modelo customizado usa a nova rate.
	if cost := tbl.Cost("claude-opus-5", 1_000_000, 0, 0, 0); cost != 99.00 {
		t.Errorf("modelo customizado devia usar nova rate: %.2f", cost)
	}
}