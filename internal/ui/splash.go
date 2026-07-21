package ui

import (
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"math"
	"os"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// splashMaxDim caps the pre-scaled frame size held in memory; the Draw step
// scales further down to the terminal. 240 px is ample for half-block output.
const splashMaxDim = 240

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

		anim.frames = append(anim.frames, scaleNearest(canvas, sw, sh))
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

// scaleNearest nearest-neighbour scales src into a new w×h RGBA (crisp for the
// pixel-art GIFs this is aimed at).
func scaleNearest(src *image.RGBA, w, h int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	b := src.Bounds()
	for y := 0; y < h; y++ {
		sy := b.Min.Y + y*b.Dy()/h
		for x := 0; x < w; x++ {
			sx := b.Min.X + x*b.Dx()/w
			dst.SetRGBA(x, y, src.RGBAAt(sx, sy))
		}
	}
	return dst
}

// splashView renders one frame of a splashAnim as colored half-blocks: each
// cell is '▀' with the top sub-pixel as foreground and the bottom as
// background, doubling vertical resolution. Any key invokes onDone (skip).
type splashView struct {
	*tview.Box
	anim   *splashAnim
	frame  int
	onDone func()
}

func newSplashView(anim *splashAnim, onDone func()) *splashView {
	s := &splashView{Box: tview.NewBox(), anim: anim, onDone: onDone}
	s.Box.SetBackgroundColor(tcell.ColorBlack)
	s.SetInputCapture(func(e *tcell.EventKey) *tcell.EventKey {
		s.onDone()
		return nil
	})
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

	img := s.anim.frames[s.frame]
	iw, ih := img.Bounds().Dx(), img.Bounds().Dy()

	// Fit the image into w columns × (h*2) sub-pixel rows, preserving aspect.
	sc := math.Min(float64(w)/float64(iw), float64(h*2)/float64(ih))
	dw := max1(int(float64(iw) * sc))
	dh := max1(int(float64(ih) * sc))
	cellRows := (dh + 1) / 2
	offX := x + (w-dw)/2
	offY := y + (h-cellRows)/2

	for cy := 0; cy < cellRows; cy++ {
		for cx := 0; cx < dw; cx++ {
			top := sampleFit(img, cx, 2*cy, dw, dh)
			style := tcell.StyleDefault.Foreground(rgb(top)).Background(tcell.ColorBlack)
			if 2*cy+1 < dh {
				style = style.Background(rgb(sampleFit(img, cx, 2*cy+1, dw, dh)))
			}
			screen.SetContent(offX+cx, offY+cy, '▀', nil, style)
		}
	}

	tview.Print(screen, "any key skips", x, y+h-1, w, tview.AlignCenter, tcell.ColorGray)
}

// sampleFit nearest-samples the img pixel for target coord (tx,ty) in a dw×dh grid.
func sampleFit(img *image.RGBA, tx, ty, dw, dh int) color.RGBA {
	b := img.Bounds()
	sx := b.Min.X + tx*b.Dx()/dw
	sy := b.Min.Y + ty*b.Dy()/dh
	return img.RGBAAt(sx, sy)
}

func rgb(c color.RGBA) tcell.Color {
	return tcell.NewRGBColor(int32(c.R), int32(c.G), int32(c.B))
}

// playSplash overlays the splash page and advances frames on their own timing
// until the GIF ends once or any key skips, then removes the page and returns
// focus to the table. Re-entrant calls (the replay key) are dropped while one
// is already on screen. All widget/state access happens inside QueueUpdateDraw.
func (a *App) playSplash() {
	done := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(done) }) }

	started := make(chan bool, 1)
	a.tv.QueueUpdateDraw(func() {
		if a.splashing {
			started <- false
			return
		}
		a.splashing = true
		a.splashView = newSplashView(a.splash, finish)
		a.pages.AddPage("splash", a.splashView, true, true)
		a.tv.SetFocus(a.splashView)
		started <- true
	})
	if !<-started {
		return // already playing
	}

loop:
	for i := range a.splash.frames {
		frame := i
		a.tv.QueueUpdateDraw(func() { a.splashView.frame = frame })
		select {
		case <-done: // skipped
			break loop
		case <-time.After(a.splash.delays[i]):
		}
	}

	a.tv.QueueUpdateDraw(func() {
		a.pages.RemovePage("splash")
		a.tv.SetFocus(a.table)
		a.splashing = false
	})
}
