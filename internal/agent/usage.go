package agent

import (
	"fmt"
	"strconv"
	"strings"
)

// Usage é o que um agente consumiu do modelo. Vem do evento final do
// stream-json, que já soma os sub-agentes que ele tiver disparado — um
// executável que não fala esse formato devolve tudo zerado, e a interface
// simplesmente não mostra gasto nenhum.
type Usage struct {
	InputTokens  int     `json:"input_tokens,omitempty"`
	OutputTokens int     `json:"output_tokens,omitempty"`
	CacheWrite   int     `json:"cache_write_tokens,omitempty"`
	CacheRead    int     `json:"cache_read_tokens,omitempty"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
}

// Total é tudo que passou pelo modelo. O cache entra na conta: lido ou
// escrito, é contexto que o agente consumiu para fazer o review.
func (u Usage) Total() int {
	return u.InputTokens + u.OutputTokens + u.CacheWrite + u.CacheRead
}

// Empty diz que não há gasto a mostrar.
func (u Usage) Empty() bool { return u.Total() == 0 && u.CostUSD == 0 }

// add soma outro gasto a este — é como os passos de uma pipeline viram um
// número só no fim do review.
func (u *Usage) add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.CacheWrite += o.CacheWrite
	u.CacheRead += o.CacheRead
	u.CostUSD += o.CostUSD
}

// String é o gasto em uma linha, do jeito que ele aparece no fim do review.
func (u Usage) String() string {
	if u.Empty() {
		return ""
	}
	s := FormatTokens(u.Total()) + " tokens"
	if u.InputTokens+u.OutputTokens > 0 {
		s += fmt.Sprintf(" (entrada %s · saída %s · cache %s)",
			FormatTokens(u.InputTokens), FormatTokens(u.OutputTokens),
			FormatTokens(u.CacheRead+u.CacheWrite))
	}
	if u.CostUSD > 0 {
		s += fmt.Sprintf(" · $%.2f", u.CostUSD)
	}
	return s
}

// FormatTokens abrevia a contagem: um review da frota queima milhões de
// tokens, e "1,8M" se lê melhor do que os sete dígitos.
func FormatTokens(n int) string {
	switch {
	case n < 0:
		return "0"
	case n < 1000:
		return strconv.Itoa(n)
	case n < 1_000_000:
		return trimZero(float64(n)/1000) + "k"
	default:
		return trimZero(float64(n)/1_000_000) + "M"
	}
}

// trimZero arredonda numa casa decimal, com vírgula, e some com o ",0".
func trimZero(v float64) string {
	s := strings.Replace(strconv.FormatFloat(v, 'f', 1, 64), ".", ",", 1)
	return strings.TrimSuffix(s, ",0")
}
