package main

import (
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePNG(t *testing.T, p string) {
	img := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for x := range 64 {
		for y := range 48 {
			img.Set(x, y, color.RGBA{uint8(x * 4), uint8(y * 5), 128, 255})
		}
	}
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	png.Encode(f, img)
}

func TestExtAndStem(t *testing.T) {
	for in, want := range map[string][2]string{"a/B.JPEG": {"jpg", "B"}, "x.tar.gz": {"tar.gz", "x"}, "Makefile": {"", "Makefile"}} {
		if ext(in) != want[0] || stem(in) != want[1] {
			t.Errorf("%s: got %s %s", in, ext(in), stem(in))
		}
	}
}

// Every pair below must actually produce a non-empty file with the installed tools.
func TestAnyToAny(t *testing.T) {
	for _, tool := range []string{"vips", "magick", "ffmpeg", "gs", "bsdtar"} {
		if !available(tool) {
			t.Skip("missing " + tool)
		}
	}
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "pic.png")
	writePNG(t, src)
	blob := filepath.Join(dir, "blob.xyz")
	os.WriteFile(blob, []byte("\x00\x01\x02\xffwhatever bytes 123\x00\x9c"), 0o644)

	chain := map[string]string{} // format -> produced file, reused as input below
	for _, to := range []string{"mp3", "mp4", "gif", "webp", "pdf", "psd", "jpg", "zip", "txt", "ico"} {
		out, err := convert(ctx, src, to, opts{})
		if err != nil {
			t.Fatalf("png -> %s: %v", to, err)
		}
		if st, err := os.Stat(out); err != nil || (st.Size() == 0 && to != "txt") { // OCR of a gradient is legitimately empty
			t.Fatalf("png -> %s: empty output %s", to, out)
		}
		chain[to] = out
	}
	for _, c := range [][2]string{{"mp3", "png"}, {"mp3", "pdf"}, {"mp4", "mp3"}, {"mp4", "psd"}, {"pdf", "mp4"}, {"psd", "webp"}, {"zip", "7z"}, {"gif", "mp4"}} {
		if _, err := convert(ctx, chain[c[0]], c[1], opts{}); err != nil {
			t.Errorf("%s -> %s: %v", c[0], c[1], err)
		}
	}
	if _, err := convert(ctx, blob, "mp3", opts{}); err != nil {
		t.Errorf("unknown -> mp3: %v", err)
	}
	if out, err := convert(ctx, blob, "docx", opts{}); err != nil {
		t.Errorf("unknown -> docx: %v", err)
	} else if back, err := convert(ctx, out, "txt", opts{}); err != nil {
		t.Errorf("docx -> txt: %v", err)
	} else if b, _ := os.ReadFile(back); !strings.Contains(string(b), "whatever bytes") {
		t.Errorf("unknown -> docx lost the readable strings: %q", b)
	}
	for _, to := range []string{"gif", "mp4", "pdf", "zip", "png", "mp3"} {
		if _, err := join(ctx, []string{src, chain["jpg"], chain["mp4"]}, to, opts{duration: 0.5}); err != nil {
			t.Errorf("join -> %s: %v", to, err)
		}
	}
	// Second conversion to the same name must not overwrite the first.
	a, _ := convert(ctx, src, "jpg", opts{})
	if a == chain["jpg"] {
		t.Error("output overwritten")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".pifile-*")); len(left) > 0 {
		t.Errorf("temp dirs left: %v", left)
	}
}
