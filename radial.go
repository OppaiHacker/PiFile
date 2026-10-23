package main

// Radial menu in the spirit of the old CS:GO buy wheel: a ring of categories,
// pick one and the ring turns into its formats. Direction picks the wedge, so
// a short flick of the mouse is enough. Runs as a Wayland layer-shell overlay.

import (
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/diamondburned/gotk4-layer-shell/pkg/gtk4layershell"
	"github.com/diamondburned/gotk4/pkg/cairo"
	"github.com/diamondburned/gotk4/pkg/core/glib"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"
	"github.com/diamondburned/gotk4/pkg/pangocairo"
)

type wedge struct {
	label, sub string
	it         *item   // leaf: run this
	kids       []wedge // category: open this ring
}

type preset struct {
	name          string
	quality, size int
}

var presets = []preset{{"original", 0, 0}, {"high · q90", 90, 0}, {"web · q80 · 1920px", 80, 1920}, {"small · q60 · 1280px", 60, 1280}}

const perPage = 8

var categories = []struct {
	name    string
	formats []string
}{
	{"image", append(slices.Clone(magickOut), "png", "jpg")},
	{"video", append(slices.Clone(videoExt), "gif")},
	{"audio", audioExt},
	{"document", []string{"pdf", "docx", "doc", "odt", "rtf", "txt", "html", "md", "epub", "mobi", "azw3", "fb2", "xlsx", "xls", "ods", "csv", "pptx", "ppt", "odp", "tex", "rst", "org", "srt", "ass", "vtt"}},
	{"archive", archOut},
}

// buildWheel groups menu items into categories; single-item categories become leaves.
func buildWheel(items []item) []wedge {
	groups := map[string][]wedge{}
	var order []string
	add := func(cat string, w wedge) {
		if _, ok := groups[cat]; !ok {
			order = append(order, cat)
		}
		groups[cat] = append(groups[cat], w)
	}
	for i := range items {
		it := &items[i]
		w := wedge{label: it.to, it: it}
		switch {
		case it.to == "extract" || it.to == "compress":
			w.label = it.label
			add("tools", w)
		case it.join && slices.Contains(archOut, it.to):
			add("pack", w)
		case it.join:
			add("join", w)
		default:
			cat := "other"
			for _, c := range categories {
				if slices.Contains(c.formats, it.to) {
					cat = c.name
					break
				}
			}
			// "gif" sits in both image and video; keep it with images for single files.
			add(cat, w)
		}
	}
	var ring []wedge
	for _, cat := range order {
		kids := groups[cat]
		if len(kids) == 1 && cat != "join" && cat != "pack" {
			k := kids[0]
			k.sub = cat
			ring = append(ring, k)
			continue
		}
		ring = append(ring, wedge{label: cat, sub: fmt.Sprintf("%d", len(kids)), kids: kids})
	}
	return ring
}

type radial struct {
	files  []string
	title  string
	stack  [][]wedge // ring history; last is shown
	page   int
	preset int
	cx, cy float64 // wheel centre
	px, py float64 // pointer
	hover  int     // -1 = centre
	glow   []float64
	opened time.Time
	shown  bool
	drop   bool // waiting for a drag to arrive
	hoverT time.Time
	choice *item
	area   *gtk.DrawingArea
	win    *gtk.ApplicationWindow
	motion bool
}

func (r *radial) ring() []wedge {
	all := r.stack[len(r.stack)-1]
	if len(all) <= perPage+1 {
		return all
	}
	start := r.page * perPage
	end := min(start+perPage, len(all))
	return all[start:end]
}

func (r *radial) pages() int {
	n := len(r.stack[len(r.stack)-1])
	if n <= perPage+1 {
		return 1
	}
	return (n + perPage - 1) / perPage
}

const (
	rOuter = 230.0
	rInner = 78.0
	dead   = 34.0
)

func (r *radial) hit(x, y float64) int {
	dx, dy := x-r.cx, y-r.cy
	if math.Hypot(dx, dy) < dead {
		return -1
	}
	n := len(r.ring())
	if n == 0 {
		return -1
	}
	span := 2 * math.Pi / float64(n)
	a := math.Atan2(dy, dx) + math.Pi/2 + span/2 // 0 = top, clockwise
	a = math.Mod(a+4*math.Pi, 2*math.Pi)
	return int(a / span)
}

func (r *radial) setRing(w []wedge) {
	r.stack = append(r.stack, w)
	r.page, r.hover, r.opened = 0, -1, time.Now()
	r.glow = make([]float64, perPage+1)
}

