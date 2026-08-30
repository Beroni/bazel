package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/beroni/bazel/internal/agent"
	"github.com/beroni/bazel/internal/gh"
)

// marksFile é o índice do que já foi revisado, ao lado dos reviews.
const marksFile = "revisados.json"

// maxMarks limita o índice: PR revisado há meses não muda mais nada na tela.
const maxMarks = 500

// marksMu serializa a leitura-modificação-escrita do índice. A interface web
// termina dois reviews ao mesmo tempo e os dois querem gravar.
var marksMu sync.Mutex

// Mark é o registro de um review já feito sobre um PR.
type Mark struct {
	Agent string    `json:"agent"`
	At    time.Time `json:"at"`
	// HeadOid é o commit que estava no topo do PR quando ele foi revisado —
	// é a comparação com o de agora que denuncia mudança depois do review.
	HeadOid string `json:"head_oid"`
	File    string `json:"file,omitempty"`
	Posted  bool   `json:"posted,omitempty"`
}

// Marks é o índice de reviews por chave de PR ("owner/repo#123").
type Marks map[string]Mark

// Status é como um PR aparece na lista depois de revisado.
type Status struct {
	Reviewed bool
	At       time.Time
	Agent    string
	Posted   bool
	// Changed diz que o PR ganhou commit novo depois do review — o que está
	// no GitHub não é mais o que foi revisado.
	Changed bool
}

// Status olha o índice para um PR.
func (m Marks) Status(pr gh.PR) Status {
	mark, ok := m[pr.Key()]
	if !ok {
		return Status{}
	}
	return Status{
		Reviewed: true,
		At:       mark.At,
		Agent:    mark.Agent,
		Posted:   mark.Posted,
		// Sem o commit registrado não dá para afirmar que mudou — melhor
		// não alarmar do que alarmar errado.
		Changed: mark.HeadOid != "" && pr.HeadRefOid != "" && mark.HeadOid != pr.HeadRefOid,
	}
}

// Age resume quando o review foi feito ("2h", "3d").
func (s Status) Age(now time.Time) string {
	d := now.Sub(s.At)
	switch {
	case d < time.Minute:
		return "agora"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d"
	}
}

// LoadMarks lê o índice. Arquivo inexistente ou corrompido é um índice vazio:
// isto é enfeite de lista, não pode derrubar a listagem de PRs.
func LoadMarks(dir string) Marks {
	marksMu.Lock()
	defer marksMu.Unlock()
	return loadMarksLocked(dir)
}

func loadMarksLocked(dir string) Marks {
	data, err := os.ReadFile(filepath.Join(dir, marksFile))
	if err != nil {
		return Marks{}
	}
	var m Marks
	if err := json.Unmarshal(data, &m); err != nil || m == nil {
		return Marks{}
	}
	return m
}

// MarkReviewed registra que um PR foi revisado, com o commit que estava no
// topo na hora. file é o markdown salvo, se houver.
func MarkReviewed(dir string, res agent.Result, file string) error {
	return update(dir, res.PR.Key(), func(m Mark) Mark {
		return Mark{
			Agent:   res.Agent,
			At:      time.Now(),
			HeadOid: res.PR.HeadRefOid,
			File:    file,
			// Agente que publica sozinho já deixou o review no PR.
			Posted: res.Posts || m.Posted,
		}
	})
}

// MarkPosted anota que o review daquele PR foi para o GitHub.
func MarkPosted(dir, key string) error {
	return update(dir, key, func(m Mark) Mark {
		m.Posted = true
		if m.At.IsZero() {
			m.At = time.Now()
		}
		return m
	})
}

func update(dir, key string, fn func(Mark) Mark) error {
	if key == "" {
		return nil
	}
	marksMu.Lock()
	defer marksMu.Unlock()

	marks := loadMarksLocked(dir)
	marks[key] = fn(marks[key])
	trim(marks)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(marks, "", "  ")
	if err != nil {
		return err
	}
	// Grava em arquivo temporário e renomeia: um review interrompido no meio
	// da escrita não pode deixar o índice pela metade.
	tmp := filepath.Join(dir, marksFile+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, marksFile))
}

// trim descarta os registros mais velhos quando o índice passa do teto.
func trim(m Marks) {
	if len(m) <= maxMarks {
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return m[keys[i]].At.Before(m[keys[j]].At) })
	for _, k := range keys[:len(m)-maxMarks] {
		delete(m, k)
	}
}
