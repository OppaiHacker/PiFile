package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Built-in writers and readers must round-trip text without any engine.
func TestNativeOffice(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "doc.txt")
	os.WriteFile(src, []byte("Zażółć gęślą jaźń\n\nsecond <para> & more\n"), 0o644)
	for _, f := range docWriters {
		out := filepath.Join(dir, "x."+f)
		back := filepath.Join(dir, "x-"+f+".txt")
		if err := writeDoc(ctx, src, out, opts{}); err != nil {
			t.Fatalf("txt -> %s: %v", f, err)
		}
		if err := readDocText(ctx, out, back, opts{}); err != nil {
			t.Fatalf("%s -> txt: %v", f, err)
		}
		if b, _ := os.ReadFile(back); !strings.Contains(string(b), "Zażółć gęślą jaźń") || !strings.Contains(string(b), "second <para> & more") {
			t.Errorf("%s round trip lost text: %q", f, b)
		}
	}
	csvf := filepath.Join(dir, "t.csv")
	os.WriteFile(csvf, []byte("name,n\nala,3\n\"a,b\",4.5\n"), 0o644)
	for _, f := range []string{"xlsx", "ods"} {
		out, back := filepath.Join(dir, "t."+f), filepath.Join(dir, "t-"+f+".csv")
		if err := writeSheet(ctx, csvf, out, opts{}); err != nil {
			t.Fatal(err)
		}
		if err := readSheetCSV(ctx, out, back, opts{}); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(back); string(b) != "name,n\nala,3\n\"a,b\",4.5\n" {
			t.Errorf("%s round trip: %q", f, b)
		}
	}
}

func TestDocumentsWithEngines(t *testing.T) {
	for _, tool := range []string{"soffice", "pandoc", "ebook-convert", "magick"} {
		if !available(tool) {
			t.Skip("missing " + tool + " (pifile engines)")
		}
	}
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "doc.txt")
	os.WriteFile(src, []byte("Hello engines\n"), 0o644)
	for _, c := range [][2]string{{"txt", "docx"}, {"docx", "pdf"}, {"txt", "pptx"}, {"pptx", "png"}, {"txt", "epub"}, {"epub", "mobi"}, {"mobi", "txt"}, {"docx", "doc"}, {"doc", "mp3"}, {"txt", "xlsx"}, {"xlsx", "xls"}} {
		in := src
		if c[0] != "txt" {
			in = filepath.Join(dir, "doc."+c[0])
		}
		if _, err := convert(ctx, in, c[1], opts{}); err != nil {
			t.Errorf("%s -> %s: %v", c[0], c[1], err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "doc (2).txt")); !strings.Contains(string(b), "Hello engines") {
		t.Errorf("mobi -> txt lost text: %q", b)
	}
	// No extension: format comes from content.
	png := filepath.Join(dir, "noext")
	writePNG(t, png)
	if out, err := convert(ctx, png, "jpg", opts{}); err != nil || ext(out) != "jpg" {
		t.Errorf("sniffed png -> jpg: %v", err)
	}
}
