package splash

import (
	"strings"
	"testing"
)

// TestEggAlignment ensures the crack overlay lands inside the egg:
// all lines must have the same width and contain the fissure's column.
func TestEggAlignment(t *testing.T) {
	width := len([]rune(egg[0]))
	for i, line := range egg {
		if got := len([]rune(line)); got != width {
			t.Fatalf("egg[%d] tem %d colunas, esperado %d", i, got, width)
		}
	}
	for _, c := range cracks {
		if c.row < 0 || c.row >= len(egg) {
			t.Fatalf("fissura fora do ovo: linha %d", c.row)
		}
		if c.col < 0 || c.col >= width {
			t.Fatalf("fissura fora do ovo: coluna %d", c.col)
		}
		if ch := []rune(egg[c.row])[c.col]; ch != ' ' {
			t.Errorf("fissura em (%d,%d) cobre a casca %q, não o interior", c.row, c.col, ch)
		}
	}
}

// TestLogoAlignment ensures the logo fits in the shockwave canvas.
func TestLogoAlignment(t *testing.T) {
	width := len([]rune(logo[0]))
	for i, line := range logo {
		if got := len([]rune(line)); got != width {
			t.Fatalf("logo[%d] tem %d colunas, esperado %d", i, got, width)
		}
	}
	if width > canvasW {
		t.Fatalf("logo com %d colunas não cabe no canvas de %d", width, canvasW)
	}
}

// TestAllFramesRender runs the whole timeline looking for panics and
// empty frames.
func TestAllFramesRender(t *testing.T) {
	for f := range endFrame {
		out := model{frame: f}.View()
		if strings.TrimSpace(out) == "" {
			t.Errorf("frame %d saiu em branco", f)
		}
	}
}

// TestUpdateTerminates confirms the animation always reaches the end.
func TestUpdateTerminates(t *testing.T) {
	m := model{}
	for range endFrame * 2 {
		next, _ := m.Update(frameMsg{})
		m = next.(model)
		if m.done {
			if m.frame < logoStart {
				t.Fatalf("animação parou antes do logo, no frame %d", m.frame)
			}
			return
		}
	}
	t.Fatal("animação não terminou dentro do orçamento de frames")
}