func (r *radial) back() {
	if len(r.stack) > 1 {
		r.stack = r.stack[:len(r.stack)-1]
		r.page, r.hover, r.opened = 0, -1, time.Now()
		return
	}
	r.win.Close()
}

func (r *radial) activate(i int) {
	ring := r.ring()
	if i < 0 || i >= len(ring) {
		if i < 0 {
			r.back()
		}
		return
	}
	w := ring[i]
	if w.kids != nil {
		r.setRing(w.kids)
		return
	}
	r.choice = w.it
	r.win.Close()
}

var animate = true

func ease(t float64) float64 { t = min(max(t, 0), 1); return 1 - math.Pow(1-t, 3) }

func (r *radial) draw(cr *cairo.Context, w, h int) {
	if !r.shown {
		return
	}
	// Keep the whole wheel on screen near edges; picking is by direction, so an offset centre still works.
	m := rOuter + 16
	r.cx = min(max(r.cx, m), max(float64(w)-m, m))
	r.cy = min(max(r.cy, m), max(float64(h)-m, m))
	t := 1.0
	if animate {
		t = ease(float64(time.Since(r.opened)) / float64(160*time.Millisecond))
	}
	cr.Save()
	cr.Translate(r.cx, r.cy)
	s := 0.9 + 0.1*t
	cr.Scale(s, s)

	// Backing disc.
	cr.SetSourceRGBA(0.07, 0.075, 0.085, 0.86*t)
	cr.Arc(0, 0, rOuter+10, 0, 2*math.Pi)
	cr.Fill()

	ring := r.ring()
	n := len(ring)
	span := 2 * math.Pi / float64(n)
	gap := 3.0
	for i, wd := range ring {
		a0 := -math.Pi/2 - span/2 + float64(i)*span
		a1 := a0 + span
		g := r.glow[i]
		// Hovered wedge grows outward a little.
		ro := rOuter + 6*g
		cr.NewPath()
		cr.Arc(0, 0, ro, a0+gap/ro, a1-gap/ro)
		cr.ArcNegative(0, 0, rInner, a1-gap/rInner, a0+gap/rInner)
		cr.ClosePath()
		cr.SetSourceRGBA(1, 1, 1, (0.05+0.03*g)*t)
		cr.FillPreserve()
		cr.SetSourceRGBA(0.96, 0.69, 0.18, 0.92*g*t) // accent
		cr.Fill()

		mid := (a0 + a1) / 2
		lr := (rInner + rOuter) / 2
		lx, ly := lr*math.Cos(mid), lr*math.Sin(mid)
		dark := g > 0.5
		font := "Monospace Bold 15"
		if wd.kids != nil || strings.Contains(wd.label, " ") {
			font = "Sans Bold 13"
		}
		text(cr, lx, ly-9, wd.label, font, dark, t)
		sub := wd.sub
		if wd.kids != nil {
			sub += " ›"
		}
		text(cr, lx, ly+11, sub, "Sans 9", dark, 0.65*t)
		if i < 9 {
			nx, ny := (rOuter-16)*math.Cos(mid), (rOuter-16)*math.Sin(mid)
			text(cr, nx, ny, strconv.Itoa(i+1), "Monospace 8", dark, 0.5*t)
		}
	}

	// Centre: what, preset, and the back/cancel target.
	ch := 0.0
	if r.hover == -1 {
		ch = 1
	}
	cr.SetSourceRGBA(1, 1, 1, (0.06+0.06*ch)*t)
	cr.Arc(0, 0, rInner-8, 0, 2*math.Pi)
	cr.Fill()
	label := r.title
	if len(r.stack) > 1 {
		label = "‹ back"
	}
	text(cr, 0, -16, label, "Sans Bold 11", false, t)
	text(cr, 0, 4, presets[r.preset].name, "Monospace 9", false, 0.8*t)
	hint := "tab: quality"
	if r.pages() > 1 {
		hint = fmt.Sprintf("page %d/%d · scroll", r.page+1, r.pages())
	}
	text(cr, 0, 22, hint, "Sans 8", false, 0.5*t)
	cr.Restore()
}

func text(cr *cairo.Context, x, y float64, s, font string, dark bool, alpha float64) {
	l := pangocairo.CreateLayout(cr)
	l.SetFontDescription(pango.FontDescriptionFromString(font))
	l.SetText(s)
	w, h := l.PixelSize()
	if dark {
		cr.SetSourceRGBA(0.08, 0.06, 0.02, alpha)
	} else {
		cr.SetSourceRGBA(0.94, 0.94, 0.95, alpha)
	}
	cr.MoveTo(x-float64(w)/2, y-float64(h)/2)
	pangocairo.ShowLayout(cr, l)
}

