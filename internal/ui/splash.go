package ui

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"math"
	"os"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

const (
	// splashMaxDim caps the pre-scaled source frame held in memory; Draw
	// area-averages from it down to the box. Kept well above the box so the
	// averaged result stays detailed.
	splashMaxDim = 320
	// default corner box size in cells; override via KCLI_SPLASH_SIZE="WxH".
	// Two sub-pixels per row make cols × (rows*2) ≈ square for a square GIF.
	defaultSplashCols = 40
	defaultSplashRows = 20
)

// splashSize returns the corner box size in cells, honouring KCLI_SPLASH_SIZE
// ("WxH", e.g. "60x30"), else the defaults. Bigger = more detail, more screen.
func splashSize() (cols, rows int) {
	cols, rows = defaultSplashCols, defaultSplashRows
	if s := os.Getenv("KCLI_SPLASH_SIZE"); s != "" {
		var w, h int
		if _, err := fmt.Sscanf(s, "%dx%d", &w, &h); err == nil &&
			w >= 8 && h >= 4 && w <= 200 && h <= 100 {
			cols, rows = w, h
		}
	}
	return cols, rows
}

// splashAnim is a decoded GIF: composited, opaque, pre-scaled RGBA frames with
// their inter-frame delays.
type splashAnim struct {
	frames []*image.RGBA
	delays []time.Duration
}

// loadGIF decodes a GIF into splashAnim. Frames are composited onto a running
// canvas (honouring the background-disposal method) so partial frames render
// correctly, then pre-scaled so playback and per-Draw scaling stay cheap.
func loadGIF(path string) (*splashAnim, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	g, err := gif.DecodeAll(f)
	if err != nil {
		return nil, err
	}

	w, h := g.Config.Width, g.Config.Height
	canvas := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.Black), image.Point{}, draw.Src)

	sc := scaleToFit(w, h, splashMaxDim, splashMaxDim)
	sw, sh := max1(int(float64(w)*sc)), max1(int(float64(h)*sc))

	anim := &splashAnim{}
	for i, frame := range g.Image {
		draw.Draw(canvas, frame.Bounds(), frame, frame.Bounds().Min, draw.Over)

		anim.frames = append(anim.frames, scaleAvg(canvas, sw, sh))
		d := time.Duration(g.Delay[i]) * 10 * time.Millisecond
		if d <= 0 {
			d = 100 * time.Millisecond
		}
		anim.delays = append(anim.delays, d)

		if i < len(g.Disposal) && g.Disposal[i] == gif.DisposalBackground {
			draw.Draw(canvas, frame.Bounds(), image.NewUniform(color.Black), image.Point{}, draw.Src)
		}
	}
	if len(anim.frames) == 0 {
		return nil, os.ErrInvalid
	}
	return anim, nil
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// scaleToFit returns the largest scale (<= 1) fitting w×h inside maxW×maxH.
func scaleToFit(w, h, maxW, maxH int) float64 {
	s := math.Min(float64(maxW)/float64(w), float64(maxH)/float64(h))
	if s > 1 {
		s = 1
	}
	return s
}

// scaleAvg box-averages src into a new w×h RGBA: each destination pixel is the
// mean of the source pixels it covers. This is a proper antialiased downscale —
// far clearer than nearest-neighbour at the low resolutions half-blocks use.
func scaleAvg(src *image.RGBA, w, h int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	for y := 0; y < h; y++ {
		sy0 := b.Min.Y + y*sh/h
		sy1 := b.Min.Y + (y+1)*sh/h
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for x := 0; x < w; x++ {
			sx0 := b.Min.X + x*sw/w
			sx1 := b.Min.X + (x+1)*sw/w
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			var r, g, bl, a, n uint32
			for sy := sy0; sy < sy1; sy++ {
				for sx := sx0; sx < sx1; sx++ {
					c := src.RGBAAt(sx, sy)
					r += uint32(c.R)
					g += uint32(c.G)
					bl += uint32(c.B)
					a += uint32(c.A)
					n++
				}
			}
			if n == 0 {
				n = 1
			}
			dst.SetRGBA(x, y, color.RGBA{uint8(r / n), uint8(g / n), uint8(bl / n), uint8(a / n)})
		}
	}
	return dst
}

// splashView renders one frame of a splashAnim with 2×2 Unicode quadrant blocks:
// each cell approximates a 2×2 pixel block via a best-fit glyph plus a
// foreground/background colour pair, so it packs four sub-pixels per cell (twice
// the horizontal detail of half-blocks). It is decorative (never focused).
type splashView struct {
	*tview.Box
	anim  *splashAnim
	frame int
}

func newSplashView(anim *splashAnim) *splashView {
	s := &splashView{Box: tview.NewBox(), anim: anim}
	s.Box.SetBackgroundColor(tcell.ColorBlack)
	s.Box.SetBorder(true).SetBorderColor(tcell.ColorGray) // a small framed "screen"
	return s
}

