package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
)

type opts struct {
	quality  int     // 1-100, 0 = tool default
	size     int     // max width/height (images) or height (video), 0 = keep
	duration float64 // seconds per still image when making video, 0 = 2
	single   bool    // never write a folder of pages, take the first page
	outDir   string  // "" = next to the original
}

type edge struct {
	tool     string // "" = built in, always available
	from, to []string
	cost     int  // 0 = 1; fallbacks get a high cost so real conversions win
	pages    bool // pdf -> image: as the last step renders every page into a folder
	run      func(ctx context.Context, in, out string, o opts) error
}

var (
	rasterIn  = []string{"jpg", "png", "webp", "avif", "heic", "tiff", "gif", "jxl", "jp2", "svg", "exr", "dng", "cr2", "cr3", "nef", "arw", "raf", "orf", "rw2"}
	rasterOut = []string{"jpg", "png", "webp", "avif", "heic", "tiff", "gif", "jxl", "jp2"}
	magickIn  = append(slices.Clone(rasterIn), "bmp", "ico", "psd", "tga", "ppm", "pcx", "xcf", "dds", "qoi")
	magickOut = append(slices.Clone(rasterOut), "bmp", "ico", "tga", "pdf", "psd", "qoi")
	videoExt  = []string{"mp4", "mkv", "webm", "mov", "avi", "m4v", "flv", "wmv", "ts", "mpg", "3gp"}
	audioExt  = []string{"mp3", "flac", "wav", "ogg", "opus", "m4a", "aac", "wma", "aiff"}
	subExt    = []string{"srt", "ass", "vtt"}
	textLike  = []string{"md", "csv", "json", "xml", "yaml", "toml", "html", "srt", "ass", "vtt", "log", "ini", "conf", "rst", "tex", "org", "sh", "py", "go", "js", "ts", "c", "h", "cpp", "rs", "java", "css", "sql"}
	textDocs  = []string{"doc", "docx", "odt", "rtf", "txt", "html"}
	sheets    = []string{"xls", "xlsx", "ods", "csv"}
	slidesExt = []string{"ppt", "pptx", "odp"}
	pandocIn  = []string{"md", "html", "docx", "odt", "epub", "rst", "tex", "org", "ipynb"}
	pandocOut = []string{"md", "html", "docx", "odt", "epub", "rst", "tex", "org", "txt"}
	ebooks    = []string{"epub", "mobi", "azw3", "fb2", "pdf", "docx", "txt"}
	archIn    = []string{"zip", "7z", "tar", "tar.gz", "tar.xz", "tar.zst", "tar.bz2", "rar", "iso", "cbz", "cbr"}
	archOut   = []string{"zip", "7z", "tar", "tar.gz", "tar.xz", "tar.zst", "tar.bz2", "cbz"}
)

