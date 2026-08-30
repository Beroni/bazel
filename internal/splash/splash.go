// Package splash desenha a animação de abertura: um ovo-bomba do Bazelgeuse
// que racha, esquenta e detona revelando o logo.
package splash

import (
	"math"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-isatty"
)

const (
	frameDur = 45 * time.Millisecond

	// Marcos da linha do tempo, em frames.
	crackStart = 12 // o ovo começa a rachar
	glowStart  = 20 // o casco esquenta
	flashStart = 24 // detonação
	waveStart  = 26 // onda de choque
	logoStart  = 34 // logo aparece
	endFrame   = 44
)

// waveGrowth é o passo do raio por frame. O anel precisa continuar visível até
// logoStart: no último frame da onda o raio ainda tem que caber no canto do
// canvas, senão a tela sai em branco.
const waveGrowth = 1.3

var (
	shellDim  = lipgloss.NewStyle().Foreground(lipgloss.Color("#7c6a58"))
	shellWarm = lipgloss.NewStyle().Foreground(lipgloss.Color("#c9b8a3"))
	crackHot  = lipgloss.NewStyle().Foreground(lipgloss.Color("#ff6a00")).Bold(true)
	glowHot   = lipgloss.NewStyle().Foreground(lipgloss.Color("#ffb020")).Bold(true)
	flashHot  = lipgloss.NewStyle().Foreground(lipgloss.Color("#fff6d5")).Bold(true)
	subtitle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#8b8b8b")).Italic(true)
	hint      = lipgloss.NewStyle().Foreground(lipgloss.Color("#5c5c5c"))
)

// egg é o ovo intacto. Todas as linhas têm a mesma largura para o overlay
// de rachaduras cair sempre na coluna certa.
var egg = []string{
	`      .-'''-.      `,
	`    .'       '.    `,
	`   /           \   `,
	`  |             |  `,
	`  |             |  `,
	`  |             |  `,
	`   \           /   `,
	`    '.       .'    `,
	`      '-...-'      `,
}

// crack é uma fissura aplicada sobre o ovo, na ordem em que aparece.
type crack struct {
	row, col int
	ch       rune
}

var cracks = []crack{
	{1, 9, '╷'},
	{2, 9, '╱'}, {2, 10, '╲'},
	{3, 8, '╱'}, {3, 11, '╲'},
	{4, 7, '╲'}, {4, 9, '✦'}, {4, 12, '╱'},
	{5, 8, '╱'}, {5, 11, '╲'},
	{6, 9, '╳'}, {6, 6, '╱'}, {6, 13, '╲'},
	{7, 8, '╲'}, {7, 11, '╱'},
}

const (
	canvasW = 41
	canvasH = 11
)

var logo = []string{
	`██████   █████  ███████ ███████ ██     `,
	`██   ██ ██   ██      ██ ██      ██     `,
	`██████  ███████    ███  █████   ██     `,
	`██   ██ ██   ██  ███    ██      ██     `,
	`██████  ██   ██ ███████ ███████ ███████`,
}

var logoRamp = []string{"#7f1d1d", "#b91c1c", "#ea580c", "#f97316", "#fbbf24", "#ffd166"}

type frameMsg struct{}

type model struct {
	frame int
	done  bool
}

func (m model) Init() tea.Cmd { return tick() }

func tick() tea.Cmd {
	return tea.Tick(frameDur, func(time.Time) tea.Msg { return frameMsg{} })
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg.(type) {
	case tea.KeyMsg:
		// Qualquer tecla pula direto pro logo final.
		return model{frame: endFrame - 1, done: true}, tea.Quit
	case frameMsg:
		if m.done {
			return m, nil
		}
		m.frame++
		if m.frame >= endFrame-1 {
			m.frame = endFrame - 1
			m.done = true
			return m, tea.Quit
		}
		return m, tick()
	}
	return m, nil
}

func (m model) View() string {
	var body string
	switch {
	case m.frame < flashStart:
		body = renderEgg(m.frame)
	case m.frame < waveStart:
		body = renderFlash(m.frame)
	case m.frame < logoStart:
		body = renderWave(m.frame)
	default:
		body = renderLogo(m.frame)
	}
	return "\n" + body + "\n"
}

