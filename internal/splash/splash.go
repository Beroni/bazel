// Package splash draws the opening animation: a Bazelgeuse bomb-egg
// that cracks, heats up and detonates to reveal the logo.
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

	// Timeline milestones, in frames.
	crackStart = 12 // the egg starts to crack
	glowStart  = 20 // the shell heats up
	flashStart = 24 // detonation
	waveStart  = 26 // shockwave
	logoStart  = 34 // logo appears
	endFrame   = 44
)

// waveGrowth is the radius step per frame. The ring has to stay visible until
// logoStart: on the wave's last frame the radius still has to fit in the
// corner of the canvas, otherwise the screen comes out blank.
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

// egg is the intact egg. All lines have the same width so the crack
// overlay always lands in the right column.
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

// crack is a fissure applied over the egg, in the order it appears.
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
		// Any key skips straight to the final logo.
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

// renderEgg draws the egg with growing pulsing, cracks and shaking.
func renderEgg(frame int) string {
	// How many fissures have appeared so far.
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

	// The shell goes from warm to incandescent as detonation approaches.
	shell := shellDim
	if frame >= glowStart {
		shell = glowHot
	} else if frame%4 < 2 {
		shell = shellWarm
	}

	// Shaking: stronger the closer it is to the blast.
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

// renderFlash is the explosion's flash.
func renderFlash(frame int) string {
	density := []string{"▓", "█"}[frame%2]
	lines := make([]string, canvasH)
	for i := range lines {
		lines[i] = flashHot.Render(strings.Repeat(density, canvasW))
	}
	return frameBlock(lines)
}

// renderWave draws the debris ring expanding from the center.
func renderWave(frame int) string {
	step := frame - waveStart
	radius := 1.5 + float64(step)*waveGrowth

	cx, cy := float64(canvasW-1)/2, float64(canvasH-1)/2
	glyphs := []rune{'✦', '*', '·', '˙'}
	// The farther out, the sparser the debris.
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
			// Aspect correction: a terminal cell is ~2x taller than it is wide.
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

// renderLogo reveals the logo heating up to its final color.
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

// Play runs the animation. It is a no-op outside a TTY, with BAZEL_NO_SPLASH
// set or with NO_COLOR — so `bazel list | grep` stays clean.
func Play() {
	if !Enabled() {
		return
	}
	p := tea.NewProgram(model{}, tea.WithOutput(os.Stderr))
	_, _ = p.Run()
}

// Enabled says whether the animation should run in the current environment.
func Enabled() bool {
	if os.Getenv("BAZEL_NO_SPLASH") != "" || os.Getenv("NO_COLOR") != "" || os.Getenv("CI") != "" {
		return false
	}
	return isatty.IsTerminal(os.Stderr.Fd())
}
