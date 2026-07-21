package ui

import (
	"bufio"
	"fmt"
	"image"
	"io"
	"os"
	"time"

	"golang.org/x/term"
)

// Sixel playback: true-pixel GIF rendering for terminals that support the Sixel
// graphics protocol (opt-in via KCLI_SPLASH_SIXEL). Unlike the cell-based corner
// animation it draws actual pixels, so it needs the whole screen (Suspend hands
// the terminal over, like exec) rather than living inside the tview layout.

// showSixel plays the loaded GIF full-screen as Sixel graphics until a key is
// pressed, then restores the TUI. No-op without a loaded GIF.
func (a *App) showSixel() {
	if a.splash == nil {
		return
	}
	a.tv.Suspend(func() { playSixel(a.splash) })
}

// playSixel loops the frames as Sixel on stdout at the GIF's own timing. stdin
// is put in raw mode so a single keypress exits without waiting for Enter.
func playSixel(anim *splashAnim) {
	out := os.Stdout
	fd := int(os.Stdin.Fd())

	var old *term.State
	if term.IsTerminal(fd) {
		old, _ = term.MakeRaw(fd)
	}
	fmt.Fprint(out, "\x1b[?25l\x1b[2J\x1b[H") // hide cursor, clear, home
	defer func() {
		fmt.Fprint(out, "\x1b[?25h\x1b[2J\x1b[H") // show cursor, clear
		if old != nil {
			term.Restore(fd, old)
		}
	}()

	stop := make(chan struct{})
	go func() {
		var b [1]byte
		os.Stdin.Read(b[:]) // any key
		close(stop)
	}()

	for i := 0; ; i = (i + 1) % len(anim.frames) {
		fmt.Fprint(out, "\x1b[H")
		encodeSixel(out, anim.frames[i])
		select {
		case <-stop:
			return
		case <-time.After(anim.delays[i]):
		}
	}
}

// sixel colours are quantised to a 6×6×6 (216) web-safe cube — no external
// quantiser needed and comfortably within Sixel's 256 registers.
func sixelLevel(v uint8) int { return (int(v) + 25) / 51 } // 0..255 -> 0..5

func sixelIndex(r, g, b uint8) int {
	return 36*sixelLevel(r) + 6*sixelLevel(g) + sixelLevel(b)
}

// encodeSixel writes img as a Sixel image: it maps every pixel to the 216-colour
// cube, declares the palette, then emits 6-row bands, one pass per colour that
// appears in the band (run-length compressed).
func encodeSixel(w io.Writer, img *image.RGBA) {
	b := img.Bounds()
	W, H := b.Dx(), b.Dy()
	if W == 0 || H == 0 {
		return
	}

	idx := make([]int, W*H)
	used := make([]bool, 216)
	for y := 0; y < H; y++ {
		for x := 0; x < W; x++ {
			c := img.RGBAAt(b.Min.X+x, b.Min.Y+y)
			i := sixelIndex(c.R, c.G, c.B)
			idx[y*W+x] = i
			used[i] = true
		}
	}

	bw := bufio.NewWriter(w)
	defer bw.Flush()

	fmt.Fprint(bw, "\x1bPq")             // enter Sixel mode
	fmt.Fprintf(bw, "\"1;1;%d;%d", W, H) // 1:1 pixel aspect, W×H raster
	for i := 0; i < 216; i++ {           // palette (RGB in 0..100 percent)
		if used[i] {
			fmt.Fprintf(bw, "#%d;2;%d;%d;%d", i, (i/36%6)*20, (i/6%6)*20, (i%6)*20)
		}
	}

	for y0 := 0; y0 < H; y0 += 6 {
		rows := 6
		if y0+rows > H {
			rows = H - y0
		}
		bandColors := make(map[int]bool)
		for r := 0; r < rows; r++ {
			for x := 0; x < W; x++ {
				bandColors[idx[(y0+r)*W+x]] = true
			}
		}

		first := true
		for ci := 0; ci < 216; ci++ {
			if !bandColors[ci] {
				continue
			}
			if !first {
				bw.WriteByte('$') // graphics CR: overlay next colour on the band
			}
			first = false
			fmt.Fprintf(bw, "#%d", ci)

			var prev byte
			var count int
			flush := func() {
				switch {
				case count == 0:
				case count > 3:
					fmt.Fprintf(bw, "!%d%c", count, prev)
				default:
					for k := 0; k < count; k++ {
						bw.WriteByte(prev)
					}
				}
			}
			for x := 0; x < W; x++ {
				var bits byte
				for r := 0; r < rows; r++ {
					if idx[(y0+r)*W+x] == ci {
						bits |= 1 << r
					}
				}
				sc := byte('?') + bits
				switch {
				case x == 0:
					prev, count = sc, 1
				case sc == prev:
					count++
				default:
					flush()
					prev, count = sc, 1
				}
			}
			flush()
		}
		bw.WriteByte('-') // graphics NL: next band
	}
	fmt.Fprint(bw, "\x1b\\") // ST: leave Sixel mode
}
