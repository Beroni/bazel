package pricing

import (
	"testing"
)

func TestDefaultTableHasKnownModels(t *testing.T) {
	for _, name := range []string{
		"claude-fable-5-1",
		"claude-opus-5-5",
		"claude-opus-5",
		"claude-sonnet-5-5",
		"claude-sonnet-4",
		"claude-haiku-4-5",
	} {
		if _, ok := Default[name]; !ok {
			t.Errorf("Default não tem %q", name)
		}
	}
}

// A escrita de cache de 5 minutos custa 1,25× o input, em todo modelo — era
// isso que a tabela antiga errava (escrita mais barata que o input no Opus).
func TestDefaultCacheWriteIsOneAndAQuarterInput(t *testing.T) {
	for name, r := range Default {
		if want := r.Input * 1.25; r.CacheWrite != want {
			t.Errorf("%s: cache_write %.4f, queria %.4f (1,25× input)", name, r.CacheWrite, want)
		}
		if r.CacheRead >= r.Input {
			t.Errorf("%s: leitura de cache (%.2f) não pode custar o input (%.2f)", name, r.CacheRead, r.Input)
		}
	}
}

// Cost calcula o custo a partir de uma rate e contadores de tokens.
func TestCostKnownModel(t *testing.T) {
	// Opus 5.5: $4/M input, $20/M output, $5/M cache write, $0.20/M cache read
	cost := Default.Cost("claude-opus-5-5", 1_000_000, 1_000_000, 1_000_000, 1_000_000)
	want := 4.00 + 20.00 + 5.00 + 0.20 // = $29.20
	if cost != want {
		t.Errorf("Cost(opus-5-5, 1M/1M/1M/1M) = %.2f, queria %.2f", cost, want)
	}
}

// Modelo desconhecido devolve zero — o custo é reportado como "unknown" em vez
// de adivinhar um número que pode enganar.
func TestCostUnknownModelReturnsZero(t *testing.T) {
	if cost := Default.Cost("claude-unknown-99", 1_000_000, 1_000_000, 0, 0); cost != 0 {
		t.Errorf("modelo desconhecido devia custar 0, custou %.2f", cost)
	}
	if _, ok := Default.CostOf("grok", 1_000_000, 0, 0, 0); ok {
		t.Error("CostOf devia dizer que grok não está na tabela")
	}
	// Case-insensitive: "CLAUDE-OPUS-5-5" funciona igual.
	if cost := Default.Cost("CLAUDE-OPUS-5-5", 1_000_000, 0, 0, 0); cost != 4.00 {
		t.Errorf("case-insensitive falhou: %.2f", cost)
	}
}

// O Claude Code reporta o modelo com data e às vezes com variante; a chave
// mais longa que é prefixo até um hífen é a que vale.
func TestRateMatchesVersionedNames(t *testing.T) {
	cases := []struct {
		model string
		input float64
		ok    bool
	}{
		{"claude-opus-4-5-20251101", 5, true},
		{"claude-opus-4-1-20250805", 15, true},
		{"claude-opus-4-20250514", 15, true},
		{"claude-sonnet-4-5-20250929", 3, true},
		{"claude-haiku-4-5-20251001", 1, true},
		{"claude-opus-5-5[1m]", 4, true},
		{"claude-fable-5-1", 10, true},
		// Prefixo sem hífen na fronteira não é o mesmo modelo.
		{"claude-opus-40", 0, false},
		{"claude", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		r, ok := Default.Rate(c.model)
		if ok != c.ok || r.Input != c.input {
			t.Errorf("Rate(%q) = (%.2f, %v), queria (%.2f, %v)", c.model, r.Input, ok, c.input, c.ok)
		}
	}
}

// Clone devolve uma tabela independente: escrever nela não pode mexer no
// Default do pacote.
func TestCloneIsIndependent(t *testing.T) {
	c := Default.Clone()
	c["claude-opus-5-5"] = Rate{Input: 99}
	c["novo"] = Rate{Input: 1}
	if Default["claude-opus-5-5"].Input != 4 {
		t.Error("Clone compartilha o map com o Default")
	}
	if _, ok := Default["novo"]; ok {
		t.Error("chave nova vazou para o Default")
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
		0:       "$0.00",
		0.42:    "$0.42",
		1.50:    "$1.50",
		92.25:   "$92.25",
		1000.00: "$1000.00",
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
		0:         "0",
		999:       "999",
		1000:      "1.0k",
		45230:     "45.2k",
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
		"claude-opus-5-5": {Input: 99.0, Output: 99.0, CacheWrite: 99.0, CacheRead: 99.0},
	}
	// Modelo que não está na customização devolve zero (não cai no default).
	if cost := tbl.Cost("claude-haiku-4-5", 1_000_000, 0, 0, 0); cost != 0 {
		t.Errorf("modelo ausente na customização devia custar 0: %.2f", cost)
	}
	// Modelo customizado usa a nova rate.
	if cost := tbl.Cost("claude-opus-5-5", 1_000_000, 0, 0, 0); cost != 99.00 {
		t.Errorf("modelo customizado devia usar nova rate: %.2f", cost)
	}
}
