// Package pricing mantém a tabela de preços por modelo e calcula o custo
// estimado de uma review a partir de agent.Usage.
//
// Os preços mudam — um modelo novo sai, uma tarifa muda — e o binário não
// pode precisar de rebuild. Por isso a tabela é um default shipped com o
// código e pode ser sobrescrita em config.yaml.
package pricing

import (
	"fmt"
	"math"
	"strings"
)

// Rate é o custo por 1M de tokens, em USD, para uma categoria de uso.
// CacheWrite e CacheRead podem ser diferentes (ex: $1.50/M write, $0.75/M read).
type Rate struct {
	Input       float64 `yaml:"input"`
	Output      float64 `yaml:"output"`
	CacheWrite  float64 `yaml:"cache_write"`
	CacheRead   float64 `yaml:"cache_read"`
}

// Table é a tabela de preços, indexada pelo nome do modelo (case-insensitive).
type Table map[string]Rate

// Default é a tabela usada quando o usuário não configurou nada.
// Valores de referência para Claude Opus 4.5 e Haiku 4.5 (outubro 2025).
var Default = Table{
	// Claude Opus 4.5 — $15/M input, $75/M output, $1.50/M cache write, $0.75/M cache read
	"claude-opus-5": {
		Input:      15.00,
		Output:     75.00,
		CacheWrite: 1.50,
		CacheRead:  0.75,
	},
	// Claude Haiku 4.5 — $1/M input, $5/M output, $1.25/M cache write, $0.10/M cache read
	"claude-haiku-4-5": {
		Input:      1.00,
		Output:     5.00,
		CacheWrite: 1.25,
		CacheRead:  0.10,
	},
	// Claude Sonnet 4 — $3/M input, $15/M output, $0.30/M cache write, $0.15/M cache read
	"claude-sonnet-4": {
		Input:      3.00,
		Output:     15.00,
		CacheWrite: 0.30,
		CacheRead:  0.15,
	},
	// Claude Opus 4 — $15/M input, $75/M output, $1.875/M cache write, $0.9375/M cache read
	"claude-opus-4": {
		Input:      15.00,
		Output:     75.00,
		CacheWrite: 1.875,
		CacheRead:  0.9375,
	},
	// Claude Sonnet 3.5 — $3/M input, $15/M output, $0.30/M cache write, $0.15/M cache read
	"claude-sonnet-3-5-20241022": {
		Input:      3.00,
		Output:     15.00,
		CacheWrite: 0.30,
		CacheRead:  0.15,
	},
}

// Cost calcula o custo em USD para um usage, usando a rate do modelo.
// Modelo desconhecido devolve zero — o custo é reportado como "unknown" em vez
// de adivinhar um número que pode enganar.
func (t Table) Cost(model string, input, output, cacheWrite, cacheRead int) float64 {
	rate, ok := t[strings.ToLower(model)]
	if !ok {
		return 0
	}
	return costOf(rate, input, output, cacheWrite, cacheRead)
}

// costOf calcula o custo a partir de uma rate e contadores de tokens.
// Preços são por 1M de tokens; o custo é arredondado para centavos.
func costOf(rate Rate, input, output, cacheWrite, cacheRead int) float64 {
	inputCost := float64(input) * rate.Input / 1_000_000
	outputCost := float64(output) * rate.Output / 1_000_000
	cacheWriteCost := float64(cacheWrite) * rate.CacheWrite / 1_000_000
	cacheReadCost := float64(cacheRead) * rate.CacheRead / 1_000_000
	return roundCents(inputCost + outputCost + cacheWriteCost + cacheReadCost)
}

// roundCents arredonda para 2 casas decimais, sem estourar de ponto flutuante.
// Usa math.Round para evitar problemas de precisão de ponto flutuante.
func roundCents(v float64) float64 {
	return math.Round(v*100) / 100
}

// FormatUSD formata um valor em dólar, com vírgula decimal e 2 casas.
func FormatUSD(v float64) string {
	return fmt.Sprintf("$%.2f", v)
}

// FormatTokens formata a contagem de tokens com sufixo k/M.
func FormatTokens(n int) string {
	switch {
	case n < 0:
		return "0"
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 1_000_000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
}