// Order matters: on equal cost the earlier edge wins, so fast tools go first.
var edges = []edge{
	{tool: "vips", from: rasterIn, to: rasterOut, run: runVips},
	{tool: "magick", from: magickIn, to: magickOut, run: runMagick},
	{tool: "ffmpeg", from: videoExt, to: videoExt, run: runFFmpeg},
	{tool: "ffmpeg", from: videoExt, to: append(slices.Clone(audioExt), "gif", "webp", "png", "jpg"), run: runFFmpeg},
	{tool: "ffmpeg", from: audioExt, to: audioExt, run: runFFmpeg},
	{tool: "ffmpeg", from: audioExt, to: []string{"png", "jpg"}, run: runFFmpeg},                 // spectrogram
	{tool: "ffmpeg", from: audioExt, to: []string{"mp4", "webm", "mkv", "mov"}, run: runFFmpeg},  // waveform video
	{tool: "ffmpeg", from: []string{"gif", "png", "jpg"}, to: videoExt, cost: 2, run: runFFmpeg}, // cost 2: png -> mp3 goes via the spectrogram, not a silent video
	{tool: "ffmpeg", from: subExt, to: subExt, run: runFFmpeg},
	{from: []string{"png", "jpg", "gif"}, to: []string{"wav"}, run: imageToWav},
	{tool: "tesseract", from: []string{"png", "jpg", "tiff"}, to: []string{"txt"}, run: func(ctx context.Context, in, out string, o opts) error {
		return sh(ctx, "tesseract", in, strings.TrimSuffix(out, ".txt"))
	}},
	{tool: "gs", from: []string{"pdf"}, to: []string{"png", "jpg"}, pages: true, run: runGsFirstPage},
	{tool: "gs", from: []string{"ps", "eps"}, to: []string{"pdf"}, run: func(ctx context.Context, in, out string, o opts) error {
		return sh(ctx, "gs", "-dSAFER", "-dBATCH", "-dNOPAUSE", "-dQUIET", "-sDEVICE=pdfwrite", "-sOutputFile="+out, in)
	}},
	{tool: "pdftotext", from: []string{"pdf"}, to: []string{"txt"}, run: func(ctx context.Context, in, out string, o opts) error {
		return sh(ctx, "pdftotext", "-layout", in, out)
	}},
	{tool: "soffice", from: []string{"doc", "docx", "odt", "rtf", "txt"}, to: append(slices.Clone(textDocs), "pdf"), run: runSoffice}, // html opens in Writer/Web, which cannot export doc/rtf
	{tool: "soffice", from: sheets, to: []string{"xls", "xlsx", "ods", "csv", "pdf"}, run: runSoffice},
	{tool: "soffice", from: slidesExt, to: []string{"ppt", "pptx", "odp", "pdf"}, run: runSoffice},
	{tool: "pandoc", from: pandocIn, to: pandocOut, run: func(ctx context.Context, in, out string, o opts) error {
		args := []string{"-o", out, in}
		if ext(out) == "txt" {
			args = append(args, "-t", "plain")
		}
		return sh(ctx, "pandoc", args...)
	}},
	{tool: "ebook-convert", from: ebooks, to: ebooks, run: func(ctx context.Context, in, out string, o opts) error {
		if b, err := os.ReadFile(in); err == nil && ext(in) == "txt" && strings.TrimSpace(string(b)) == "" {
			// calibre cannot build an azw3 from an empty text; give it one blank page.
			blank := out + ".txt"
			if err := os.WriteFile(blank, []byte("\f\n"), 0o644); err != nil {
				return err
			}
			defer os.Remove(blank)
			in = blank
		}
		return sh(ctx, "ebook-convert", in, out)
	}},
	{from: textLike, to: []string{"txt"}, run: copyFile},
	{from: docReaders, to: []string{"txt"}, cost: 3, run: readDocText},
	{from: []string{"txt"}, to: docWriters, cost: 3, run: writeDoc},
	{from: []string{"xlsx", "ods"}, to: []string{"csv"}, cost: 3, run: readSheetCSV},
	{from: []string{"csv"}, to: []string{"xlsx", "ods"}, cost: 3, run: writeSheet},
	{from: []string{"txt"}, to: []string{"csv"}, cost: 3, run: copyFile}, // one line = one row
	{tool: "magick", from: []string{"txt"}, to: []string{"pdf", "png"}, run: func(ctx context.Context, in, out string, o opts) error {
		src := "text:" + in
		if ext(out) == "png" {
			src += "[0]"
		}
		return sh(ctx, "magick", src, out)
	}},
	{tool: "bsdtar", from: archIn, to: archOut, run: runRepack},
	// Fallbacks for anything else: pack it, or draw its raw bytes as pixels.
	{tool: "bsdtar", from: []string{"*"}, to: archOut, run: runPack},
	{from: []string{"*"}, to: []string{"png"}, cost: 22, run: bytesToPNG}, // text/audio describe unknown files better
	{from: []string{"*"}, to: []string{"txt"}, cost: 20, run: bytesToText},
	{from: []string{"*"}, to: []string{"wav"}, cost: 20, run: bytesToWav},
}

var aliases = map[string]string{"jpeg": "jpg", "jpe": "jpg", "tif": "tiff", "heif": "heic", "htm": "html",
	"markdown": "md", "tgz": "tar.gz", "txz": "tar.xz", "tbz2": "tar.bz2", "mpeg": "mpg", "oga": "ogg",
	"aif": "aiff", "yml": "yaml", "text": "txt"}

func norm(e string) string {
	e = strings.ToLower(strings.TrimPrefix(e, "."))
	if a, ok := aliases[e]; ok {
		return a
	}
	return e
}

// ext returns the normalized format of a path ("tar.gz", "jpg", ...).
func ext(p string) string {
	l := strings.ToLower(filepath.Base(p))
	for _, d := range []string{"tar.gz", "tar.xz", "tar.zst", "tar.bz2"} {
		if strings.HasSuffix(l, "."+d) {
			return d
		}
	}
	return norm(filepath.Ext(l))
}

