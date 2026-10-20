//go:build windows

package tray

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"testing"
)

// TestIconRender writes the tray icon at 8x zoom for each mode (manual check).
func TestIconRender(t *testing.T) {
	out := os.Getenv("BATON_ICON_OUT")
	if out == "" {
		t.Skip("set BATON_ICON_OUT to render")
	}
	modes := []string{"normal", "stream", "game"}
	const s, z = 32, 6
	img := image.NewRGBA(image.Rect(0, 0, len(modes)*(s*z+20), s*z))
	for mi, m := range modes {
		dib := drawDIB(s, m, []float64{0.88, 0.35})
		px := dib[40:]
		for y := 0; y < s; y++ {
			for x := 0; x < s; x++ {
				o := ((s-1-y)*s + x) * 4
				c := color.RGBA{px[o+2], px[o+1], px[o], px[o+3]}
				for dy := 0; dy < z; dy++ {
					for dx := 0; dx < z; dx++ {
						img.Set(mi*(s*z+20)+x*z+dx, y*z+dy, c)
					}
				}
			}
		}
	}
	f, _ := os.Create(out)
	defer f.Close()
	png.Encode(f, img)
	if len(icon("normal", []float64{0.5, 0.5})) < 1000 {
		t.Fatal("icon too small")
	}
}