func (s *splashView) Draw(screen tcell.Screen) {
	s.Box.DrawForSubclass(screen, s.Box)
	x, y, w, h := s.Box.GetInnerRect()
	if w <= 0 || h <= 0 || len(s.anim.frames) == 0 {
		return
	}
	if s.frame >= len(s.anim.frames) {
		s.frame = len(s.anim.frames) - 1
	}

	src := s.anim.frames[s.frame]
	iw, ih := src.Bounds().Dx(), src.Bounds().Dy()

	// Render with 2×2 quadrant blocks: two sub-pixels per cell in EACH axis (vs
	// one column for half-blocks), doubling horizontal detail. Terminal cells
	// are ~twice as tall as wide, so a square image wants twice as many
	// horizontal sub-pixels as vertical — size the grid to match.
	maxPW, maxPH := 2*w, 2*h
	ph := maxPH
	pw := ph * 2 * iw / ih
	if pw > maxPW {
		pw = maxPW
		ph = pw * ih / (2 * iw)
	}
	pw &^= 1 // make even so it splits cleanly into 2-wide cells
	ph &^= 1
	if pw < 2 {
		pw = 2
	}
	if ph < 2 {
		ph = 2
	}
	img := scaleAvg(src, pw, ph)

	cw, ch := pw/2, ph/2
	offX := x + (w-cw)/2
	offY := y + (h-ch)/2
	for cy := 0; cy < ch; cy++ {
		for cx := 0; cx < cw; cx++ {
			glyph, fg, bg := quadCell(
				img.RGBAAt(2*cx, 2*cy), img.RGBAAt(2*cx+1, 2*cy),
				img.RGBAAt(2*cx, 2*cy+1), img.RGBAAt(2*cx+1, 2*cy+1))
			screen.SetContent(offX+cx, offY+cy, glyph, nil,
				tcell.StyleDefault.Foreground(fg).Background(bg))
		}
	}
}

// quadrantGlyphs maps a 4-bit foreground mask (bit0 TL, bit1 TR, bit2 BL,
// bit3 BR) to the 2×2 block-element glyph that fills those quadrants.
var quadrantGlyphs = [16]rune{
	' ', '▘', '▝', '▀', '▖', '▌', '▞', '▛',
	'▗', '▚', '▐', '▜', '▄', '▙', '▟', '█',
}

// quadCell approximates a 2×2 pixel block with one glyph and two colors: it
// splits the four pixels by luminance, averages each group into the foreground
// (filled quadrants) and background colors, and picks the matching glyph.
func quadCell(tl, tr, bl, br color.RGBA) (rune, tcell.Color, tcell.Color) {
	px := [4]color.RGBA{tl, tr, bl, br}
	var lum [4]int
	sum := 0
	for i, c := range px {
		lum[i] = (299*int(c.R) + 587*int(c.G) + 114*int(c.B)) / 1000
		sum += lum[i]
	}
	thr := sum / 4

	mask := 0
	var fR, fG, fB, fN, kR, kG, kB, kN int
	for i, c := range px {
		if lum[i] >= thr {
			mask |= 1 << i
			fR += int(c.R)
			fG += int(c.G)
			fB += int(c.B)
			fN++
		} else {
			kR += int(c.R)
			kG += int(c.G)
			kB += int(c.B)
			kN++
		}
	}
	if kN == 0 { // all four pixels one colour → a solid block
		return '█', rgb(px[0]), tcell.ColorBlack
	}
	fg := rgb(color.RGBA{uint8(fR / fN), uint8(fG / fN), uint8(fB / fN), 255})
	bg := rgb(color.RGBA{uint8(kR / kN), uint8(kG / kN), uint8(kB / kN), 255})
	return quadrantGlyphs[mask], fg, bg
}

func rgb(c color.RGBA) tcell.Color {
	return tcell.NewRGBColor(int32(c.R), int32(c.G), int32(c.B))
}

// startSplash overlays the animated GIF in the bottom-right corner of the main
// screen and loops it. The table keeps focus — the corner is decorative. No-op
// if already showing. Must run on the UI goroutine.
func (a *App) startSplash() {
	if a.splashing || a.splash == nil {
		return
	}
	a.splashing = true
	a.splashView = newSplashView(a.splash)
	a.splashStop = make(chan struct{})
	a.pages.AddPage("splash", a.splashCorner(a.splashView), true, true)
	a.tv.SetFocus(a.table) // keys still go to the table, not the corner
	go a.animateSplash(a.splashView, a.splashStop)
}

// stopSplash removes the corner animation. Must run on the UI goroutine.
func (a *App) stopSplash() {
	if !a.splashing {
		return
	}
	close(a.splashStop)
	a.pages.RemovePage("splash")
	a.tv.SetFocus(a.table)
	a.splashing = false
}

// toggleSplash shows/hides the corner animation (the `a` key).
func (a *App) toggleSplash() {
	if a.splashing {
		a.stopSplash()
	} else {
		a.startSplash()
	}
}

// splashCorner wraps view in transparent spacers so it sits in the bottom-right
// corner — a one-column margin off the right edge and one row above the footer.
// The nil spacers draw nothing, so the main view shows through around the box.
func (a *App) splashCorner(view *splashView) tview.Primitive {
	row := tview.NewFlex().SetDirection(tview.FlexColumn).
		AddItem(nil, 0, 1, false).          // left spacer
		AddItem(view, a.splashW, 0, false). // the box
		AddItem(nil, 1, 0, false)           // right margin
	return tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(nil, 0, 1, false).         // top spacer
		AddItem(row, a.splashH, 0, false). // the box row
		AddItem(nil, 1, 0, false)          // bottom margin (above the footer)
}

// animateSplash loops view's frames forever on the GIF's own timing until stop
// is closed. view is captured so a toggle-off mid-frame can't touch a new view.
func (a *App) animateSplash(view *splashView, stop chan struct{}) {
	i := 0
	for {
		frame := i
		a.tv.QueueUpdateDraw(func() { view.frame = frame })
		select {
		case <-stop:
			return
		case <-time.After(a.splash.delays[i]):
		}
		i = (i + 1) % len(a.splash.frames)
	}
}
