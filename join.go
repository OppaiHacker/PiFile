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
)

// What a set of files can be joined into, and the format each input is first converted to.
var joinVia = map[string]string{
	"pdf": "pdf", "txt": "txt",
	"png": "png", "jpg": "png", "tiff": "png", "psd": "png", // strip / pages / layers
	"mp4": "clip", "mkv": "clip", "webm": "clip", "mov": "clip", "gif": "clip", "webp": "clip",
	"mp3": "wav", "wav": "wav", "flac": "wav", "ogg": "wav", "m4a": "wav", "opus": "wav",
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func joinTargets(files []string) []string {
	var out []string
	if available("bsdtar") {
		out = append(out, archOut...)
	}
	for _, p := range files {
		if isDir(p) {
			return out
		}
	}
	for _, to := range []string{"pdf", "gif", "mp4", "webp", "mkv", "webm", "mov", "png", "jpg", "tiff", "psd", "mp3", "wav", "flac", "ogg", "m4a", "opus", "txt"} {
		if canJoin(files, to) {
			out = append(out, to)
		}
	}
	return out
}

func canJoin(files []string, to string) bool {
	via := joinVia[to]
	for _, p := range files {
		e := formatOf(p)
		switch {
		case via == "clip":
			if !available("ffmpeg") || !(isVideo(e) || isAudio(e) || e == "gif" || e == "png" || route(e, "png") != nil) {
				return false
			}
		case e != via && route(e, via) == nil:
			return false
		}
	}
	switch via {
	case "pdf":
		return available("gs")
	case "png":
		return available("magick")
	case "wav":
		return available("ffmpeg")
	}
	return true
}

func join(ctx context.Context, files []string, to string, o opts) (string, error) {
	to = norm(to)
	dir, err := outDir(files[0], o)
	if err != nil {
		return "", err
	}
	name := "joined"
	if len(files) == 1 || slices.Contains(archOut, to) {
		name = stem(files[0])
	}
	if slices.Contains(archOut, to) {
		out, err := reserve(dir, name, to)
		if err != nil {
			return "", err
		}
		abs := make([]string, len(files))
		for i, f := range files {
			abs[i], _ = filepath.Abs(f)
		}
		if err := archive(ctx, out, abs...); err != nil {
			os.Remove(out)
			return "", err
		}
		return out, nil
	}

	tmp, err := os.MkdirTemp(dir, ".pifile-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	via := joinVia[to]
	if via == "" {
		return "", fmt.Errorf("cannot join into %s", to)
	}
	parts, errs := parallel(files, func(i int, f string) (string, error) {
		if via == "clip" {
			return clip(ctx, f, filepath.Join(tmp, fmt.Sprintf("%04d.mp4", i)), o)
		}
		if ext(f) == via {
			return f, nil
		}
		return convert(ctx, f, via, opts{quality: o.quality, size: o.size, single: true, outDir: filepath.Join(tmp, strconv.Itoa(i))})
	})
	if err := errors.Join(errs...); err != nil {
		return "", err
	}

	res := filepath.Join(tmp, "joined."+to)
	switch via {
	case "pdf":
		err = sh(ctx, "gs", append([]string{"-dSAFER", "-dBATCH", "-dNOPAUSE", "-dQUIET", "-sDEVICE=pdfwrite", "-sOutputFile=" + res}, parts...)...)
	case "txt":
		err = concatFiles(res, parts)
	case "png":
		args := slices.Clone(parts)
		if to == "png" || to == "jpg" {
			args = append(args, "-resize", "x1080", "+append") // side by side, same height
		}
		err = sh(ctx, "magick", append(args, res)...)
	case "wav":
		err = ffconcat(ctx, tmp, parts, res, "-ar", "44100", "-ac", "2")
	case "clip":
		mp4 := filepath.Join(tmp, "joined.mp4")
		if err = ffconcat(ctx, tmp, parts, mp4, "-c", "copy"); err == nil && to != "mp4" {
			err = runHop(ctx, mp4, res, o)
		} else {
			res = mp4
		}
	}
	if err != nil {
		return "", err
	}
	out, err := reserve(dir, name, to)
	if err != nil {
		return "", err
	}
	return out, os.Rename(res, out)
}

func concatFiles(out string, parts []string) error {
	var buf []byte
	for _, p := range parts {
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		buf = append(append(buf, b...), '\n')
	}
	return os.WriteFile(out, buf, 0o644)
}

func ffconcat(ctx context.Context, tmp string, parts []string, out string, args ...string) error {
	var list strings.Builder
	for _, p := range parts {
		abs, _ := filepath.Abs(p)
		fmt.Fprintf(&list, "file '%s'\n", strings.ReplaceAll(abs, "'", `'\''`))
	}
	lf := filepath.Join(tmp, "list.txt")
	if err := os.WriteFile(lf, []byte(list.String()), 0o644); err != nil {
		return err
	}
	return sh(ctx, "ffmpeg", append(append([]string{"-hide_banner", "-loglevel", "error", "-y", "-f", "concat", "-safe", "0", "-i", lf}, args...), out)...)
}

// clip turns any input into a uniform H.264/AAC segment so all segments concat without re-encoding.
// Clips share a fixed 30 fps 16:9 canvas; other aspect ratios are letterboxed.
func clip(ctx context.Context, in, out string, o opts) (string, error) {
	h := 720
	if o.size > 0 {
		h = o.size &^ 1
	}
	w := (h * 16 / 9) &^ 1
	dur := o.duration
	if dur <= 0 {
		dur = 2
	}
	d := strconv.FormatFloat(dur, 'f', -1, 64)
	fit := fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2,setsar=1,fps=30,format=yuv420p", w, h, w, h)
	silence := []string{"-f", "lavfi", "-i", "anullsrc=r=44100:cl=stereo"}
	enc := []string{"-c:v", "libx264", "-preset", "veryfast", "-c:a", "aac", "-ar", "44100", "-ac", "2", "-shortest", out}
	pre := []string{"-hide_banner", "-loglevel", "error", "-y"}

	e := ext(in)
	var args []string
	switch {
	case isAudio(e):
		args = append(pre, "-i", in, "-filter_complex",
			fmt.Sprintf("[0:a]showwaves=s=%dx%d:mode=cline:colors=white,format=yuv420p[v]", w, h), "-map", "[v]", "-map", "0:a")
	case isVideo(e) || e == "gif":
		args = append(append(pre, "-i", in), silence...)
		args = append(args, "-vf", fit, "-map", "0:v:0")
		if hasAudio(ctx, in) {
			args = append(args, "-map", "0:a:0")
		} else {
			args = append(args, "-map", "1:a")
		}
	default:
		img := in
		if e != "png" && e != "jpg" {
			p, err := convert(ctx, in, "png", opts{single: true, outDir: filepath.Dir(out)})
			if err != nil {
				return "", err
			}
			defer os.Remove(p)
			img = p
		}
		args = append(append(pre, "-loop", "1", "-t", d, "-i", img), silence...)
		args = append(args, "-vf", fit, "-map", "0:v", "-map", "1:a")
	}
	return out, sh(ctx, "ffmpeg", append(args, enc...)...)
}

func hasAudio(ctx context.Context, p string) bool {
	out, _ := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-select_streams", "a", "-show_entries", "stream=index", "-of", "csv=p=0", p).Output()
	return len(strings.TrimSpace(string(out))) > 0
}