// renderEgg desenha o ovo com pulsação, rachaduras e tremor crescentes.
func renderEgg(frame int) string {
	// Quantas fissuras já apareceram.
	shown := 0
	if frame >= crackStart {
		progress := float64(frame-crackStart) / float64(flashStart-crackStart)
		shown = int(progress * float64(len(cracks)+1))
		if shown > len(cracks) {
			shown = len(cracks)
		}
	}
	active := make(map[[2]int]rune, shown)
	for _, c := range cracks[:shown] {
		active[[2]int{c.row, c.col}] = c.ch
	}

	// O casco vai de morno a incandescente conforme a detonação se aproxima.
	shell := shellDim
	if frame >= glowStart {
		shell = glowHot
	} else if frame%4 < 2 {
		shell = shellWarm
	}

	// Tremor: mais forte quanto mais perto do estouro.
	shake := 0
	if frame >= crackStart {
		amp := 1
		if frame >= glowStart {
			amp = 2
		}
		shake = ((frame % 2) * 2 * amp) - amp
	}

	lines := make([]string, 0, len(egg)+2)
	for r, row := range egg {
		var b strings.Builder
		for c, ch := range []rune(row) {
			if hot, ok := active[[2]int{r, c}]; ok {
				b.WriteString(crackHot.Render(string(hot)))
				continue
			}
			b.WriteString(shell.Render(string(ch)))
		}
		lines = append(lines, pad(shake)+b.String())
	}

	caption := hint.Render("incubando...")
	if frame >= crackStart {
		caption = crackHot.Render("⚠  instável")
	}
	if frame >= glowStart {
		caption = glowHot.Render("⚠  DETONAÇÃO IMINENTE")
	}
	lines = append(lines, "", caption)
	return frameBlock(lines)
}

// renderFlash é o clarão da explosão.
func renderFlash(frame int) string {
	density := []string{"▓", "█"}[frame%2]
	lines := make([]string, canvasH)
	for i := range lines {
		lines[i] = flashHot.Render(strings.Repeat(density, canvasW))
	}
	return frameBlock(lines)
}

// renderWave desenha o anel de detritos se expandindo a partir do centro.
func renderWave(frame int) string {
	step := frame - waveStart
	radius := 1.5 + float64(step)*waveGrowth

	cx, cy := float64(canvasW-1)/2, float64(canvasH-1)/2
	glyphs := []rune{'✦', '*', '·', '˙'}
	// Quanto mais longe, mais rarefeito o detrito.
	glyph := glyphs[min(step/3, len(glyphs)-1)]

	style := glowHot
	if step >= 4 {
		style = crackHot
	}
	if step >= 7 {
		style = shellDim
	}

	lines := make([]string, canvasH)
	for y := range canvasH {
		var b strings.Builder
		for x := range canvasW {
			// Correção de aspecto: célula de terminal é ~2x mais alta que larga.
			dx := (float64(x) - cx) * 0.5
			dy := float64(y) - cy
			d := math.Sqrt(dx*dx + dy*dy)
			if math.Abs(d-radius) < 0.7 && (x+y+step)%2 == 0 {
				b.WriteString(style.Render(string(glyph)))
			} else {
				b.WriteString(" ")
			}
		}
		lines[y] = b.String()
	}
	return frameBlock(lines)
}

// renderLogo revela o logo esquentando até a cor final.
func renderLogo(frame int) string {
	step := frame - logoStart
	color := logoRamp[min(step, len(logoRamp)-1)]
	style := lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Bold(true)

	lines := make([]string, 0, len(logo)+4)
	lines = append(lines, "")
	for _, row := range logo {
		lines = append(lines, style.Render(row))
	}
	lines = append(lines, "")

	return frameBlock(lines)
}

func frameBlock(lines []string) string {
	return lipgloss.NewStyle().
		Width(canvasW + 8).
		Align(lipgloss.Center).
		Render(strings.Join(lines, "\n"))
}

func pad(shake int) string {
	if shake <= 0 {
		return ""
	}
	return strings.Repeat(" ", shake)
}

// Play roda a animação. Vira no-op fora de um TTY, com BAZEL_NO_SPLASH
// definido ou com NO_COLOR — assim `bazel list | grep` continua limpo.
func Play() {
	if !Enabled() {
		return
	}
	p := tea.NewProgram(model{}, tea.WithOutput(os.Stderr))
	_, _ = p.Run()
}

// Enabled diz se a animação deve rodar no ambiente atual.
func Enabled() bool {
	if os.Getenv("BAZEL_NO_SPLASH") != "" || os.Getenv("NO_COLOR") != "" || os.Getenv("CI") != "" {
		return false
	}
	return isatty.IsTerminal(os.Stderr.Fd())
}
