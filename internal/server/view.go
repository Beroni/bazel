package server

import (
	"time"

	"github.com/beroni/bazel/internal/gh"
	"github.com/beroni/bazel/internal/store"
)

// prView é o PR como o navegador precisa dele.
//
// gh.PR marca Repo como `json:"-"` — é preenchido a partir do repositório
// consultado, não vem do gh — então serializar o struct direto perderia
// justamente o campo que identifica o PR. Daí esta view.
type prView struct {
	Key          string    `json:"key"`
	Repo         string    `json:"repo"`
	Slug         string    `json:"slug"`
	Number       int       `json:"number"`
	Title        string    `json:"title"`
	URL          string    `json:"url"`
	Author       string    `json:"author"`
	Draft        bool      `json:"draft"`
	Additions    int       `json:"additions"`
	Deletions    int       `json:"deletions"`
	ChangedFiles int       `json:"changed_files"`
	UpdatedAt    time.Time `json:"updated_at"`
	Age          string    `json:"age"`
	Branch       string    `json:"branch"`
	Base         string    `json:"base"`
	Decision     string    `json:"review_decision"`
	Mine         bool      `json:"mine"`
	// BodyHTML é a descrição do PR já convertida e sanitizada. Só vai
	// preenchida na listagem — nos eventos de job seria render à toa a cada
	// mudança de estado.
	BodyHTML string `json:"body_html,omitempty"`

	// Reviewed e companhia são o histórico: este PR já passou por um review?
	// Ele mudou desde então? O review foi para o GitHub?
	Reviewed     bool   `json:"reviewed,omitempty"`
	ReviewedAt   string `json:"reviewed_at,omitempty"`
	ReviewAgent  string `json:"review_agent,omitempty"`
	ReviewPosted bool   `json:"review_posted,omitempty"`
	Changed      bool   `json:"changed_since_review,omitempty"`
}

// withStatus carimba no PR o que o índice de reviews sabe sobre ele.
func (v prView) withStatus(st store.Status, now time.Time) prView {
	if !st.Reviewed {
		return v
	}
	v.Reviewed = true
	v.ReviewedAt = st.Age(now)
	v.ReviewAgent = st.Agent
	v.ReviewPosted = st.Posted
	v.Changed = st.Changed
	return v
}

func newPRView(pr gh.PR, mine bool, now time.Time) prView {
	return prView{
		Key:          pr.Key(),
		Repo:         pr.Repo,
		Slug:         pr.Slug(),
		Number:       pr.Number,
		Title:        pr.Title,
		URL:          pr.URL,
		Author:       pr.Author.Login,
		Draft:        pr.IsDraft,
		Additions:    pr.Additions,
		Deletions:    pr.Deletions,
		ChangedFiles: pr.ChangedFiles,
		UpdatedAt:    pr.UpdatedAt,
		Age:          pr.Age(now),
		Branch:       pr.HeadRefName,
		Base:         pr.BaseRefName,
		Decision:     pr.ReviewDecision,
		Mine:         mine,
	}
}

// stepView é um passo do review serializado. started_at vai junto para a
// página conseguir contar o tempo do passo que está rodando sem depender de um
// evento novo a cada segundo.
type stepView struct {
	Name      string     `json:"name"`
	State     State      `json:"state"`
	Seconds   int        `json:"seconds"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	Err       string     `json:"error,omitempty"`
}

// jobView é um job serializável. O corpo do review só vai junto quando o
// cliente pede um job específico — na lista ele só ocuparia banda.
type jobView struct {
	ID       string `json:"id"`
	PR       prView `json:"pr"`
	Agent    string `json:"agent,omitempty"`
	Pipeline bool   `json:"pipeline,omitempty"`
	Posts    bool   `json:"posts,omitempty"`
	// Publishing marca o job que está levando um review já lido ao PR, e
	// PublishOf aponta para o review de onde ele saiu.
	Publishing bool       `json:"publishing,omitempty"`
	PublishOf  string     `json:"publish_of,omitempty"`
	LogLines   int        `json:"log_lines"`
	Steps      []stepView `json:"steps,omitempty"`
	Cloning    bool       `json:"cloning,omitempty"`
	State      State      `json:"state"`
	QueuedAt   time.Time  `json:"queued_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Seconds    int        `json:"seconds"`
	Err        string     `json:"error,omitempty"`
	SavedTo    string     `json:"saved_to,omitempty"`
	Workdir    string     `json:"workdir,omitempty"`
	Truncated  bool       `json:"truncated,omitempty"`
	// Tokens e Cost são o gasto do review, mostrado quando ele termina. Um
	// agente que não fala stream-json não reporta gasto: fica zerado, e a
	// página não mostra linha nenhuma.
	Tokens  int     `json:"tokens,omitempty"`
	Cost    float64 `json:"cost_usd,omitempty"`
	Posted  bool    `json:"posted"`
	PostErr string  `json:"post_error,omitempty"`
	HasBody bool    `json:"has_body"`
	Body    string  `json:"body,omitempty"`
	HTML    string  `json:"html,omitempty"`
}

// view serializa o job. Chamar com o lock do Manager seguro.
func (j *Job) view(withBody bool) jobView {
	v := jobView{
		ID:        j.ID,
		PR:        newPRView(j.PR, j.Mine, time.Now()),
		Agent:     j.Choice.Name,
		Pipeline:  j.Choice.Pipeline,
		Posts:     j.Choice.Posts,
		LogLines:  j.logSeq,
		Steps:     j.stepViews(),
		Cloning:   j.Cloning,
		State:     j.State,
		QueuedAt:  j.QueuedAt,
		Seconds:   int(j.duration().Seconds()),
		Err:       j.Err,
		SavedTo:   j.SavedTo,
		Workdir:   j.Result.Workdir,
		Truncated: j.Result.Truncated,
		Tokens:    j.Result.Usage.Total(),
		Cost:      j.Result.Usage.CostUSD,
		Posted:    j.Posted,
		PostErr:   j.PostErr,
		HasBody:   j.Result.Body != "",
	}
	if j.publish != nil {
		v.Publishing = true
		v.PublishOf = j.publish.From
	}
	if !j.StartedAt.IsZero() {
		t := j.StartedAt
		v.StartedAt = &t
	}
	if !j.FinishedAt.IsZero() {
		t := j.FinishedAt
		v.FinishedAt = &t
	}
	if withBody && j.Result.Body != "" {
		v.Body = j.Result.Body
		v.HTML = renderMarkdown(j.Result.Body)
	}
	return v
}

// stepViews serializa os passos. Chamar com o lock do Manager seguro.
func (j *Job) stepViews() []stepView {
	if len(j.Steps) == 0 {
		return nil
	}
	out := make([]stepView, 0, len(j.Steps))
	for _, st := range j.Steps {
		v := stepView{Name: st.Name, State: st.State, Err: st.Err}
		switch {
		case st.State == StateRunning && !st.StartedAt.IsZero():
			t := st.StartedAt
			v.StartedAt = &t
			v.Seconds = int(time.Since(st.StartedAt).Seconds())
		default:
			v.Seconds = int(st.Duration.Seconds())
		}
		out = append(out, v)
	}
	return out
}
