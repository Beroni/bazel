package agent

import "time"

// Limits é o quanto da cota do Claude já foi consumido — as mesmas janelas que
// o `/usage` do Claude Code mostra: a de cinco horas (a "sessão") e a de sete
// dias (a "semana").
//
// O dado não se consulta: ele vem de graça no stream de quem está rodando, no
// evento `rate_limit_event`. Fora de um review não há de onde tirá-lo, então o
// que a página mostra é sempre a última leitura — daí o At.
type Limits struct {
	Session Window `json:"session"`
	Week    Window `json:"week"`
	// Status é o que o Claude diz da cota: "allowed" enquanto dá para rodar.
	Status string `json:"status,omitempty"`
	// At é quando esta leitura chegou.
	At time.Time `json:"at"`
}

// Window é uma janela de cota: quanto já foi usado e quando ela zera.
type Window struct {
	// Utilization vai de 0 a 1.
	Utilization float64   `json:"utilization"`
	ResetsAt    time.Time `json:"resets_at,omitempty"`
}

// Empty diz que ainda não houve leitura nenhuma.
func (l Limits) Empty() bool { return l.At.IsZero() }

// Percent é a utilização em porcentagem inteira, para a tela.
func (w Window) Percent() int { return int(w.Utilization*100 + 0.5) }