func stem(p string) string {
	b := filepath.Base(p)
	if e := ext(p); strings.HasPrefix(e, "tar.") {
		return b[:len(b)-len(e)-1]
	}
	return strings.TrimSuffix(b, filepath.Ext(b))
}

var lookCache sync.Map

func available(tool string) bool {
	if tool == "" {
		return true
	}
	if v, ok := lookCache.Load(tool); ok {
		return v.(bool)
	}
	_, err := exec.LookPath(tool)
	lookCache.Store(tool, err == nil)
	return err == nil
}

func accepts(e *edge, format string) bool {
	return available(e.tool) && (slices.Contains(e.from, format) || slices.Contains(e.from, "*"))
}

type hop struct {
	e  *edge
	to string
}

// shortest is Dijkstra from one format to every reachable format.
// Ties go to the node discovered first, which follows edge order.
func shortest(from string) (prev map[string]hop, prevNode map[string]string) {
	prev, prevNode = map[string]hop{}, map[string]string{}
	dist := map[string]int{from: 0}
	order := []string{from}
	done := map[string]bool{}
	for {
		n, best := "", -1
		for _, c := range order {
			if !done[c] && (best < 0 || dist[c] < best) {
				n, best = c, dist[c]
			}
		}
		if best < 0 {
			return
		}
		done[n] = true
		for i := range edges {
			e := &edges[i]
			if !accepts(e, n) {
				continue
			}
			for _, t := range e.to {
				d := best + max(e.cost, 1)
				old, seen := dist[t]
				if seen && d >= old {
					continue
				}
				if !seen {
					order = append(order, t)
				}
				dist[t], prev[t], prevNode[t] = d, hop{e, t}, n
			}
		}
	}
}

func route(from, to string) []hop {
	if from == to {
		return nil
	}
	prev, prevNode := shortest(from)
	var path []hop
	for n := to; n != from; n = prevNode[n] {
		h, ok := prev[n]
		if !ok {
			return nil
		}
		path = append([]hop{h}, path...)
	}
	return path
}

// known reports whether some real (non-fallback) edge reads this format.
func known(format string) bool {
	for i := range edges {
		if format != "" && slices.Contains(edges[i].from, format) {
			return true
		}
	}
	return false
}

// formatOf is the extension, or the sniffed content type when the extension is unknown.
func formatOf(p string) string {
	if e := ext(p); known(e) || isDir(p) {
		return e
	}
	if s := sniff(p); known(s) {
		return s
	}
	if s := probe(p); s != "" {
		return s
	}
	return ext(p)
}

// probe asks the tools themselves whether they can read a file file(1) does not
// know, and names a format of that kind so routing picks the right tool.
// ffmpeg, ImageMagick and libarchive all detect the real format from content.
func probe(p string) string {
	if available("ffprobe") {
		out, _ := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_type:format=format_name", "-of", "default=nw=1:nk=1", p).Output()
		s := string(out)
		image := strings.Contains(s, "image2") || strings.Contains(s, "_pipe")
		switch {
		case strings.Contains(s, "video") && !image:
			return "mkv"
		case strings.Contains(s, "audio"):
			return "wav"
		}
	}
	if available("magick") && exec.Command("magick", "identify", "-ping", p+"[0]").Run() == nil {
		return "tiff"
	}
	if available("bsdtar") && exec.Command("bsdtar", "-tf", p).Run() == nil {
		return "tar"
	}
	return ""
}

// sniff guesses the format from file content via file(1).
func sniff(p string) string {
	out, err := exec.Command("file", "-b", "--extension", p).Output()
	if e, _, _ := strings.Cut(strings.TrimSpace(string(out)), "/"); err == nil && e != "???" {
		return norm(e)
	}
	out, _ = exec.Command("file", "-b", "--mime-type", p).Output()
	if strings.HasPrefix(string(out), "text/") {
		return "txt"
	}
	return ""
}

func targets(from string) []string {
	prev, _ := shortest(from)
	var out []string
	for t := range prev {
		if t != from {
			out = append(out, t)
		}
	}
	return out
}

func outDir(in string, o opts) (string, error) {
	dir := o.outDir
	if dir == "" {
		dir = filepath.Dir(in)
	}
	return dir, os.MkdirAll(dir, 0o755)
}

