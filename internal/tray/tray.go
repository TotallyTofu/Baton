//go:build windows

// Package tray puts baton in the notification area: a live icon showing both
// cards' memory, and a menu for the common switches.
package tray

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"fyne.io/systray"

	"baton/internal/broker"
)

// Run blocks on the tray's message loop until quit is chosen.
func Run(b *broker.Broker, dashURL string, quit func()) {
	systray.Run(func() { onReady(b, dashURL, quit) }, func() {})
}

func Stop() { systray.Quit() }

func OpenURL(u string) {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = cmd.Start()
}

func onReady(b *broker.Broker, dashURL string, quit func()) {
	systray.SetIcon(icon("normal", nil))
	setTooltip("Baton")
	systray.SetOnTapped(func() { OpenURL(dashURL) })

	mStatus := systray.AddMenuItem("Baton", "")
	mStatus.Disable()
	mOpen := systray.AddMenuItem("Open dashboard", "")
	mGames := systray.AddMenuItem("Manage games…", "Folders and programs that count as games")
	systray.AddSeparator()

	v := b.View(1 << 62)
	type engItem struct {
		name string
		item *systray.MenuItem
	}
	var engs []engItem
	for _, e := range v.Engines {
		label := "Use " + e.Name
		if e.Kind == "comfyui" {
			label = "Give GPUs to " + e.Name
		}
		engs = append(engs, engItem{e.Name, systray.AddMenuItem(label, "")})
	}
	mFree := systray.AddMenuItem("Free all GPUs", "Release every engine")
	mHold := systray.AddMenuItemCheckbox("Hold GPUs (gaming)", "Keep every engine off the cards until unchecked", v.Mode == broker.Hold)
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit baton", "Engines keep running")

	go func() {
		for {
			select {
			case <-mOpen.ClickedCh:
				OpenURL(dashURL)
			case <-mGames.ClickedCh:
				OpenURL(dashURL + "#games")
			case <-mFree.ClickedCh:
				go b.FreeAll(context.Background())
			case <-mHold.ClickedCh:
				if mHold.Checked() {
					b.ClearHold()
				} else {
					b.SetHold("tray", 0)
				}
			case <-mQuit.ClickedCh:
				quit()
				return
			}
		}
	}()
	for _, e := range engs {
		go func(e engItem) {
			for range e.item.ClickedCh {
				go b.Activate(context.Background(), e.name, "", "requested from tray")
			}
		}(e)
	}

	go func() {
		last := ""
		for {
			v := b.View(1 << 62)
			var fr []float64
			for _, g := range v.GPUs {
				if g.MemTotal > 0 {
					fr = append(fr, float64(g.MemUsed)/float64(g.MemTotal))
				}
			}
			key := fmt.Sprintf("%s|%v", v.Mode, quant(fr))
			if key != last {
				systray.SetIcon(icon(string(v.Mode), fr))
				last = key
			}
			status := statusLine(v)
			mStatus.SetTitle(status)
			setTooltip(tooltip(v))
			if v.Mode == broker.Hold {
				mHold.Check()
			} else {
				mHold.Uncheck()
			}
			for _, e := range engs {
				held := v.Mode == broker.Game || v.Mode == broker.Hold
				if held || v.Op != nil {
					e.item.Disable()
				} else {
					e.item.Enable()
				}
			}
			time.Sleep(2 * time.Second)
		}
	}()
}

func statusLine(v broker.View) string {
	switch v.Mode {
	case broker.Game:
		return "Game: " + v.ModeWhy
	case broker.Hold:
		return "Hold: " + v.ModeWhy
	case broker.Stream:
		return "Streaming: " + v.ModeWhy
	}
	if v.Guest != nil {
		return "Guest: " + v.Guest.Why + " (the LLMs wait)"
	}
	if v.Op != nil {
		if v.Op.Stage != "" {
			return v.Op.Stage
		}
		return v.Op.Name
	}
	if v.Holder != "" {
		return v.Holder + " holds the GPUs"
	}
	return "GPUs free"
}

func tooltip(v broker.View) string {
	parts := []string{"Baton: " + statusLine(v)}
	for _, g := range v.GPUs {
		name := strings.TrimPrefix(g.Name, "NVIDIA GeForce ")
		parts = append(parts, fmt.Sprintf("%s %.1f/%.0f GiB %d%%", name, float64(g.MemUsed)/(1<<30), float64(g.MemTotal)/(1<<30), g.Util))
	}
	if m := v.Memory; m != nil && m.Total > 0 {
		parts = append(parts, fmt.Sprintf("RAM %.1f/%.0f GiB", float64(m.Used)/(1<<30), float64(m.Total)/(1<<30)))
	}
	t := strings.Join(parts, "\n")
	if len(t) > 127 { // NOTIFYICONDATA tooltip limit
		t = t[:127]
	}
	return t
}

