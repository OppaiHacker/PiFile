package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

// Portable engines installed without root into ~/.local/share/pifile/engines.
// Versions are pinned; update the URLs here to upgrade.
type engine struct{ tool, url, file, bin string }

var engines = []engine{
	{"pandoc", "https://github.com/jgm/pandoc/releases/download/3.12.1/pandoc-3.12.1-linux-amd64.tar.gz", "pandoc.tar.gz", "pandoc-3.12.1/bin/pandoc"},
	{"ebook-convert", "https://github.com/kovidgoyal/calibre/releases/download/v9.17.0/calibre-9.17.0-x86_64.txz", "calibre.txz", "ebook-convert"},
	{"soffice", "https://appimages.libreitalia.org/LibreOffice-fresh.basic-x86_64.AppImage", "libreoffice.AppImage", "squashfs-root/AppRun"},
}

func engineDir() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "pifile", "engines")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "pifile", "engines")
}

func init() {
	// Installed engines are found like any tool on PATH.
	os.Setenv("PATH", filepath.Join(engineDir(), "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func installEngines(ctx context.Context) error {
	bin := filepath.Join(engineDir(), "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return err
	}
	// Download and unpack all engines at once; the network is the bottleneck.
	var wg sync.WaitGroup
	errs := make([]error, len(engines))
	for i, e := range engines {
		if available(e.tool) {
			fmt.Printf("%-14s already installed\n", e.tool)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := installEngine(ctx, e, bin); err != nil {
				errs[i] = fmt.Errorf("%s: %w", e.tool, err)
				return
			}
			fmt.Printf("%-14s ok\n", e.tool)
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

func installEngine(ctx context.Context, e engine, bin string) error {
	dir := filepath.Join(engineDir(), e.tool)
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	fmt.Printf("%-14s downloading %s\n", e.tool, e.url)
	dl := filepath.Join(dir, e.file)
	if err := download(ctx, e.url, dl); err != nil {
		return err
	}
	var err error
	if filepath.Ext(dl) == ".AppImage" {
		os.Chmod(dl, 0o755)
		cmd := exec.CommandContext(ctx, dl, "--appimage-extract") // no FUSE needed
		cmd.Dir = dir
		err = cmd.Run()
	} else {
		err = sh(ctx, "bsdtar", "-xf", dl, "-C", dir)
	}
	os.Remove(dl)
	if err != nil {
		return err
	}
	link := filepath.Join(bin, e.tool)
	os.Remove(link)
	if err := os.Symlink(filepath.Join(dir, e.bin), link); err != nil {
		return err
	}
	lookCache.Delete(e.tool)
	return nil
}

func download(ctx context.Context, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download: %s", resp.Status)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
