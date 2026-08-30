// Este arquivo traduz o JSONL do `claude -p --output-format stream-json` em
// linhas de log legíveis. É o que faz a interface web mostrar o agente
// trabalhando — sem isso, um `claude -p` fica mudo até o relatório sair.
package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// isStreamJSON diz se os args pedem o formato de eventos. Só nesse caso o
// stdout é JSONL: em qualquer outro é o relatório em si, e sai cru no log.
func isStreamJSON(args []string) bool {
	for i, a := range args {
		if v, ok := strings.CutPrefix(a, "--output-format="); ok {
			return strings.TrimSpace(v) == "stream-json"
		}
		if a == "--output-format" && i+1 < len(args) {
			return strings.TrimSpace(args[i+1]) == "stream-json"
		}
	}
	return false
}

// streamEvent é a parte de um evento do stream que interessa aqui. O formato
// tem muito mais campo do que isto; tudo que não está mapeado é ignorado de
// propósito, para uma versão nova do agente não quebrar o parser.
type streamEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	// Result e IsError vêm no evento final.
	Result     string  `json:"result"`
	IsError    bool    `json:"is_error"`
	DurationMS int     `json:"duration_ms"`
	CostUSD    float64 `json:"total_cost_usd"`
	// Usage é o gasto da conversa principal — só dela. Um agente que dispara
	// sub-agentes gasta muito mais do que isto diz.
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		CacheWrite   int `json:"cache_creation_input_tokens"`
		CacheRead    int `json:"cache_read_input_tokens"`
	} `json:"usage"`
	// ModelUsage é o gasto por modelo, e é ele que fecha a conta: entram as
	// lentes que rodaram como sub-agente e os modelos auxiliares que o próprio
	// Claude Code usa. Numa rodada com uma lente só, o `usage` acima contou
	// 52k tokens enquanto este somou 68k.
	ModelUsage map[string]struct {
		InputTokens  int     `json:"inputTokens"`
		OutputTokens int     `json:"outputTokens"`
		CacheRead    int     `json:"cacheReadInputTokens"`
		CacheWrite   int     `json:"cacheCreationInputTokens"`
		CostUSD      float64 `json:"costUSD"`
	} `json:"modelUsage"`
	// ParentToolUseID marca o que rodou dentro de um sub-agente.
	ParentToolUseID *string `json:"parent_tool_use_id"`
	Tools           []any   `json:"tools"`
	Message         struct {
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// spent é o gasto do evento final. O detalhe por modelo manda quando existe —
// é o único que enxerga os sub-agentes; sem ele sobra o `usage`, que é o que um
// agente mais simples reporta.
func (ev streamEvent) spent() Usage {
	u := Usage{CostUSD: ev.CostUSD}
	if len(ev.ModelUsage) == 0 {
		u.InputTokens = ev.Usage.InputTokens
		u.OutputTokens = ev.Usage.OutputTokens
		u.CacheWrite = ev.Usage.CacheWrite
		u.CacheRead = ev.Usage.CacheRead
		return u
	}
	var custo float64
	for _, m := range ev.ModelUsage {
		u.InputTokens += m.InputTokens
		u.OutputTokens += m.OutputTokens
		u.CacheWrite += m.CacheWrite
		u.CacheRead += m.CacheRead
		custo += m.CostUSD
	}
	// Detalhe por modelo presente mas sem token nenhum: o que vale é o usage.
	if u.Total() == 0 {
		u.InputTokens = ev.Usage.InputTokens
		u.OutputTokens = ev.Usage.OutputTokens
		u.CacheWrite = ev.Usage.CacheWrite
		u.CacheRead = ev.Usage.CacheRead
	}
	// O total_cost_usd é a palavra final sobre o custo; a soma por modelo só
	// entra quando ele não veio.
	if u.CostUSD == 0 {
		u.CostUSD = custo
	}
	return u
}

// contentBlock é um bloco de conteúdo de mensagem: texto, chamada de
// ferramenta ou resultado de ferramenta.
type contentBlock struct {
	Type string `json:"type"`
	// ID é o identificador de uma chamada de ferramenta — é por ele que os
	// eventos de dentro de um Task dizem de qual sub-agente vieram.
	ID      string          `json:"id"`
	Text    string          `json:"text"`
	Name    string          `json:"name"`
	Input   map[string]any  `json:"input"`
	IsError bool            `json:"is_error"`
	Content json.RawMessage `json:"content"`
}

// logEntry é uma linha de log já atribuída a quem a escreveu. Agent vazio é o
// próprio agente do passo; preenchido é um sub-agente que ele disparou.
type logEntry struct {
	Agent string
	Text  string
}

// streamParser vai lendo o JSONL e guardando o texto final do agente.
type streamParser struct {
	result string
	// usage é o que o agente gastou, somado do evento final. Um agente que
	// não fala stream-json deixa isto zerado — não há de onde tirar.
	usage Usage
	// texts é o texto que o assistente escreveu, usado como relatório se o
	// evento final não vier (agente morto no meio, formato diferente).
	texts []string
	// subagents liga o id de uma chamada de Task ao nome do sub-agente que
	// ela subiu. É o que permite dizer qual das lentes da frota escreveu
	// cada linha: elas rodam em paralelo dentro do mesmo processo e sem isso
	// as linhas chegam embaralhadas e anônimas.
	subagents map[string]string
}

// line traduz uma linha do stream em zero ou mais linhas de log. Linha que não
// é JSON válido passa direto: é o único jeito de não engolir a saída de um
// agente que não fala esse formato.
func (p *streamParser) line(raw string) []logEntry {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	var ev streamEvent
	if err := json.Unmarshal([]byte(trimmed), &ev); err != nil {
		return []logEntry{{Text: raw}}
	}

	// Quem escreveu: o agente do passo, ou o sub-agente dono da chamada de
	// Task de onde este evento saiu.
	who := ""
	if ev.ParentToolUseID != nil && *ev.ParentToolUseID != "" {
		who = p.subagentName(*ev.ParentToolUseID)
	}
	one := func(text string) []logEntry { return []logEntry{{Agent: who, Text: text}} }

	switch ev.Type {
	case "system":
		if ev.Subtype == "init" {
			return one(fmt.Sprintf("· sessão iniciada · %d ferramentas", len(ev.Tools)))
		}
		return nil

	case "result":
		p.result = ev.Result
		took := time.Duration(ev.DurationMS) * time.Millisecond
		// O gasto conta mesmo quando o agente termina mal: os tokens foram
		// queimados do mesmo jeito.
		p.usage.add(ev.spent())
		if ev.IsError || ev.Subtype != "success" {
			return one(fmt.Sprintf("✗ o agente terminou com erro (%s)", ev.Subtype))
		}
		line := fmt.Sprintf("✓ pronto em %s", took.Round(time.Second))
		if t := p.usage.Total(); t > 0 {
			line += " · " + FormatTokens(t) + " tokens"
		}
		if ev.CostUSD > 0 {
			line += fmt.Sprintf(" · $%.2f", ev.CostUSD)
		}
		return one(line)

	case "assistant", "user":
		var blocks []contentBlock
		if err := json.Unmarshal(ev.Message.Content, &blocks); err != nil {
			return nil // conteúdo em texto puro: nada de útil para mostrar
		}
		var out []logEntry
		for _, b := range blocks {
			out = append(out, p.block(who, ev.Type == "assistant", b)...)
		}
		return out
	}
	return nil
}

func (p *streamParser) block(who string, fromAssistant bool, b contentBlock) []logEntry {
	entry := func(text string) logEntry { return logEntry{Agent: who, Text: text} }

	switch b.Type {
	case "text":
		text := strings.TrimSpace(b.Text)
		if text == "" {
			return nil
		}
		if !fromAssistant {
			// Texto num evento de usuário é o enunciado entregue ao
			// sub-agente. Vale como primeira linha do terminal dele — "foi
			// isto que pediram" — mas inteiro é a skill toda numa caixinha.
			return []logEntry{entry(truncateRunes(firstLine(text), 200))}
		}
		// Só o texto do agente principal vira relatório: o do sub-agente já
		// chega consolidado no resultado da chamada.
		if who == "" {
			p.texts = append(p.texts, text)
		}
		var out []logEntry
		for _, l := range strings.Split(text, "\n") {
			out = append(out, entry(l))
		}
		return out

	case "tool_use":
		p.rememberSubagent(b)
		return []logEntry{entry("→ " + toolSummary(b.Name, b.Input))}

	case "tool_result":
		// O resultado inteiro é ruído — só o que deu errado merece linha.
		if !b.IsError {
			return nil
		}
		return []logEntry{entry("  ✗ " + firstLine(rawText(b.Content)))}
	}
	// thinking e o que mais vier: fora do log.
	return nil
}

// rememberSubagent guarda o nome do sub-agente que uma chamada subiu, para as
// linhas vindas de dentro dela saberem dizer de quem são.
//
// Quem decide não é o nome da ferramenta — ela se chama `Agent` numa versão e
// `Task` noutra — e sim ter um `subagent_type` na entrada: é o que distingue
// "subiu um agente" de "leu um arquivo".
func (p *streamParser) rememberSubagent(b contentBlock) {
	if b.ID == "" {
		return
	}
	name := str(b.Input["subagent_type"])
	if name == "" && !isAgentTool(b.Name) {
		return
	}
	if name == "" {
		name = str(b.Input["description"])
	}
	if name == "" {
		name = "sub-agente"
	}
	p.register(b.ID, truncateRunes(name, 40))
}

// register anota o nome, desempatando quando a mesma lente sobe duas vezes:
// dois `exploit-digger` em paralelo são dois terminais, não um.
func (p *streamParser) register(id, name string) {
	if p.subagents == nil {
		p.subagents = map[string]string{}
	}
	if _, já := p.subagents[id]; já {
		return
	}
	final := name
	for n := 2; p.nameTaken(final); n++ {
		final = fmt.Sprintf("%s %d", name, n)
	}
	p.subagents[id] = final
}

func (p *streamParser) nameTaken(name string) bool {
	for _, v := range p.subagents {
		if v == name {
			return true
		}
	}
	return false
}

// subagentName é de quem são as linhas que saem de uma chamada. Chamada que o
// parser não viu nascer — evento perdido, formato diferente — ainda ganha um
// nome próprio por id, para dois desconhecidos não virarem um só.
func (p *streamParser) subagentName(parentID string) string {
	if name, ok := p.subagents[parentID]; ok {
		return name
	}
	p.register(parentID, "sub-agente")
	return p.subagents[parentID]
}

// isAgentTool reconhece a ferramenta que sobe um sub-agente pelos nomes que
// ela já teve.
func isAgentTool(name string) bool {
	switch name {
	case "Agent", "Task":
		return true
	}
	return false
}

// toolSummary resume uma chamada de ferramenta em uma linha: o nome e o
// argumento que diz o que ela está fazendo.
func toolSummary(name string, input map[string]any) string {
	if name == "" {
		name = "?"
	}
	if isAgentTool(name) {
		who := str(input["subagent_type"])
		what := str(input["description"])
		switch {
		case who != "" && what != "":
			return fmt.Sprintf("Agent(%s): %s", who, what)
		case who != "":
			return "Agent(" + who + ")"
		}
		return "Agent: " + truncateRunes(what, 70)
	}
	for _, key := range []string{"file_path", "notebook_path", "path", "command", "pattern", "url", "query", "skill", "description", "prompt"} {
		if v := str(input[key]); v != "" {
			arg := truncateRunes(strings.ReplaceAll(v, "\n", " "), 90)
			if key == "pattern" {
				if in := str(input["path"]); in != "" {
					arg += " em " + in
				}
			}
			return name + " " + arg
		}
	}
	return name
}

func str(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// rawText tira o texto de um conteúdo que tanto pode ser string quanto lista
// de blocos.
func rawText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, " ")
	}
	return string(raw)
}

// report é o relatório do agente: o texto do evento final ou, se ele não
// veio, o que o assistente escreveu no caminho.
func (p *streamParser) report() string {
	if r := strings.TrimSpace(p.result); r != "" {
		return r
	}
	return strings.TrimSpace(strings.Join(p.texts, "\n\n"))
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