func quant(fr []float64) []int {
	out := make([]int, len(fr))
	for i, f := range fr {
		out[i] = int(f*12 + 0.5)
	}
	return out
}

// --- icon drawing: a rounded tile in the mode's color with one bar per card ---

type rgba struct{ r, g, b, a uint8 }

var modeColor = map[string]rgba{
	"normal": {0x1f, 0x2a, 0x36, 255},
	"stream": {0xb8, 0x2d, 0x7a, 255},
	"game":   {0xd9, 0x6a, 0x12, 255},
	"hold":   {0xd9, 0x6a, 0x12, 255},
}

func icon(mode string, fr []float64) []byte {
	sizes := []int{16, 20, 24, 32, 48}
	imgs := make([][]byte, len(sizes))
	for i, s := range sizes {
		imgs[i] = drawDIB(s, mode, fr)
	}
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, [3]uint16{0, 1, uint16(len(sizes))})
	off := 6 + 16*len(sizes)
	for i, s := range sizes {
		w := uint8(s)
		binary.Write(&buf, binary.LittleEndian, struct {
			W, H, Colors, Reserved uint8
			Planes, BitCount       uint16
			Bytes, Offset          uint32
		}{w, w, 0, 0, 1, 32, uint32(len(imgs[i])), uint32(off)})
		off += len(imgs[i])
	}
	for _, img := range imgs {
		buf.Write(img)
	}
	return buf.Bytes()
}

func drawDIB(s int, mode string, fr []float64) []byte {
	bg, ok := modeColor[mode]
	if !ok {
		bg = modeColor["normal"]
	}
	px := make([]rgba, s*s)
	radius := float64(s) * 0.22
	inRounded := func(x, y float64, x0, y0, x1, y1, r float64) bool {
		cx := clamp(x, x0+r, x1-r)
		cy := clamp(y, y0+r, y1-r)
		dx, dy := x-cx, y-cy
		return dx*dx+dy*dy <= r*r
	}
	if len(fr) == 0 {
		fr = []float64{0, 0}
	}
	n := float64(len(fr))
	pad := float64(s) * 0.2
	gap := float64(s) * 0.1
	barW := (float64(s) - 2*pad - gap*(n-1)) / n
	top, bot := float64(s)*0.18, float64(s)*0.82
	fill := rgba{0x5e, 0xea, 0xd4, 255}
	if mode != "normal" {
		fill = rgba{255, 255, 255, 255}
	}
	empty := rgba{255, 255, 255, 60}
	for y := 0; y < s; y++ {
		for x := 0; x < s; x++ {
			fx, fy := float64(x)+0.5, float64(y)+0.5
			if !inRounded(fx, fy, 0, 0, float64(s), float64(s), radius) {
				continue
			}
			c := bg
			for i, f := range fr {
				x0 := pad + float64(i)*(barW+gap)
				if fx >= x0 && fx < x0+barW && fy >= top && fy < bot {
					level := bot - (bot-top)*clamp(f, 0, 1)
					if fy >= level {
						c = fill
					} else {
						c = blend(bg, empty)
					}
				}
			}
			px[y*s+x] = c
		}
	}
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, struct {
		Size                     uint32
		W, H                     int32
		Planes, Bits             uint16
		Compression, ImageSize   uint32
		XPPM, YPPM, Used, Import int32
	}{40, int32(s), int32(2 * s), 1, 32, 0, uint32(s * s * 4), 0, 0, 0, 0})
	for y := s - 1; y >= 0; y-- { // bottom-up
		for x := 0; x < s; x++ {
			p := px[y*s+x]
			buf.Write([]byte{p.b, p.g, p.r, p.a})
		}
	}
	maskRow := ((s + 31) / 32) * 4
	buf.Write(make([]byte, maskRow*s)) // AND mask unused with alpha
	return buf.Bytes()
}

func blend(a, over rgba) rgba {
	f := float64(over.a) / 255
	mix := func(x, y uint8) uint8 { return uint8(float64(x)*(1-f) + float64(y)*f) }
	return rgba{mix(a.r, over.r), mix(a.g, over.g), mix(a.b, over.b), 255}
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
