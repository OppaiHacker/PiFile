package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"html"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"unsafe"
)

const usage = `pifile: convert, join, pack and unpack files

  pifile convert [flags] FILE...   convert each file (-to FORMAT)
  pifile join    [flags] FILE...   join all files into one (-to pdf|gif|mp4|zip|mp3|psd|...)
  pifile pick    FILE...           radial menu with what these files can become (file manager entry)
  pifile drop                      radial menu that appears while dragging files (Ctrl during a drag)
  pifile targets FILE...           list what these files can become
  pifile watch   [flags] DIR       convert every new file in DIR (-to FORMAT [-move])
  pifile tools                     show which engines are installed
  pifile engines                   download LibreOffice, pandoc and calibre (no root) for full-quality documents
  pifile install [FM...]           add "Convert…" to file managers (default: every installed one;
                                   nautilus dolphin thunar nemo caja pcmanfm pcmanfm-qt)
  pifile uninstall                 remove pifile, its engines and every file manager entry

Special -to values: compress (PDF), extract (archives).

flags:
  -to FORMAT   target format or operation
  -q N         quality 1-100
  -size N      max width/height in px (video: height)
  -d SEC       seconds per still image in videos (default 2)
  -o DIR       output folder (default: next to the original)
  -j N         parallel jobs (default: CPU count)
  -move        watch: move originals to DIR/originals
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	fs.Usage = func() { fmt.Print(usage) }
	var o opts
	to := fs.String("to", "", "")
	move := fs.Bool("move", false, "")
	fs.IntVar(&o.quality, "q", 0, "")
	fs.IntVar(&o.size, "size", 0, "")
	fs.Float64Var(&o.duration, "d", 0, "")
	fs.StringVar(&o.outDir, "o", "", "")
	fs.IntVar(&jobs, "j", 0, "")
	// Allow flags after file names: pifile convert *.png -to webp
	var files []string
	for args := os.Args[2:]; ; {
		fs.Parse(args)
		if fs.NArg() == 0 {
			break
		}
		files = append(files, fs.Arg(0))
		args = fs.Args()[1:]
	}

	var err error
	switch os.Args[1] {
	case "convert", "join":
		if *to == "" || len(files) == 0 {
			fail(errors.New("need -to FORMAT and at least one file"))
		}
		err = report(do(ctx, files, *to, os.Args[1] == "join", o))
	case "pick":
		err = pick(ctx, files, o)
	case "drop":
		if it, fs, p := runRadial(nil, true); it != nil {
			o.quality, o.size = p.quality, p.size
			err = runChosen(ctx, fs, *it, o)
		}
	case "targets":
		for _, it := range menuItems(files) {
			fmt.Println(it.label)
		}
	case "watch":
		if *to == "" || len(files) != 1 {
			fail(errors.New("usage: pifile watch -to FORMAT [-move] DIR"))
		}
		err = watch(ctx, files[0], *to, *move, o)
	case "tools":
		for _, t := range []string{"vips", "magick", "ffmpeg", "ffprobe", "gs", "pdftotext", "tesseract", "bsdtar", "soffice", "pandoc", "ebook-convert", "rofi", "notify-send"} {
			state := "missing"
			if available(t) {
				state = "ok"
			}
			fmt.Printf("%-14s %s\n", t, state)
		}
	case "engines":
		err = installEngines(ctx)
	case "install":
		err = install(files)
	case "uninstall":
		err = uninstall()
	default:
		fmt.Print(usage)
		os.Exit(2)
	}
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "pifile:", err)
	os.Exit(1)
}

func isOp(to string) bool { return to == "compress" || to == "extract" }

// do runs one conversion per file, or a single join, and returns output paths and errors.
func do(ctx context.Context, files []string, to string, joinAll bool, o opts) ([]string, []error) {
	if joinAll {
		out, err := join(ctx, files, to, o)
		return []string{out}, []error{err}
	}
	return parallel(files, func(_ int, f string) (string, error) {
		if isOp(to) {
			return runOp(ctx, to, f, o)
		}
		return convert(ctx, f, to, o)
	})
}

func report(outs []string, errs []error) error {
	for i, err := range errs {
		if err == nil {
			fmt.Println(outs[i])
		}
	}
	return errors.Join(errs...)
}

func runOp(ctx context.Context, op, in string, o opts) (string, error) {
	dir, err := outDir(in, o)
	if err != nil {
		return "", err
	}
	switch {
	case op == "compress" && ext(in) == "pdf":
		out, err := reserve(dir, stem(in)+" (compressed)", "pdf")
		if err != nil {
			return "", err
		}
		err = sh(ctx, "gs", "-dSAFER", "-dBATCH", "-dNOPAUSE", "-dQUIET", "-sDEVICE=pdfwrite",
			"-dPDFSETTINGS=/ebook", "-sOutputFile="+out, in)
		if err != nil {
			os.Remove(out)
		}
		return out, err
	case op == "extract" && slices.Contains(archIn, ext(in)):
		out, err := reserveDir(dir, stem(in))
		if err != nil {
			return "", err
		}
		if err := sh(ctx, "bsdtar", "-xf", in, "-C", out); err != nil {
			os.RemoveAll(out)
			return "", err
		}
		return out, nil
	}
	return "", fmt.Errorf("%s: cannot %s a .%s file", filepath.Base(in), op, ext(in))
}

type item struct {
	label, to string
	join      bool
}

// Most used formats first in the menu, the rest alphabetically.
var popular = []string{"jpg", "png", "webp", "pdf", "mp4", "gif", "mp3", "avif", "heic", "zip", "txt", "wav", "flac", "mkv", "webm", "docx", "psd", "7z", "tar.gz"}

func rank(list []string) []string {
	sort.Slice(list, func(i, j int) bool {
		a, b := slices.Index(popular, list[i]), slices.Index(popular, list[j])
		if a < 0 {
			a = len(popular)
		}
		if b < 0 {
			b = len(popular)
		}
		if a != b {
			return a < b
		}
		return list[i] < list[j]
	})
	return list
}

func menuItems(files []string) []item {
	var items []item
	all := func(f func(string) bool) bool {
		for _, p := range files {
			if !f(p) {
				return false
			}
		}
		return true
	}
	if all(func(p string) bool { return slices.Contains(archIn, ext(p)) }) && available("bsdtar") {
		items = append(items, item{"extract here", "extract", false})
	}
	if all(func(p string) bool { return ext(p) == "pdf" }) && available("gs") {
		items = append(items, item{"compress pdf", "compress", false})
	}
	if !slices.ContainsFunc(files, isDir) {
		// Formats every selected file can reach.
		var common []string
		for i, p := range files {
			t := targets(formatOf(p))
			if i == 0 {
				common = t
			} else {
				common = slices.DeleteFunc(common, func(c string) bool { return !slices.Contains(t, c) })
			}
		}
		for _, t := range rank(common) {
			label := "to " + t
			if len(files) > 1 {
				label = fmt.Sprintf("each to %s", t)
			}
			items = append(items, item{label, t, false})
		}
	}
	var joins []item
	for _, t := range rank(joinTargets(files)) {
		label := "join into one " + t
		if slices.Contains(archOut, t) {
			label = "pack into " + t
		}
		if len(files) == 1 && !isDir(files[0]) {
			continue // single-file join is just a conversion, already listed
		}
		joins = append(joins, item{label, t, true})
	}
	if len(files) > 1 {
		return append(joins, items...) // joining is why people select many files
	}
	return append(items, joins...)
}

func pick(ctx context.Context, files []string, o opts) error {
	if len(files) == 0 {
		return errors.New("no files")
	}
	for i, f := range files {
		files[i], _ = filepath.Abs(f)
	}
	items := menuItems(files)
	if len(items) == 0 {
		notify("", "PiFile", "Nothing to convert these files to")
		return nil
	}
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		it, _, p := runRadial(files, false)
		if it == nil {
			return nil
		}
		o.quality, o.size = p.quality, p.size
		return runChosen(ctx, files, *it, o)
	}
	labels := make([]string, len(items))
	for i, it := range items {
		labels[i] = it.label
	}
	i := slices.Index(labels, menu(describe(files), labels))
	if i < 0 {
		return nil // cancelled
	}
	return runChosen(ctx, files, items[i], o)
}

func describe(files []string) string {
	if len(files) > 1 {
		return fmt.Sprintf("%d files", len(files))
	}
	return filepath.Base(files[0])
}

// runChosen converts with progress and result notifications.
func runChosen(ctx context.Context, files []string, it item, o opts) error {
	what := describe(files)
	id := notify("", "PiFile", fmt.Sprintf("%s: %s…", what, it.label))
	outs, errs := do(ctx, files, it.to, it.join, o)
	var ok []string
	var msgs []string
	for i, err := range errs {
		if err != nil {
			msgs = append(msgs, err.Error())
		} else {
			ok = append(ok, outs[i])
		}
	}
	body := fmt.Sprintf("%d done", len(ok))
	if len(ok) == 1 {
		body = filepath.Base(ok[0])
	}
	if len(msgs) > 0 {
		body += fmt.Sprintf(", %d failed\n%s", len(msgs), strings.Join(msgs, "\n"))
	}
	if len(ok) == 0 {
		notify(id, "PiFile: failed", body)
		return errors.Join(errs...)
	}
	// Offer to open the result; blocks until the notification closes.
	out, _ := exec.Command("notify-send", "-r", id, "-a", "PiFile", "-A", "open=Show", "-t", "8000", "--wait", "PiFile", body).Output()
	if strings.TrimSpace(string(out)) == "open" {
		target := ok[0]
		if len(ok) > 1 {
			target = filepath.Dir(ok[0])
		}
		exec.Command("xdg-open", target).Start()
	}
	return nil
}

// menu shows a picker with the first launcher found and returns the chosen line.
func menu(prompt string, items []string) string {
	in := strings.Join(items, "\n")
	var cmd *exec.Cmd
	switch {
	case available("rofi"):
		cmd = exec.Command("rofi", "-dmenu", "-i", "-no-custom", "-p", prompt)
	case available("wofi"):
		cmd = exec.Command("wofi", "--dmenu", "-i", "-p", prompt)
	case available("fuzzel"):
		cmd = exec.Command("fuzzel", "--dmenu", "-p", prompt+" ")
	case available("zenity"):
		cmd = exec.Command("zenity", append([]string{"--list", "--title=PiFile", "--text=" + prompt, "--column=Convert", "--hide-header"}, items...)...)
		in = ""
	default:
		return ""
	}
	cmd.Stdin = strings.NewReader(in)
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

// notify shows or replaces (id != "") a desktop notification and returns its id.
func notify(id, title, body string) string {
	args := []string{"-p", "-a", "PiFile"}
	if id != "" {
		args = append(args, "-r", id)
	}
	out, _ := exec.Command("notify-send", append(args, title, body)...).Output()
	return strings.TrimSpace(string(out))
}

func watch(ctx context.Context, dir, to string, move bool, o opts) error {
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC)
	if err != nil {
		return err
	}
	if _, err := syscall.InotifyAddWatch(fd, dir, syscall.IN_CLOSE_WRITE|syscall.IN_MOVED_TO); err != nil {
		return err
	}
	go func() { <-ctx.Done(); syscall.Close(fd) }()
	fmt.Printf("watching %s, converting new files to %s (Ctrl+C stops)\n", dir, to)
	buf := make([]byte, 64<<10)
	for {
		n, err := syscall.Read(fd, buf)
		if err != nil || n <= 0 {
			return ctx.Err()
		}
		for off := 0; off+syscall.SizeofInotifyEvent <= n; {
			ev := (*syscall.InotifyEvent)(unsafe.Pointer(&buf[off]))
			name := string(bytes.TrimRight(buf[off+syscall.SizeofInotifyEvent:off+syscall.SizeofInotifyEvent+int(ev.Len)], "\x00"))
			off += syscall.SizeofInotifyEvent + int(ev.Len)
			// Skip folders, hidden/partial files (our temp dirs included) and our own outputs.
			if ev.Mask&syscall.IN_ISDIR != 0 || name == "" || strings.HasPrefix(name, ".") || ext(name) == norm(to) {
				continue
			}
			if !isOp(to) && route(ext(name), norm(to)) == nil {
				continue
			}
			p := filepath.Join(dir, name)
			out, errs := do(ctx, []string{p}, to, false, o)
			if errs[0] != nil {
				fmt.Fprintln(os.Stderr, "error:", errs[0])
				continue
			}
			fmt.Println(name, "->", filepath.Base(out[0]))
			if move {
				od := filepath.Join(dir, "originals")
				if err := os.MkdirAll(od, 0o755); err == nil {
					os.Rename(p, filepath.Join(od, name))
				}
			}
		}
	}
}

// fileManagers maps a file manager to the binary that reveals it is installed.
var fileManagers = []struct{ name, bin string }{
	{"nautilus", "nautilus"}, {"dolphin", "dolphin"}, {"thunar", "thunar"}, {"nemo", "nemo"},
	{"caja", "caja"}, {"pcmanfm", "pcmanfm"}, {"pcmanfm-qt", "pcmanfm-qt"},
}

// install adds "Convert…" to the given file managers, or to every installed one.
func install(names []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	if len(names) == 0 {
		for _, fm := range fileManagers {
			if _, err := exec.LookPath(fm.bin); err == nil {
				names = append(names, fm.name)
			}
		}
		if len(names) == 0 {
			return errors.New("no supported file manager found (nautilus, dolphin, thunar, nemo, caja, pcmanfm, pcmanfm-qt)")
		}
	}
	home, _ := os.UserHomeDir()
	const name = "Convert…"
	const desc = "Convert, join, pack or extract with PiFile"
	q := "'" + strings.ReplaceAll(exe, "'", `'\''`) + "'"
	script := "#!/bin/sh\nexec " + q + " pick \"$@\"\n"
	for _, fm := range names {
		var err error
		switch fm {
		case "nautilus":
			if err = writeFile(filepath.Join(home, ".local/share/nautilus/scripts", name), script, 0o755); err == nil {
				err = appendLine(filepath.Join(home, ".config/nautilus/scripts-accels"), "<Control><Shift>c "+name)
			}
		case "dolphin":
			err = writeFile(filepath.Join(home, ".local/share/kio/servicemenus/pifile.desktop"), fmt.Sprintf(`[Desktop Entry]
