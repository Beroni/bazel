// Package pricing mantém a tabela de preços por modelo e calcula o custo
// estimado de uma review a partir de agent.Usage.
//
// Os preços mudam — um modelo novo sai, uma tarifa muda — e o binário não
// pode precisar de rebuild. Por isso a tabela é um default shipped com o
// código e pode ser sobrescrita em config.yaml.
package pricing

import (
	"fmt"
	"strings"
)

// Rate é o custo por 1M de tokens, em USD, para uma categoria de uso.
// CacheWrite é a escrita de cache de 5 minutos (1,25× o input); CacheRead é a
// leitura, que varia por modelo.
type Rate struct {
	Input      float64 `yaml:"input"`
	Output     float64 `yaml:"output"`
	CacheWrite float64 `yaml:"cache_write"`
	CacheRead  float64 `yaml:"cache_read"`
}

// Table is the price table, keyed by model name (case-insensitive). A key also
// covers the dated releases of that model: Claude Code reports
// `claude-opus-4-5-20251101`, and the price comes from `claude-opus-4-5`. A new
// minor of a family (`claude-opus-4-9`) does not inherit the family price: it
// needs its own row, otherwise it keeps the cost the provider reported.
type Table map[string]Rate

// Default é a tabela usada quando o usuário não configurou nada. Preços de
// API da Anthropic por 1M de tokens (setembro de 2026); a escrita de cache é
// 1,25× o input e a leitura é 0,1× — com as exceções que cada linha anota.
var Default = Table{
	// Fable 5.1: leitura de cache a 0,025× do input.
	"claude-fable-5-1": {Input: 10, Output: 50, CacheWrite: 12.50, CacheRead: 0.25},
	"claude-fable-5":   {Input: 10, Output: 50, CacheWrite: 12.50, CacheRead: 1.00},
	// Opus 5.5: leitura de cache a 0,05× do input.
	"claude-opus-5-5": {Input: 4, Output: 20, CacheWrite: 5.00, CacheRead: 0.20},
	"claude-opus-5":   {Input: 5, Output: 25, CacheWrite: 6.25, CacheRead: 0.50},
	"claude-opus-4-8": {Input: 5, Output: 25, CacheWrite: 6.25, CacheRead: 0.50},
	"claude-opus-4-7": {Input: 5, Output: 25, CacheWrite: 6.25, CacheRead: 0.50},
	"claude-opus-4-6": {Input: 5, Output: 25, CacheWrite: 6.25, CacheRead: 0.50},
	"claude-opus-4-5": {Input: 5, Output: 25, CacheWrite: 6.25, CacheRead: 0.50},
	// Opus 4 and 4.1 cost the same.
	"claude-opus-4-1":   {Input: 15, Output: 75, CacheWrite: 18.75, CacheRead: 1.50},
	"claude-opus-4":     {Input: 15, Output: 75, CacheWrite: 18.75, CacheRead: 1.50},
	"claude-sonnet-5-5": {Input: 2, Output: 10, CacheWrite: 2.50, CacheRead: 0.20},
	"claude-sonnet-5":   {Input: 2, Output: 10, CacheWrite: 2.50, CacheRead: 0.20},
	// Sonnet 4, 4.5 and 4.6 cost the same.
	"claude-sonnet-4-6": {Input: 3, Output: 15, CacheWrite: 3.75, CacheRead: 0.30},
	"claude-sonnet-4-5": {Input: 3, Output: 15, CacheWrite: 3.75, CacheRead: 0.30},
	"claude-sonnet-4":   {Input: 3, Output: 15, CacheWrite: 3.75, CacheRead: 0.30},
	"claude-haiku-4-5":  {Input: 1, Output: 5, CacheWrite: 1.25, CacheRead: 0.10},
}

// Rate finds the price of a model. The exact name wins; failing that, a dated
// name (`claude-opus-4-5-20251101`) is priced by the key without the date
// (`claude-opus-4-5`). No other suffix matches: `claude-opus-4-9` is not
// `claude-opus-4`, and pricing it as such would replace the provider's cost
// with a guess. A bracketed variant suffix (`[1m]`) is ignored.
func (t Table) Rate(model string) (Rate, bool) {
	name := strings.ToLower(strings.TrimSpace(model))
	if i := strings.IndexByte(name, '['); i >= 0 {
		name = name[:i]
	}
	if name == "" {
		return Rate{}, false
	}
	if rate, ok := t.lookup(name); ok {
		return rate, true
	}
	if base, ok := undated(name); ok {
		return t.lookup(base)
	}
	return Rate{}, false
}

// lookup finds the key equal to name, ignoring case. The exact key wins over
// one with different case, which the loop would find in random map order.
func (t Table) lookup(name string) (Rate, bool) {
	if rate, ok := t[name]; ok {
		return rate, true
	}
	for key, rate := range t {
		if strings.ToLower(key) == name {
			return rate, true
		}
	}
	return Rate{}, false
}

// undated strips a `-YYYYMMDD` date suffix from name; ok is false without one.
func undated(name string) (string, bool) {
	const suffix = len("-20060102")
	if len(name) <= suffix || name[len(name)-suffix] != '-' {
		return "", false
	}
	for _, c := range name[len(name)-suffix+1:] {
		if c < '0' || c > '9' {
			return "", false
		}
	}
	return name[:len(name)-suffix], true
}

// Cost calcula o custo em USD para um usage, usando a rate do modelo.
// Modelo desconhecido devolve zero — o custo é reportado como "unknown" em vez
// de adivinhar um número que pode enganar. Quem precisa distinguir "grátis"
// de "não sei" usa CostOf.
func (t Table) Cost(model string, input, output, cacheWrite, cacheRead int) float64 {
	c, _ := t.CostOf(model, input, output, cacheWrite, cacheRead)
	return c
}

// CostOf é o Cost que diz se o modelo estava na tabela.
func (t Table) CostOf(model string, input, output, cacheWrite, cacheRead int) (float64, bool) {
	rate, ok := t.Rate(model)
	if !ok {
		return 0, false
	}
	return costOf(rate, input, output, cacheWrite, cacheRead), true
}

// With é esta tabela com as entradas de over por cima, numa cópia. As chaves
// de over entram em minúsculas, para que `Claude-Opus-5-5` no config.yaml
// troque o preço de `claude-opus-5-5` em vez de disputar com ele.
func (t Table) With(over Table) Table {
	c := t.Clone()
	for k, v := range over {
		c[strings.ToLower(strings.TrimSpace(k))] = v
	}
	return c
}

// Clone copia a tabela. Default é uma variável do pacote: entregá-la direto
// para a config deixaria o yaml do usuário escrever nela.
func (t Table) Clone() Table {
	c := make(Table, len(t))
	for k, v := range t {
		c[k] = v
	}
	return c
}

// costOf computes the cost from a rate and token counts; prices are per 1M
// tokens. The result is not rounded: the steps of a review are summed later,
// and rounding each one would zero out the cheap ones. Cents are for display.
func costOf(rate Rate, input, output, cacheWrite, cacheRead int) float64 {
	inputCost := float64(input) * rate.Input / 1_000_000
	outputCost := float64(output) * rate.Output / 1_000_000
	cacheWriteCost := float64(cacheWrite) * rate.CacheWrite / 1_000_000
	cacheReadCost := float64(cacheRead) * rate.CacheRead / 1_000_000
	return inputCost + outputCost + cacheWriteCost + cacheReadCost
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
