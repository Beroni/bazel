package agent

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/beroni/bazel/internal/pricing"
)

// Usage é o que um agente consumiu do modelo. Vem do evento final do
// stream-json, que já soma os sub-agentes que ele tiver disparado — um
// executável que não fala esse formato devolve tudo zerado, e a interface
// simplesmente não mostra gasto nenhum.
//
// Model é o nome do modelo principal que rodou. É o que a tabela de preços
// usa para calcular o custo: um modelo desconhecido cai em tokens-only, sem
// tentar adivinhar um número que pode enganar.
type Usage struct {
	InputTokens  int     `json:"input_tokens,omitempty"`
	OutputTokens int     `json:"output_tokens,omitempty"`
	CacheWrite   int     `json:"cache_write_tokens,omitempty"`
	CacheRead    int     `json:"cache_read_tokens,omitempty"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
	Model        string  `json:"model,omitempty"`
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

// Plus é a soma de dois gastos, sem mexer em nenhum dos dois. Publicar um
// review depois de produzi-lo é o mesmo card gastando duas vezes, e o número
// que ele mostra tem de ser o das duas.
func (u Usage) Plus(o Usage) Usage {
	u.add(o)
	return u
}

// String é o gasto em uma linha, do jeito que ele aparece no fim do review.
func (u Usage) String() string {
	if u.Empty() {
		return ""
	}
	s := FormatTokens(u.Total()) + " tokens"
	if u.InputTokens+u.OutputTokens > 0 {
		s += fmt.Sprintf(" (in %s · out %s · cache %s)",
			FormatTokens(u.InputTokens), FormatTokens(u.OutputTokens),
			FormatTokens(u.CacheRead+u.CacheWrite))
	}
	if u.CostUSD > 0 {
		s += fmt.Sprintf(" · $%.2f", u.CostUSD)
	}
	return s
}

// StringWithCost é o gasto em uma linha, garantindo que o custo apareça
// mesmo se não veio do stream (usando pricing table). Um custo que já veio —
// do provedor ou do Runner, que já passou pela tabela — não é refeito: aqui
// só se preenche o que faltou.
func (u Usage) StringWithCost(pricing *pricing.Table) string {
	if u.Empty() {
		return ""
	}
	if pricing != nil && u.CostUSD == 0 {
		u.CostUSD = pricing.Cost(u.Model, u.InputTokens, u.OutputTokens, u.CacheWrite, u.CacheRead)
	}
	s := FormatTokens(u.Total()) + " tokens"
	if u.InputTokens+u.OutputTokens > 0 {
		s += fmt.Sprintf(" (in %s · out %s · cache %s)",
			FormatTokens(u.InputTokens), FormatTokens(u.OutputTokens),
			FormatTokens(u.CacheRead+u.CacheWrite))
	}
	if u.CostUSD > 0 {
		s += fmt.Sprintf(" · $%.2f", u.CostUSD)
	}
	return s
}

// priced refaz o custo pela tabela de preços. byModel é o gasto separado por
// modelo, quando o adapter o tem: uma rodada do Claude Code mistura modelos —
// o principal, as lentes que rodaram como sub-agente, os auxiliares — e cada
// um é cobrado pelo seu preço. Um modelo que a tabela não conhece fica com o
// custo que o próprio provedor reportou para ele. Sem nenhum modelo conhecido
// o custo do provedor fica como veio — zerá-lo apagaria o único número que
// existe.
func priced(u Usage, byModel []Usage, table *pricing.Table) Usage {
	if table == nil {
		return u
	}
	parts := byModel
	if len(parts) == 0 {
		parts = []Usage{u}
	}
	var (
		total float64
		known bool
	)
	for _, p := range parts {
		if c, ok := table.CostOf(p.Model, p.InputTokens, p.OutputTokens, p.CacheWrite, p.CacheRead); ok {
			total += c
			known = true
		} else {
			total += p.CostUSD
		}
	}
	if known {
		u.CostUSD = math.Round(total*100) / 100
	}
	return u
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