Type=Service
MimeType=all/allfiles;inode/directory;
Actions=pifile
X-KDE-ServiceTypes=KonqPopupMenu/Plugin

[Desktop Action pifile]
Name=%s
Icon=document-export
Exec=%s pick %%F
`, name, q), 0o755)
		case "thunar":
			err = thunarAction(filepath.Join(home, ".config/Thunar/uca.xml"), name, desc, q+" pick %F")
		case "nemo":
			err = writeFile(filepath.Join(home, ".local/share/nemo/actions/pifile.nemo_action"), fmt.Sprintf(`[Nemo Action]
Name=%s
Comment=%s
Exec=%s pick %%F
Icon-Name=document-export
Selection=notnone
Extensions=any;
`, name, desc, q), 0o644)
		case "caja":
			err = writeFile(filepath.Join(home, ".config/caja/scripts", name), script, 0o755)
		case "pcmanfm", "pcmanfm-qt":
			err = writeFile(filepath.Join(home, ".local/share/file-manager/actions/pifile.desktop"), fmt.Sprintf(`[Desktop Entry]
Type=Action
Name=%s
Tooltip=%s
Icon=document-export
Profiles=pifile;

[X-Action-Profile pifile]
MimeTypes=all/all;
Exec=%s pick %%F
`, name, desc, q), 0o644)
		default:
			err = fmt.Errorf("unknown file manager %q", fm)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", fm, err)
		}
		fmt.Printf("%s: added %q (restart it to see it)\n", fm, name)
	}
	return nil
}

// uninstall removes everything install.sh and install put on the system.
func uninstall() error {
	home, _ := os.UserHomeDir()
	const name = "Convert…"
	paths := []string{
		filepath.Join(home, ".local/share/nautilus/scripts", name),
		filepath.Join(home, ".local/share/kio/servicemenus/pifile.desktop"),
		filepath.Join(home, ".local/share/nemo/actions/pifile.nemo_action"),
		filepath.Join(home, ".config/caja/scripts", name),
		filepath.Join(home, ".local/share/file-manager/actions/pifile.desktop"),
		filepath.Dir(engineDir()), // ~/.local/share/pifile
	}
	for _, p := range paths {
		if _, err := os.Lstat(p); err == nil {
			if err := os.RemoveAll(p); err != nil {
				return err
			}
			fmt.Println("removed", p)
		}
	}
	accels := filepath.Join(home, ".config/nautilus/scripts-accels")
	if b, err := os.ReadFile(accels); err == nil {
		var keep []string
		for _, l := range strings.SplitAfter(string(b), "\n") {
			if !strings.Contains(l, name) {
				keep = append(keep, l)
			}
		}
		os.WriteFile(accels, []byte(strings.Join(keep, "")), 0o644)
	}
	uca := filepath.Join(home, ".config/Thunar/uca.xml")
	if b, err := os.ReadFile(uca); err == nil {
		s := string(b)
		if i := strings.Index(s, "<unique-id>pifile-convert</unique-id>"); i >= 0 {
			start := strings.LastIndex(s[:i], "<action>")
			end := strings.Index(s[i:], "</action>\n")
			if start >= 0 && end >= 0 {
				os.WriteFile(uca, []byte(s[:start]+s[i+end+len("</action>\n"):]), 0o644)
				fmt.Println("removed Thunar action")
			}
		}
	}
	// The ImageMagick 6 shim install.sh writes on Debian/Ubuntu.
	shim := filepath.Join(home, ".local/bin/magick")
	if b, err := os.ReadFile(shim); err == nil && string(b) == "#!/bin/sh\nexec convert \"$@\"\n" {
		os.Remove(shim)
	}
	// The Nautilus extension lives in a system directory.
	dirs := []string{"/usr/lib/nautilus/extensions-4", "/usr/lib64/nautilus/extensions-4", "/usr/lib/x86_64-linux-gnu/nautilus/extensions-4"}
	if out, err := exec.Command("pkg-config", "--variable=extensiondir", "libnautilus-extension-4").Output(); err == nil {
		dirs = append([]string{strings.TrimSpace(string(out))}, dirs...)
	}
	for _, d := range dirs {
		so := filepath.Join(d, "libpifile-nautilus.so")
		if _, err := os.Stat(so); err == nil {
			fmt.Println("removing", so, "(needs sudo)")
			cmd := exec.Command("sudo", "rm", "-f", so)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := cmd.Run(); err != nil {
				fmt.Fprintln(os.Stderr, "could not remove it:", err)
			}
			break
		}
	}
	if exe, err := os.Executable(); err == nil {
		exe, _ = filepath.EvalSymlinks(exe)
		if err := os.Remove(exe); err == nil {
			fmt.Println("removed", exe)
		}
	}
	fmt.Println("PiFile is uninstalled. Restart your file manager (nautilus -q) to drop the menu entries.")
	return nil
}

func appendLine(p, line string) error {
	cur, _ := os.ReadFile(p)
	s := string(cur)
	if strings.Contains(s, line) {
		return nil
	}
	if s != "" && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return writeFile(p, s+line+"\n", 0o644)
}

// thunarAction adds a custom action to Thunar's uca.xml, keeping the user's own actions.
func thunarAction(p, name, desc, command string) error {
	cur, _ := os.ReadFile(p)
	s := string(cur)
	if strings.Contains(s, "<unique-id>pifile-convert</unique-id>") {
		return nil
	}
	action := fmt.Sprintf(`<action>
	<icon>document-export</icon>
	<name>%s</name>
	<unique-id>pifile-convert</unique-id>
	<command>%s</command>
	<description>%s</description>
	<patterns>*</patterns>
	<directories/><audio-files/><image-files/><other-files/><text-files/><video-files/>
</action>
`, html.EscapeString(name), html.EscapeString(command), html.EscapeString(desc))
	i := strings.LastIndex(s, "</actions>")
	if i < 0 {
		s, i = "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<actions>\n</actions>\n", len("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<actions>\n")
	}
	return writeFile(p, s[:i]+action+s[i:], 0o644)
}

func writeFile(p, s string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p, []byte(s), mode); err != nil {
		return err
	}
	return os.Chmod(p, mode)
}
