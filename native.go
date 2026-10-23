package main

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func numCPU() int { return runtime.NumCPU() }

func copyFile(ctx context.Context, in, out string, o opts) error {
	src, err := os.Open(in)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.Create(out)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}

// imageToWav paints the image into a spectrogram: x is time, y is frequency, brightness is loudness.
// Converting the result back (audio -> png) shows the picture again.
func imageToWav(ctx context.Context, in, out string, o opts) error {
	f, err := os.Open(in)
	if err != nil {
		return err
	}
	img, _, err := image.Decode(f)
	f.Close()
	if err != nil {
		return err
	}
	const (
		rate  = 44100
		rows  = 160
		fmin  = 300.0
		fmax  = 16000.0
		colMs = 25
	)
	b := img.Bounds()
	cols := min(b.Dx(), 400)
	amp := make([][]float64, cols) // amp[x][y]
	for x := range cols {
		amp[x] = make([]float64, rows)
		for y := range rows {
			r, g, bl, _ := img.At(b.Min.X+x*b.Dx()/cols, b.Min.Y+y*b.Dy()/rows).RGBA()
			l := (0.299*float64(r) + 0.587*float64(g) + 0.114*float64(bl)) / 65535
			amp[x][y] = l * l
		}
	}
	perCol := rate * colMs / 1000
	samples := make([]float64, cols*perCol)
	phase := make([]float64, rows)
	step := make([]float64, rows)
	for y := range rows {
		freq := fmax - float64(y)*(fmax-fmin)/(rows-1) // top row = highest pitch
		step[y] = 2 * math.Pi * freq / rate
		phase[y] = float64(y*y) * 0.7 // spread phases so peaks don't pile up
	}
	peak := 1e-9
	for i := range samples {
		x, t := i/perCol, float64(i%perCol)/float64(perCol)
		nx := min(x+1, cols-1)
		var s float64
		for y := range rows {
			a := amp[x][y]*(1-t) + amp[nx][y]*t // interpolate to avoid clicks
			if a > 0.002 {
				s += a * math.Sin(phase[y])
			}
			phase[y] += step[y]
		}
		samples[i] = s
		peak = max(peak, math.Abs(s))
	}
	pcm := make([]int16, len(samples))
	for i, s := range samples {
		pcm[i] = int16(s / peak * 30000)
	}
	return writeWav(out, rate, pcm)
}

func writeWav(path string, rate int, pcm []int16) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	n := uint32(len(pcm) * 2)
	hdr := []any{[4]byte{'R', 'I', 'F', 'F'}, 36 + n, [4]byte{'W', 'A', 'V', 'E'},
		[4]byte{'f', 'm', 't', ' '}, uint32(16), uint16(1), uint16(1), uint32(rate), uint32(rate * 2), uint16(2), uint16(16),
		[4]byte{'d', 'a', 't', 'a'}, n, pcm}
	for _, v := range hdr {
		if err := binary.Write(f, binary.LittleEndian, v); err != nil {
			f.Close()
			return err
		}
	}
	return f.Close()
}

// bytesToPNG draws any file's raw bytes as RGB pixels: the last-resort bridge
// that makes every format reachable from every other.
// Reads at most 48 MB (4000x4000 px); bigger files are truncated.
func bytesToPNG(ctx context.Context, in, out string, o opts) error {
	f, err := os.Open(in)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(f, 48<<20))
	f.Close()
	if err != nil {
		return err
	}
	px := max((len(data)+2)/3, 1)
	w := max(int(math.Ceil(math.Sqrt(float64(px)))), 1)
	h := (px + w - 1) / w
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := range px {
		p := img.Pix[i*4 : i*4+4]
		copy(p[:3], data[min(i*3, len(data)):min(i*3+3, len(data))])
		p[3] = 255
	}
	for i := px * 4; i < len(img.Pix); i += 4 {
		img.Pix[i+3] = 255
	}
	dst, err := os.Create(out)
	if err != nil {
		return err
	}
	if err := png.Encode(dst, img); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}

// readHead reads up to 48 MB of any file for the byte-level fallbacks.
func readHead(in string) ([]byte, error) {
	f, err := os.Open(in)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 48<<20))
}

// bytesToText describes a file nothing else can read: its type as libmagic
// sees it, its size, the readable strings inside and a hex dump of its start.
func bytesToText(ctx context.Context, in, out string, o opts) error {
	data, err := readHead(in)
	if err != nil {
		return err
	}
	desc, _ := exec.CommandContext(ctx, "file", "-b", in).Output()
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s\nType: %sSize: %d bytes\n\nHex dump (first 512 bytes):\n%s\nReadable strings:\n%s",
		filepath.Base(in), desc, len(data), hex.Dump(data[:min(len(data), 512)]), asciiStrings(data))
	return os.WriteFile(out, []byte(sb.String()), 0o644)
}

// asciiStrings is strings(1): runs of 4+ printable ASCII bytes, one per line.
func asciiStrings(b []byte) string {
	var sb strings.Builder
	start := -1
	for i, c := range append(b, 0) {
		if c >= 0x20 && c < 0x7f || c == '\t' {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 && i-start >= 4 {
			sb.Write(append(b[start:i:i], '\n'))
		}
		start = -1
	}
	return sb.String()
}

// bytesToWav plays the raw bytes as 8-bit PCM, the way audio editors import raw data.
func bytesToWav(ctx context.Context, in, out string, o opts) error {
	data, err := readHead(in)
	if err != nil {
		return err
	}
	pcm := make([]int16, max(len(data), 22050/10))
	for i, b := range data {
		pcm[i] = int16(int(b)-128) << 8
	}
	return writeWav(out, 22050, pcm)
}