// cursorOnMonitor returns the pointer position relative to the focused monitor (Hyprland).
func cursorOnMonitor() (float64, float64, bool) {
	out, err := exec.Command("hyprctl", "cursorpos").Output()
	if err != nil {
		return 0, 0, false
	}
	var x, y float64
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%f, %f", &x, &y); err != nil {
		return 0, 0, false
	}
	mon, _ := exec.Command("hyprctl", "monitors").Output()
	// "Monitor NAME (ID n):\n\t2560x1440@165.00000 at 0x0\n ... focused: yes"
	for _, block := range strings.Split(string(mon), "Monitor ") {
		if !strings.Contains(block, "focused: yes") {
			continue
		}
		if i := strings.Index(block, " at "); i >= 0 {
			pos, _, _ := strings.Cut(block[i+4:], "\n")
			sx, sy, _ := strings.Cut(strings.TrimSpace(pos), "x") // "0x480": %f would read it as hex
			mx, _ := strconv.ParseFloat(sx, 64)
			my, _ := strconv.ParseFloat(sy, 64)
			return x - mx, y - my, true
		}
	}
	return x, y, true
}

// runRadial shows the wheel and returns the chosen item and preset.
// dropMode: stay invisible until files are dragged onto the overlay.
func runRadial(files []string, dropMode bool) (*item, []string, preset) {
	r := &radial{files: files, drop: dropMode, hover: -1}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))) // GTK info chatter
	app := gtk.NewApplication("dev.pifile.radial", gio.ApplicationNonUnique)
	app.ConnectActivate(func() {
		if s := gtk.SettingsGetDefault(); s != nil {
			animate = s.ObjectProperty("gtk-enable-animations").(bool)
		}
		css := gtk.NewCSSProvider()
		css.LoadFromString("window, window.background { background: transparent; }")
		gtk.StyleContextAddProviderForDisplay(gdk.DisplayGetDefault(), css, gtk.STYLE_PROVIDER_PRIORITY_APPLICATION)

		r.win = gtk.NewApplicationWindow(app)
		win := &r.win.Window
		gtk4layershell.InitForWindow(win)
		gtk4layershell.SetLayer(win, gtk4layershell.LayerShellLayerOverlay)
		gtk4layershell.SetNamespace(win, "pifile")
		for _, e := range []gtk4layershell.Edge{gtk4layershell.LayerShellEdgeTop, gtk4layershell.LayerShellEdgeBottom, gtk4layershell.LayerShellEdgeLeft, gtk4layershell.LayerShellEdgeRight} {
			gtk4layershell.SetAnchor(win, e, true)
		}
		gtk4layershell.SetExclusiveZone(win, -1)
		if dropMode {
			gtk4layershell.SetKeyboardMode(win, gtk4layershell.LayerShellKeyboardModeNone)
		} else {
			gtk4layershell.SetKeyboardMode(win, gtk4layershell.LayerShellKeyboardModeExclusive)
		}

		r.area = gtk.NewDrawingArea()
		r.area.SetDrawFunc(func(_ *gtk.DrawingArea, cr *cairo.Context, w, h int) { r.draw(cr, w, h) })
		r.win.SetChild(r.area)

		start := func(fs []string, x, y float64) {
			r.files = fs
			items := menuItems(fs)
			r.title = fmt.Sprintf("%d files", len(fs))
			if len(fs) == 1 {
				r.title = ellipsize(baseName(fs[0]), 18)
			}
			r.cx, r.cy, r.px, r.py = x, y, x, y
			r.stack = nil
			r.setRing(buildWheel(items))
			r.shown = len(r.ring()) > 0
			if !r.shown {
				r.win.Close()
			}
		}
		if !dropMode {
			x, y, ok := cursorOnMonitor()
			if at := os.Getenv("PIFILE_AT"); at != "" { // for testing placement
				fmt.Sscanf(at, "%f,%f", &x, &y)
			}
			if !ok {
				x, y = 600, 400
			}
			start(files, x, y)
		}

		r.area.AddTickCallback(func(_ gtk.Widgetter, _ gdk.FrameClocker) bool {
			if r.shown {
				prev := r.hover
				r.hover = r.hit(r.px, r.py)
				if r.hover != prev {
					r.hoverT = time.Now()
				}
				for i := range r.glow {
					target := 0.0
					if i == r.hover {
						target = 1
					}
					if animate {
						r.glow[i] += (target - r.glow[i]) * 0.35
					} else {
						r.glow[i] = target
					}
				}
				// Dragging: hovering a category for a moment opens it (spring-loaded).
				if r.drop && r.hover >= 0 && r.hover < len(r.ring()) && r.ring()[r.hover].kids != nil && time.Since(r.hoverT) > 350*time.Millisecond {
					r.activate(r.hover)
				}
				r.area.QueueDraw()
			}
			return true
		})

		mo := gtk.NewEventControllerMotion()
		mo.ConnectEnter(func(x, y float64) {
			if dropMode && !r.shown {
				r.win.Close() // plain pointer, not a drag: get out of the way at once
			}
			// Enter coordinates are unreliable on layer surfaces with an offset monitor; wait for motion.
		})
		mo.ConnectMotion(func(x, y float64) {
			r.px, r.py = x, y
		})
		r.area.AddController(mo)

		click := gtk.NewGestureClick()
		click.SetButton(0)
		click.ConnectPressed(func(n int, x, y float64) {
			if !r.shown {
				return
			}
			r.px, r.py = x, y
			switch click.CurrentButton() {
			case 1, 2:
				r.activate(r.hit(x, y))
			default:
				r.back()
			}
		})
		r.area.AddController(click)

		sc := gtk.NewEventControllerScroll(gtk.EventControllerScrollVertical | gtk.EventControllerScrollDiscrete)
		sc.ConnectScroll(func(dx, dy float64) bool {
			step := 1
			if dy < 0 {
				step = -1
			}
			if r.pages() > 1 {
				r.page = (r.page + step + r.pages()) % r.pages()
			} else {
				r.preset = (r.preset + step + len(presets)) % len(presets)
			}
			return true
		})
		r.area.AddController(sc)

		key := gtk.NewEventControllerKey()
		key.ConnectKeyPressed(func(keyval, _ uint, _ gdk.ModifierType) bool {
			switch {
			case keyval == gdk.KEY_Escape || keyval == gdk.KEY_BackSpace:
				r.back()
			case keyval == gdk.KEY_Tab:
				r.preset = (r.preset + 1) % len(presets)
			case keyval == gdk.KEY_Return || keyval == gdk.KEY_KP_Enter || keyval == gdk.KEY_space:
				r.activate(r.hover)
			case keyval >= gdk.KEY_1 && keyval <= gdk.KEY_9:
				r.activate(int(keyval - gdk.KEY_1))
			case keyval == gdk.KEY_Page_Down || keyval == gdk.KEY_Right:
				r.page = (r.page + 1) % r.pages()
			case keyval == gdk.KEY_Page_Up || keyval == gdk.KEY_Left:
				r.page = (r.page - 1 + r.pages()) % r.pages()
			}
			return true
		})
		r.win.AddController(key)

		if dropMode {
			dt := gtk.NewDropTarget(gdk.GTypeFileList, gdk.ActionCopy)
			dt.SetPreload(true)
			dt.ConnectEnter(func(x, y float64) gdk.DragAction {
				r.px, r.py = x, y
				return gdk.ActionCopy
			})
			dt.ConnectMotion(func(x, y float64) gdk.DragAction {
				r.px, r.py = x, y
				if !r.shown {
					if fs := fileList(dt.Value()); len(fs) > 0 {
						start(fs, x, y)
					}
				}
				return gdk.ActionCopy
			})
			dt.ConnectDrop(func(v *glib.Value, x, y float64) bool {
				r.drop = false
				if !r.shown {
					if fs := fileList(v); len(fs) > 0 {
						start(fs, x, y)
					}
					return true
				}
				r.px, r.py = x, y
				r.activate(r.hit(x, y)) // a category opens and waits for a click
				return true
			})
			r.area.AddController(dt)
			// Nothing dragged in within a short moment: this Ctrl press was not a drag.
			glib.TimeoutAdd(900, func() bool {
				if !r.shown {
					r.win.Close()
				}
				return false
			})
		}
		r.win.Present()
	})
	app.Run([]string{os.Args[0]})
	return r.choice, r.files, presets[r.preset]
}

func fileList(v *glib.Value) []string {
	if v == nil {
		return nil
	}
	fl, ok := v.GoValue().(*gdk.FileList)
	if !ok || fl == nil {
		return nil
	}
	var out []string
	for _, f := range fl.Files() {
		if p := f.Path(); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func ellipsize(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
