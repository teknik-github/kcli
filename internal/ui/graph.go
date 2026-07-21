package ui

import (
	"context"
	"fmt"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/guptarohit/asciigraph"
	"github.com/rivo/tview"

	"github.com/teknik-github/kcli/internal/k8s"
)

const (
	graphSamples  = 60              // history window length
	graphInterval = 2 * time.Second // sampling cadence
	graphHeight   = 8               // rows per line chart
)

// showGraph opens a live CPU/MEM sparkline panel for the selected pod or node.
// metrics-server only exposes instantaneous usage, so kcli samples on its own
// interval and keeps a rolling history to plot.
func (a *App) showGraph() {
	kind, ns, name, ok := a.selectedName()
	if !ok || (kind != "pod" && kind != "node") {
		return
	}

	view := tview.NewTextView().SetDynamicColors(true).SetWrap(false)
	view.SetBorder(true).SetTitle(fmt.Sprintf(" graph: %s %s/%s (every %s) ", kind, ns, name, graphInterval))
	view.SetText("sampling…")
	view.SetInputCapture(func(e *tcell.EventKey) *tcell.EventKey {
		if e.Key() == tcell.KeyEscape || e.Rune() == 'q' {
			a.stopGraph()
			a.closeModal("graph")
			return nil
		}
		return e
	})
	a.openModalFull("graph", view)

	stop := make(chan struct{})
	a.graphStop = stop
	cl := a.client // pin the cluster this sampler reads, in case the context switches
	go a.runGraph(view, cl, kind, ns, name, stop)
}

// stopGraph halts the running graph sampler, if any.
func (a *App) stopGraph() {
	if a.graphStop != nil {
		close(a.graphStop)
		a.graphStop = nil
	}
}

// runGraph is the sampling loop: it appends a fresh reading each tick and
// redraws the sparklines until stop is closed.
func (a *App) runGraph(view *tview.TextView, cl *k8s.Client, kind, ns, name string, stop <-chan struct{}) {
	var cpuHist, memHist []float64

	sample := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()

		var cpuVal, memVal float64
		var cpuLabel, memLabel string

		if kind == "pod" {
			cpuMilli, memBytes, err := cl.PodUsage(ctx, ns, name)
			if err != nil {
				a.tv.QueueUpdateDraw(func() { view.SetText(fmt.Sprintf("[red]metrics error: %v[-]", err)) })
				return
			}
			cpuVal, memVal = float64(cpuMilli), float64(memBytes)/(1024*1024)
			cpuLabel, memLabel = fmt.Sprintf("%dm", cpuMilli), fmt.Sprintf("%dMi", int64(memVal))
		} else {
			cpuMilli, memBytes, cpuCap, memCap, err := cl.NodeUsage(ctx, name)
			if err != nil {
				a.tv.QueueUpdateDraw(func() { view.SetText(fmt.Sprintf("[red]metrics error: %v[-]", err)) })
				return
			}
			cpuVal, memVal = float64(cpuMilli), float64(memBytes)/(1024*1024)
			cpuLabel = fmt.Sprintf("%dm%s", cpuMilli, pct(cpuMilli, cpuCap))
			memLabel = fmt.Sprintf("%dMi%s", int64(memVal), pct(memBytes, memCap))
		}

		cpuHist = appendSample(cpuHist, cpuVal)
		memHist = appendSample(memHist, memVal)

		a.tv.QueueUpdateDraw(func() {
			view.SetText(renderGraph(cpuHist, memHist, cpuLabel, memLabel))
		})
	}

	sample() // draw first reading immediately
	ticker := time.NewTicker(graphInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			sample()
		}
	}
}

// appendSample adds v and trims the history to the sample window.
func appendSample(hist []float64, v float64) []float64 {
	hist = append(hist, v)
	if len(hist) > graphSamples {
		hist = hist[len(hist)-graphSamples:]
	}
	return hist
}

// renderGraph lays out two line charts (CPU, MEM) with current and peak values.
func renderGraph(cpuHist, memHist []float64, cpuLabel, memLabel string) string {
	if len(cpuHist) < 2 {
		return "\n collecting samples… (need at least two readings for a line)"
	}
	return fmt.Sprintf(
		"\n [red::b]CPU[-::-]  now [green]%s[-]  peak [yellow]%s[-]\n%s\n\n"+
			" [blue::b]MEM (Mi)[-::-]  now [green]%s[-]  peak [yellow]%s[-]\n%s\n\n"+
			" [gray]samples: %d/%d   interval %s   [q/esc] close[-]",
		cpuLabel, maxLabel(cpuHist, "m"), lineChart(cpuHist, asciigraph.Red),
		memLabel, maxLabel(memHist, "Mi"), lineChart(memHist, asciigraph.Blue),
		len(cpuHist), graphSamples, graphInterval,
	)
}

// lineChart renders a series as a box-drawing line chart with a Y axis, the
// line drawn in the given color. asciigraph emits raw ANSI, so translate it to
// tview color tags for the dynamic-colors TextView to render.
func lineChart(series []float64, color asciigraph.AnsiColor) string {
	// Width defaults to len(series), so the chart grows as samples accrue —
	// a natural live-plot feel — and never resamples/distorts the data.
	plot := asciigraph.Plot(series,
		asciigraph.Height(graphHeight),
		asciigraph.Precision(0),
		asciigraph.SeriesColors(color),
	)
	return tview.TranslateANSI(plot)
}

// maxLabel formats the peak of a history window with a unit suffix.
func maxLabel(hist []float64, unit string) string {
	max := 0.0
	for _, v := range hist {
		if v > max {
			max = v
		}
	}
	return fmt.Sprintf("%d%s", int64(max), unit)
}

// pct returns a " (NN%)" suffix, or "" when capacity is unknown.
func pct(used, capacity int64) string {
	if capacity <= 0 {
		return ""
	}
	return fmt.Sprintf(" (%d%%)", used*100/capacity)
}
