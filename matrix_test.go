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
	"testing"
)

// PIFILE_MATRIX=1 go test -run Matrix: every source format below to every format it can reach.
func TestMatrix(t *testing.T) {
	if os.Getenv("PIFILE_MATRIX") == "" {
		t.Skip("set PIFILE_MATRIX=1")
	}
	jobs = 4 // keep the desktop responsive
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "seed.png")
	writePNG(t, src)
	txt := filepath.Join(dir, "seed.txt")
	os.WriteFile(txt, []byte("Hello matrix\nsecond line\n"), 0o644)
	// Build one sample per source format from the seeds.
	samples := map[string]string{"png": src, "txt": txt}
	for _, f := range []string{"jpg", "webp", "gif", "psd", "tiff", "heic", "avif", "bmp", "ico", "mp4", "mkv", "webm", "mp3", "wav", "flac", "ogg", "pdf", "zip", "7z", "tar.gz"} {
		out, err := convert(ctx, src, f, opts{single: true, outDir: filepath.Join(dir, "seed-"+f)})
		if err != nil {
			t.Fatalf("seed %s: %v", f, err)
		}
		samples[f] = out
	}
	for _, f := range []string{"docx", "odt", "rtf", "html", "md", "epub", "mobi", "csv", "xlsx", "ods", "pptx", "odp", "doc", "xls", "fb2", "srt"} {
		from := txt
		if f == "srt" {
			os.WriteFile(filepath.Join(dir, "s.srt"), []byte("1\n00:00:00,000 --> 00:00:01,000\nHi\n"), 0o644)
			samples[f] = filepath.Join(dir, "s.srt")
			continue
		}
		if f == "md" {
			from = txt
		}
		out, err := convert(ctx, from, f, opts{single: true, outDir: filepath.Join(dir, "seed-"+f)})
		if err != nil {
			t.Fatalf("seed %s: %v", f, err)
		}
		samples[f] = out
	}
	blob := filepath.Join(dir, "blob.qqq")
	os.WriteFile(blob, []byte{0, 1, 2, 3, 250, 99, 7}, 0o644)
	samples["qqq"] = blob

	var mu sync.Mutex
	total, failed := 0, 0
	var srcs []string
	for k := range samples {
		srcs = append(srcs, k)
	}
	slices.Sort(srcs)
	for _, from := range srcs {
		tg := targets(formatOf(samples[from]))
		slices.Sort(tg)
		parallel(tg, func(i int, to string) (string, error) {
			out, err := convert(ctx, samples[from], to, opts{single: true, outDir: filepath.Join(dir, "out", from, to)})
			mu.Lock()
			defer mu.Unlock()
			total++
			if err == nil {
				if st, e := os.Stat(out); e != nil || (st.Size() == 0 && to != "txt" && to != "csv") { // OCR of a picture without text is legitimately empty
					err = os.ErrNotExist
				}
			}
			if err == nil {
				err = validate(ctx, from, out, to)
			}
			if err != nil {
				failed++
				t.Errorf("%s -> %s: %v", from, to, err)
			}
			return out, err
		})
	}
	t.Logf("%d conversions, %d failed", total, failed)
}

var textSources = []string{"txt", "docx", "odt", "rtf", "html", "md", "epub", "mobi", "fb2", "doc", "pptx", "odp", "csv", "xlsx", "ods", "xls"}

// validate checks the output is a real file of its kind, not just non-empty bytes.
func validate(ctx context.Context, from, out, to string) error {
	run := func(name string, args ...string) (string, error) {
		b, err := exec.CommandContext(ctx, name, args...).Output()
		return strings.TrimSpace(string(b)), err
	}
	switch {
	case slices.Contains(archOut, to):
		list, err := run("bsdtar", "-tf", out)
		if err != nil || list == "" {
			return fmt.Errorf("archive empty or unreadable: %v", err)
		}
	case to == "pdf":
		if _, err := run("pdfinfo", out); err != nil {
			return fmt.Errorf("pdf unreadable: %v", err)
		}
	case isAudio(to) || isVideo(to):
		// MPEG-PS/TS often only carry a per-stream duration.
		d, err := run("ffprobe", "-v", "error", "-show_entries", "format=duration:stream=duration", "-of", "default=nw=1:nk=1", out)
		best := 0.0
		for _, f := range strings.Fields(d) {
			v, _ := strconv.ParseFloat(f, 64)
			best = max(best, v)
		}
		if err != nil || best <= 0 {
			return fmt.Errorf("media has no duration (%q): %v", d, err)
		}
	case slices.Contains(magickOut, to) || slices.Contains(rasterOut, to):
		if _, err := run("magick", "identify", out+"[0]"); err != nil {
			return fmt.Errorf("image unreadable: %v", err)
		}
	case slices.Contains(subExt, to):
		if _, err := run("ffprobe", "-v", "error", out); err != nil {
			return fmt.Errorf("subtitles unreadable: %v", err)
		}
	default: // documents: must read back as text, and keep the text when the source had some
		back := out
		if to != "txt" {
			var err error
			if back, err = convert(ctx, out, "txt", opts{outDir: filepath.Dir(out) + "/back"}); err != nil {
				return fmt.Errorf("document unreadable: %w", err)
			}
		}
		b, _ := os.ReadFile(back)
		if slices.Contains(textSources, from) && to != "csv" && !strings.Contains(strings.ToLower(string(b)), "hello") {
			return errors.New("text lost: " + strconv.Quote(string(b[:min(len(b), 80)])))
		}
	}
	return nil
}