func convert(ctx context.Context, in, to string, o opts) (string, error) {
	to = norm(to)
	dir, err := outDir(in, o)
	if err != nil {
		return "", err
	}
	// Temp dir next to the output so the final rename never crosses filesystems.
	tmp, err := os.MkdirTemp(dir, ".pifile-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)

	cur, from := in, ext(in)
	if !known(from) {
		// Missing or wrong extension: trust the content instead.
		if s := formatOf(in); s != from && known(s) {
			abs, _ := filepath.Abs(in)
			cur, from = filepath.Join(tmp, "src."+s), s
			if err := os.Symlink(abs, cur); err != nil {
				return "", err
			}
		}
	}
	path := route(from, to)
	if path == nil {
		return "", fmt.Errorf("%s: no route %s -> %s", filepath.Base(in), from, to)
	}
	for i, h := range path {
		next := filepath.Join(tmp, strconv.Itoa(i)+"."+h.to)
		if h.e.pages && i == len(path)-1 && !o.single {
			pages := filepath.Join(tmp, "pages")
			if err := os.Mkdir(pages, 0o755); err != nil {
				return "", err
			}
			if err := runGsPages(ctx, cur, filepath.Join(pages, stem(in)+"-%03d."+h.to), o); err != nil {
				return "", err
			}
			files, _ := os.ReadDir(pages)
			if len(files) == 1 {
				cur = filepath.Join(pages, files[0].Name())
				break
			}
			out, err := reserveDir(dir, stem(in)+" ("+h.to+")")
			if err != nil {
				return "", err
			}
			os.Remove(out)
			return out, os.Rename(pages, out)
		}
		if err := runHop(ctx, cur, next, o); err != nil {
			return "", fmt.Errorf("%s: %w", filepath.Base(in), err)
		}
		cur = next
	}
	final, err := reserve(dir, stem(in), to)
	if err != nil {
		return "", err
	}
	if err := os.Rename(cur, final); err != nil {
		os.Remove(final)
		return "", err
	}
	return final, nil
}

// runHop tries every available tool that can do this single step, cheapest and earliest first.
func runHop(ctx context.Context, in, out string, o opts) error {
	var cands []*edge
	for i := range edges {
		if e := &edges[i]; accepts(e, ext(in)) && slices.Contains(e.to, ext(out)) {
			cands = append(cands, e)
		}
	}
	slices.SortStableFunc(cands, func(a, b *edge) int { return max(a.cost, 1) - max(b.cost, 1) })
	var errs []error
	for _, e := range cands {
		err := e.run(ctx, in, out, o)
		if err == nil {
			return nil
		}
		errs = append(errs, err)
		os.Remove(out)
	}
	return errors.Join(errs...)
}

// reserve atomically claims "name.ext", "name (2).ext", ... so parallel jobs never overwrite anything.
func reserve(dir, name, e string) (string, error) {
	for n := 1; ; n++ {
		p := filepath.Join(dir, name+"."+e)
		if n > 1 {
			p = filepath.Join(dir, fmt.Sprintf("%s (%d).%s", name, n, e))
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			return p, f.Close()
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
}

func reserveDir(dir, name string) (string, error) {
	for n := 1; ; n++ {
		p := filepath.Join(dir, name)
		if n > 1 {
			p = filepath.Join(dir, fmt.Sprintf("%s (%d)", name, n))
		}
		err := os.Mkdir(p, 0o755)
		if err == nil {
			return p, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
}

func sh(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if i := strings.LastIndexByte(msg, '\n'); i >= 0 {
			msg = msg[i+1:]
		}
		return fmt.Errorf("%s: %v: %s", name, err, msg)
	}
	return nil
}

func animated(e string) bool { return e == "gif" || e == "webp" }

func lossy(e string) bool {
	return slices.Contains([]string{"jpg", "webp", "avif", "heic", "jxl", "jp2"}, e)
}

func runVips(ctx context.Context, in, out string, o opts) error {
	src, dst := in, out
	if animated(ext(in)) && animated(ext(out)) {
		src += "[n=-1]"
	}
	if o.quality > 0 && lossy(ext(out)) {
		dst += fmt.Sprintf("[Q=%d]", o.quality)
	}
	if o.size > 0 {
		s := strconv.Itoa(o.size)
		return sh(ctx, "vips", "thumbnail", src, dst, s, "--height", s, "--size", "down")
	}
	return sh(ctx, "vips", "copy", src, dst)
}

func runMagick(ctx context.Context, in, out string, o opts) error {
	src := in
	// Layered/multi-frame inputs would otherwise write one file per frame.
	if !slices.Contains([]string{"gif", "webp", "tiff", "pdf", "psd"}, ext(out)) {
		src += "[0]"
	}
	args := []string{src}
	if o.size > 0 {
		args = append(args, "-resize", fmt.Sprintf("%dx%d>", o.size, o.size))
	}
	if o.quality > 0 {
		args = append(args, "-quality", strconv.Itoa(o.quality))
	}
	if ext(out) == "ico" {
		args = append(args, "-resize", "256x256>", "-define", "icon:auto-resize=256,128,64,48,32,16")
	}
	return sh(ctx, "magick", append(args, out)...)
}

func isVideo(e string) bool { return slices.Contains(videoExt, e) }
func isAudio(e string) bool { return slices.Contains(audioExt, e) }

func runFFmpeg(ctx context.Context, in, out string, o opts) error {
	from, to := ext(in), ext(out)
	pre := []string{"-hide_banner", "-loglevel", "error", "-y"}
	base := append(slices.Clone(pre), "-i", in)

	// Remux without re-encoding when possible: orders of magnitude faster.
	if isVideo(from) && isVideo(to) && o.quality == 0 && o.size == 0 && to != "mpg" && playable(ctx, in) {
		if sh(ctx, "ffmpeg", append(base, "-map", "0", "-c", "copy", out)...) == nil {
			return nil
		}
	}
	if isVideo(from) && isAudio(to) && !hasAudio(ctx, in) {
		// Silent video: sonify its first frame instead of failing.
		frame, wav := out+".png", out+".wav"
		defer os.Remove(frame)
		defer os.Remove(wav)
		if err := sh(ctx, "ffmpeg", append(base, "-frames:v", "1", frame)...); err != nil {
			return err
		}
		if err := imageToWav(ctx, frame, wav, o); err != nil {
			return err
		}
		return sh(ctx, "ffmpeg", append(pre, "-i", wav, out)...)
	}
	var args []string
	switch {
	case isAudio(from) && (to == "png" || to == "jpg"):
		args = []string{"-lavfi", "showspectrumpic=s=1024x512:legend=0", "-frames:v", "1"}
	case isAudio(from) && isVideo(to):
		args = []string{"-filter_complex", "[0:a]showwaves=s=1280x720:mode=cline:colors=white,format=yuv420p[v]", "-map", "[v]", "-map", "0:a"}
	case (from == "png" || from == "jpg") && isVideo(to):
		d := o.duration
		if d <= 0 {
			d = 2
		}
		base = append(slices.Clone(pre), "-loop", "1", "-t", strconv.FormatFloat(d, 'f', -1, 64), "-i", in)
		args = []string{"-r", "30", "-pix_fmt", "yuv420p", "-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2"}
	case isVideo(from) && (to == "png" || to == "jpg"):
		args = []string{"-frames:v", "1"}
	case to == "gif":
		w := 480
		if o.size > 0 {
			w = o.size
		}
		args = []string{"-vf", fmt.Sprintf("fps=15,scale=%d:-1:flags=lanczos,split[a][b];[a]palettegen[p];[b][p]paletteuse", w)}
	case to == "webp":
		args = []string{"-c:v", "libwebp_anim", "-loop", "0", "-an", "-vf", "fps=15"}
	case isAudio(to):
		args = []string{"-vn"}
	case from == "gif":
		vf := "scale=trunc(iw/2)*2:trunc(ih/2)*2"
		if to == "ts" {
			vf += ",tpad=stop_mode=clone:stop_duration=0.5" // a 1-frame gif muxes to an empty ts
		}
		args = []string{"-r", "30", "-pix_fmt", "yuv420p", "-vf", vf} // VFR gif gives ts with no timestamps
	case isVideo(to) && o.size > 0:
		args = []string{"-vf", fmt.Sprintf("scale=-2:%d", o.size)}
	}
	if to == "mpg" {
		args = append(args, "-c:v", "mpeg2video", "-q:v", "3", "-c:a", "mp2") // MPEG-PS cannot carry H.264 properly
	}
	if to == "3gp" {
		args = append(args, "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac") // default h263 rejects most frame sizes
	}
	if o.quality > 0 && isVideo(to) {
		args = append(args, "-crf", strconv.Itoa(51-o.quality*51/100))
	}
	return sh(ctx, "ffmpeg", append(append(base, args...), out)...)
}

// playable reports whether every stream uses a codec common players handle,
// so copying it into another container gives a file people can actually play.
func playable(ctx context.Context, in string) bool {
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_entries", "stream=codec_name", "-of", "csv=p=0", in).Output()
	if err != nil {
		return false
	}
	common := []string{"h264", "hevc", "av1", "vp8", "vp9", "mpeg4", "aac", "mp3", "opus", "vorbis", "flac", "ac3", "eac3", "subrip", "ass", "webvtt", "mov_text"}
	for _, c := range strings.Fields(string(out)) {
		if !slices.Contains(common, strings.TrimSuffix(c, ",")) {
			return false
		}
	}
	return true
}

func gsImage(ctx context.Context, in, out string, o opts, extra ...string) error {
	args := []string{"-dSAFER", "-dBATCH", "-dNOPAUSE", "-dQUIET", "-r150", "-sDEVICE=png16m"}
	if ext(out) == "jpg" {
		args[len(args)-1] = "-sDEVICE=jpeg"
		if o.quality > 0 {
			args = append(args, fmt.Sprintf("-dJPEGQ=%d", o.quality))
		}
	}
	return sh(ctx, "gs", append(append(args, extra...), "-sOutputFile="+out, in)...)
}

func runGsFirstPage(ctx context.Context, in, out string, o opts) error {
	return gsImage(ctx, in, out, o, "-dFirstPage=1", "-dLastPage=1")
}

func runGsPages(ctx context.Context, in, pattern string, o opts) error {
	return gsImage(ctx, in, pattern, o)
}

func runSoffice(ctx context.Context, in, out string, o opts) error {
	// Own profile per run: parallel soffice instances otherwise collide on the lock.
	tmp, err := os.MkdirTemp(filepath.Dir(out), ".soffice-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	// The AppImage engine changes directory, so every path must be absolute.
	tmp, _ = filepath.Abs(tmp)
	in, _ = filepath.Abs(in)
	args := []string{"-env:UserInstallation=file://" + tmp + "/profile", "--headless", "--convert-to", ext(out), "--outdir", tmp, in}
	err = sh(ctx, "soffice", args...)
	if err != nil && strings.Contains(err.Error(), "exit status 81") {
		err = sh(ctx, "soffice", args...) // 81 = fresh profile created, run again
	}
	if err != nil {
		return err
	}
	return os.Rename(filepath.Join(tmp, stem(in)+"."+ext(out)), out)
}

// archive writes the given paths (files or folders) into one archive, format from out's extension.
func archive(ctx context.Context, out string, paths ...string) error {
	args := []string{"-acf", out}
	switch ext(out) {
	case "7z":
		args = []string{"--format", "7zip", "-cf", out}
	case "zip", "cbz":
		args = []string{"--format", "zip", "-cf", out}
	}
	for _, p := range paths {
		args = append(args, "-C", filepath.Dir(p), filepath.Base(p))
	}
	return sh(ctx, "bsdtar", args...)
}

func runPack(ctx context.Context, in, out string, o opts) error { return archive(ctx, out, in) }

func runRepack(ctx context.Context, in, out string, o opts) error {
	d := out + ".d"
	if err := os.Mkdir(d, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(d)
	if err := sh(ctx, "bsdtar", "-xf", in, "-C", d); err != nil {
		return err
	}
	entries, err := os.ReadDir(d)
	if err != nil {
		return err
	}
	var paths []string
	for _, e := range entries {
		paths = append(paths, filepath.Join(d, e.Name()))
	}
	return archive(ctx, out, paths...)
}

var jobs = 0 // 0 = NumCPU

// parallel runs f over items with a bounded worker pool, keeping result order.
// One pool for all tools. ffmpeg and soffice are multithreaded themselves; lower -j if they thrash.
func parallel(items []string, f func(int, string) (string, error)) ([]string, []error) {
	n := jobs
	if n <= 0 {
		n = numCPU()
	}
	outs, errs := make([]string, len(items)), make([]error, len(items))
	sem := make(chan struct{}, n)
	var wg sync.WaitGroup
	for i, it := range items {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			outs[i], errs[i] = f(i, it)
		}()
	}
	wg.Wait()
	return outs, errs
}
