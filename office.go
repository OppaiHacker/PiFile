package main

// Built-in office/e-book readers and writers. Text only (no images, no styling):
// they make every document format reachable when LibreOffice/pandoc/calibre are
// missing. When those engines are installed they win, because these edges cost more.

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/csv"
	"encoding/xml"
	"errors"
	"fmt"
	"hash/crc32"
	"html"
	"io"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

var (
	docReaders = []string{"docx", "odt", "pptx", "odp", "epub", "fb2", "html", "rtf", "doc", "xls", "ppt", "mobi", "azw3"}
	docWriters = []string{"docx", "odt", "odp", "rtf", "html", "epub", "fb2"}
)

func readDocText(ctx context.Context, in, out string, o opts) error {
	var text string
	var err error
	switch ext(in) {
	case "docx":
		text, err = zipXMLText(in, "word/document.xml")
	case "odt", "odp":
		text, err = zipXMLText(in, "content.xml")
	case "pptx":
		text, err = pptxText(in)
	case "epub":
		text, err = epubText(in)
	case "fb2":
		var b []byte
		if b, err = os.ReadFile(in); err == nil {
			text = markupText(b, false)
		}
	case "html":
		var b []byte
		if b, err = os.ReadFile(in); err == nil {
			text = markupText(b, true)
		}
	case "rtf":
		var b []byte
		if b, err = os.ReadFile(in); err == nil {
			text = rtfText(b)
		}
	case "mobi", "azw3":
		text, err = mobiText(in)
	default: // doc, xls, ppt: legacy binary, best-effort string scan
		var b []byte
		if b, err = os.ReadFile(in); err == nil {
			text = binaryStrings(b)
		}
	}
	if err != nil {
		return err
	}
	return os.WriteFile(out, []byte(strings.TrimSpace(text)+"\n"), 0o644)
}

// Paragraph-like elements end with a newline, cells with a tab.
var (
	blockTags = map[string]bool{"p": true, "h": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
		"li": true, "tr": true, "div": true, "title": true, "section": true, "table-row": true, "list-item": true, "page": true, "v": true}
	cellTags = map[string]bool{"tc": true, "td": true, "th": true, "table-cell": true}
	skipTags = map[string]bool{"script": true, "style": true, "head": false, "binary": true, "instrText": true}
)

func markupText(b []byte, isHTML bool) string {
	d := xml.NewDecoder(bytes.NewReader(b))
	d.Strict = false
	if isHTML {
		d.AutoClose = xml.HTMLAutoClose
		d.Entity = xml.HTMLEntity
	}
	var sb strings.Builder
	skip := 0
	for {
		tok, err := d.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch n := t.Name.Local; {
			case skipTags[n]:
				skip++
			case n == "br" || n == "line-break":
				sb.WriteByte('\n')
			case n == "tab":
				sb.WriteByte('\t')
			case n == "s":
				sb.WriteByte(' ')
			}
		case xml.EndElement:
			switch n := t.Name.Local; {
			case skipTags[n]:
				skip--
			case blockTags[n]:
				sb.WriteByte('\n')
			case cellTags[n]:
				sb.WriteByte('\t')
			}
		case xml.CharData:
			if skip == 0 {
				s := string(t)
				if isHTML {
					s = strings.Join(strings.Fields(s), " ")
				}
				sb.WriteString(s)
			}
		}
	}
	return sb.String()
}

func zipRead(z *zip.ReadCloser, name string) ([]byte, error) {
	f, err := z.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func zipXMLText(p, member string) (string, error) {
	z, err := zip.OpenReader(p)
	if err != nil {
		return "", err
	}
	defer z.Close()
	b, err := zipRead(z, member)
	if err != nil {
		return "", err
	}
	return markupText(b, false), nil
}

func pptxText(p string) (string, error) {
	z, err := zip.OpenReader(p)
	if err != nil {
		return "", err
	}
	defer z.Close()
	var slides []string
	for _, f := range z.File {
		if strings.HasPrefix(f.Name, "ppt/slides/slide") && strings.HasSuffix(f.Name, ".xml") {
			slides = append(slides, f.Name)
		}
	}
	num := func(s string) int {
		n, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(s, "ppt/slides/slide"), ".xml"))
		return n
	}
	slices.SortFunc(slides, func(a, b string) int { return num(a) - num(b) })
	var sb strings.Builder
	for _, s := range slides {
		b, err := zipRead(z, s)
		if err != nil {
			return "", err
		}
		sb.WriteString(markupText(b, false) + "\n")
	}
	return sb.String(), nil
}

func epubText(p string) (string, error) {
	z, err := zip.OpenReader(p)
	if err != nil {
		return "", err
	}
	defer z.Close()
	b, err := zipRead(z, "META-INF/container.xml")
	if err != nil {
		return "", err
	}
	var c struct {
		Rootfiles []struct {
			Path string `xml:"full-path,attr"`
		} `xml:"rootfiles>rootfile"`
	}
	if err := xml.Unmarshal(b, &c); err != nil || len(c.Rootfiles) == 0 {
		return "", errors.New("epub: no rootfile")
	}
	opfPath := c.Rootfiles[0].Path
	if b, err = zipRead(z, opfPath); err != nil {
		return "", err
	}
	var opf struct {
		Items []struct {
			ID   string `xml:"id,attr"`
			Href string `xml:"href,attr"`
		} `xml:"manifest>item"`
		Spine []struct {
			IDRef string `xml:"idref,attr"`
		} `xml:"spine>itemref"`
	}
	if err := xml.Unmarshal(b, &opf); err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, s := range opf.Spine {
		for _, it := range opf.Items {
			if it.ID == s.IDRef {
				name := path.Join(path.Dir(opfPath), it.Href)
				if b, err := zipRead(z, name); err == nil {
					sb.WriteString(markupText(b, true) + "\n")
				}
			}
		}
	}
	return sb.String(), nil
}

func rtfText(b []byte) string {
	var sb strings.Builder
	type group struct{ skip bool }
	stack := []group{{}}
	skipWords := map[string]bool{"fonttbl": true, "colortbl": true, "stylesheet": true, "info": true, "pict": true, "header": true, "footer": true, "object": true}
	ucSkip := 0
	for i := 0; i < len(b); i++ {
		c := b[i]
		top := &stack[len(stack)-1]
		switch {
		case c == '{':
			stack = append(stack, group{top.skip})
		case c == '}':
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
		case c == '\\' && i+1 < len(b):
			n := b[i+1]
			switch {
			case n == '*':
				top.skip = true
				i++
			case n == '\'' && i+3 < len(b):
				v, _ := strconv.ParseUint(string(b[i+2:i+4]), 16, 8)
				if !top.skip {
					if ucSkip > 0 {
						ucSkip--
					} else {
						sb.WriteRune(rune(v)) // code page treated as Latin-1
					}
				}
				i += 3
			case n == '\\' || n == '{' || n == '}':
				if !top.skip {
					sb.WriteByte(n)
				}
				i++
			case (n >= 'a' && n <= 'z') || (n >= 'A' && n <= 'Z'):
				j := i + 1
				for j < len(b) && ((b[j] >= 'a' && b[j] <= 'z') || (b[j] >= 'A' && b[j] <= 'Z')) {
					j++
				}
				word := string(b[i+1 : j])
				k := j
				for k < len(b) && (b[k] == '-' || (b[k] >= '0' && b[k] <= '9')) {
					k++
				}
				arg, _ := strconv.Atoi(string(b[j:k]))
				if k < len(b) && b[k] == ' ' {
					k++
				}
				i = k - 1
				switch {
				case skipWords[word]:
					top.skip = true
				case top.skip:
				case word == "par" || word == "line" || word == "row":
					sb.WriteByte('\n')
				case word == "tab" || word == "cell":
					sb.WriteByte('\t')
				case word == "u":
					if arg < 0 {
						arg += 65536
					}
					sb.WriteRune(rune(arg))
					ucSkip = 1
				}
			default:
				i++
			}
		case c == '\r' || c == '\n':
		default:
			if !top.skip {
				if ucSkip > 0 {
					ucSkip--
				} else {
					sb.WriteByte(c)
				}
			}
		}
	}
	return sb.String()
}

// binaryStrings pulls readable UTF-16LE and 8-bit runs out of legacy Office files.
func binaryStrings(b []byte) string {
	var sb strings.Builder
	var run []uint16
	flush := func() {
		if len(run) >= 6 {
			sb.WriteString(string(utf16.Decode(run)) + "\n")
		}
		run = run[:0]
	}
	for i := 0; i+1 < len(b); i += 2 {
		r := rune(binary.LittleEndian.Uint16(b[i:]))
		if r == '\r' || (r >= 0x20 && r < 0xd800 && r != 0x7f && r != 0xfffd) {
			run = append(run, uint16(r))
		} else {
			flush()
		}
	}
	flush()
	if sb.Len() == 0 { // 8-bit text
		var cur []byte
		for _, c := range append(b, 0) {
			if c >= 0x20 && c < 0x7f || c == '\r' || c == '\t' {
				cur = append(cur, c)
				continue
			}
			if len(cur) >= 8 {
				sb.Write(append(cur, '\n'))
			}
			cur = cur[:0]
		}
	}
	return strings.ReplaceAll(sb.String(), "\r", "\n")
}

// mobiText reads MOBI/AZW3 text records (PalmDOC compression or none).
func mobiText(p string) (string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	if len(b) < 78 {
		return "", errors.New("mobi: too short")
	}
	be := binary.BigEndian
	nrec := int(be.Uint16(b[76:]))
	off := func(i int) int { return int(be.Uint32(b[78+i*8:])) }
	if nrec < 2 || len(b) < 78+nrec*8 {
		return "", errors.New("mobi: bad record table")
	}
	r0 := b[off(0):off(1)]
	if len(r0) < 16 {
		return "", errors.New("mobi: bad header")
	}
	comp, textRecs := be.Uint16(r0[0:]), int(be.Uint16(r0[8:]))
	if be.Uint16(r0[12:]) != 0 {
		return "", errors.New("mobi: DRM-protected")
	}
	if comp != 1 && comp != 2 {
		return "", fmt.Errorf("mobi: unsupported compression %d", comp)
	}
	var flags uint16
	cp1252 := true
	if len(r0) >= 0xF4 && string(r0[16:20]) == "MOBI" {
		cp1252 = be.Uint32(r0[28:]) != 65001
		if be.Uint32(r0[20:]) >= 0xE4 {
			flags = be.Uint16(r0[0xF2:])
		}
	}
	var out []byte
	for i := 1; i <= textRecs && i < nrec; i++ {
		end := len(b)
		if i+1 < nrec {
			end = off(i + 1)
		}
		rec := b[off(i):end]
		rec = rec[:len(rec)-trailing(rec, flags)]
		if comp == 2 {
			rec = palmdoc(rec)
		}
		out = append(out, rec...)
	}
	if cp1252 {
		r := []rune{}
		for _, c := range out {
			r = append(r, rune(c))
		}
		out = []byte(string(r))
	}
	return markupText(out, true), nil
}

func trailing(rec []byte, flags uint16) int {
	n := 0
	for bit := 15; bit > 0; bit-- {
		if flags&(1<<bit) == 0 {
			continue
		}
		// Backward-encoded variable-width size at the end.
		size, shift := 0, 0
		for k := len(rec) - n - 1; k >= 0 && shift < 28; k-- {
			size |= int(rec[k]&0x7f) << shift
			shift += 7
			if rec[k]&0x80 != 0 {
				break
			}
		}
		n += size
	}
	if flags&1 != 0 && len(rec) > n {
		n += int(rec[len(rec)-n-1]&3) + 1
	}
	return min(n, len(rec))
}

func palmdoc(in []byte) []byte {
	var out []byte
	for i := 0; i < len(in); i++ {
		c := in[i]
		switch {
		case c >= 1 && c <= 8:
			end := min(i+1+int(c), len(in))
			out = append(out, in[i+1:end]...)
			i = end - 1
		case c < 0x80:
			out = append(out, c)
		case c >= 0xC0:
			out = append(out, ' ', c^0x80)
		default:
			if i+1 >= len(in) {
				return out
			}
			v := int(c)<<8 | int(in[i+1])
			i++
			dist, n := (v>>3)&0x7ff, (v&7)+3
			if dist == 0 || dist > len(out) {
				continue
			}
			for k := 0; k < n; k++ {
				out = append(out, out[len(out)-dist])
			}
		}
	}
	return out
}

// --- spreadsheets ---

func readSheetCSV(ctx context.Context, in, out string, o opts) error {
	var rows [][]string
	var err error
	if ext(in) == "xlsx" {
		rows, err = xlsxRows(in)
	} else {
		rows, err = odsRows(in)
	}
	if err != nil {
		return err
	}
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	w.WriteAll(rows)
	if err := w.Error(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func xlsxRows(p string) ([][]string, error) {
	z, err := zip.OpenReader(p)
	if err != nil {
		return nil, err
	}
	defer z.Close()
	var shared []string
	if b, err := zipRead(z, "xl/sharedStrings.xml"); err == nil {
		var sst struct {
			SI []struct {
				T []string `xml:"t"`
				R []struct {
					T string `xml:"t"`
				} `xml:"r"`
			} `xml:"si"`
		}
		xml.Unmarshal(b, &sst)
		for _, si := range sst.SI {
			s := strings.Join(si.T, "")
			for _, r := range si.R {
				s += r.T
			}
			shared = append(shared, s)
		}
	}
	b, err := zipRead(z, "xl/worksheets/sheet1.xml")
	if err != nil {
		return nil, err
	}
	var ws struct {
		Rows []struct {
			C []struct {
				R  string `xml:"r,attr"`
				T  string `xml:"t,attr"`
				V  string `xml:"v"`
				IS string `xml:"is>t"`
			} `xml:"c"`
		} `xml:"sheetData>row"`
	}
	if err := xml.Unmarshal(b, &ws); err != nil {
		return nil, err
	}
	var rows [][]string
	for _, r := range ws.Rows {
		var row []string
		for _, c := range r.C {
			col := 0
			for _, ch := range c.R {
				if ch >= 'A' && ch <= 'Z' {
					col = col*26 + int(ch-'A'+1)
				}
			}
			for col > 0 && len(row) < col-1 {
				row = append(row, "")
			}
			v := c.V
			switch c.T {
			case "s":
				if i, err := strconv.Atoi(v); err == nil && i < len(shared) {
					v = shared[i]
				}
			case "inlineStr":
				v = c.IS
			}
			row = append(row, v)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func odsRows(p string) ([][]string, error) {
	z, err := zip.OpenReader(p)
	if err != nil {
		return nil, err
	}
	defer z.Close()
	b, err := zipRead(z, "content.xml")
	if err != nil {
		return nil, err
	}
	d := xml.NewDecoder(bytes.NewReader(b))
	attr := func(e xml.StartElement, name string) int {
		for _, a := range e.Attr {
			if a.Name.Local == name {
				n, _ := strconv.Atoi(a.Value)
				return n
			}
		}
		return 1
	}
	var rows [][]string
	var row []string
	var cell strings.Builder
	emptyCells, emptyRows, rowRep, cellRep, tables := 0, 0, 1, 1, 0
	inCell := false
	for {
		tok, err := d.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "table":
				tables++
			case "table-row":
				row, emptyCells, rowRep = nil, 0, attr(t, "number-rows-repeated")
			case "table-cell", "covered-table-cell":
				inCell, cellRep = true, attr(t, "number-columns-repeated")
				cell.Reset()
			case "p":
				if cell.Len() > 0 {
					cell.WriteByte('\n')
				}
			}
		case xml.CharData:
			if inCell {
				cell.Write(t)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "table":
				if tables == 1 {
					return rows, nil // first sheet only
				}
			case "table-cell", "covered-table-cell":
				inCell = false
				if cell.Len() == 0 {
					emptyCells += cellRep // trailing empties are repeated thousands of times; only add when followed by data
					continue
				}
				for ; emptyCells > 0; emptyCells-- {
					row = append(row, "")
				}
				for range cellRep {
					row = append(row, cell.String())
				}
			case "table-row":
				if len(row) == 0 {
					emptyRows += rowRep
					continue
				}
				for ; emptyRows > 0; emptyRows-- {
					rows = append(rows, nil)
				}
				for range min(rowRep, 1000) {
					rows = append(rows, row)
				}
			}
		}
	}
	return rows, nil
}

// --- writers ---

func readLines(p string) ([]string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(b) {
		r := []rune{}
		for _, c := range b {
			r = append(r, rune(c))
		}
		b = []byte(string(r))
	}
	return strings.Split(strings.TrimRight(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n"), "\n"), nil
}

// esc escapes text for XML and drops control characters XML forbids
// (tesseract and pdftotext emit \f page breaks).
func esc(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' {
			return -1
		}
		return r
	}, s)
	return html.EscapeString(s)
}

// writeZip writes files in order; "mimetype" is stored uncompressed as ODF/EPUB require.
func writeZip(out string, files ...[2]string) error {
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	z := zip.NewWriter(f)
	for _, kv := range files {
		h := &zip.FileHeader{Name: kv[0], Method: zip.Deflate, Modified: time.Now()}
		var w io.Writer
		var err error
		if kv[0] == "mimetype" {
			// Raw stored entry: no data descriptor, which LibreOffice rejects here.
			h.Method, h.CRC32 = zip.Store, crc32.ChecksumIEEE([]byte(kv[1]))
			h.CompressedSize64, h.UncompressedSize64 = uint64(len(kv[1])), uint64(len(kv[1]))
			w, err = z.CreateRaw(h)
		} else {
			w, err = z.CreateHeader(h)
		}
		if err != nil {
			f.Close()
			return err
		}
		io.WriteString(w, kv[1])
	}
	if err := z.Close(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

const odfManifest = `<?xml version="1.0" encoding="UTF-8"?>
<manifest:manifest xmlns:manifest="urn:oasis:names:tc:opendocument:xmlns:manifest:1.0" manifest:version="1.2">
<manifest:file-entry manifest:full-path="/" manifest:media-type="%s"/>
<manifest:file-entry manifest:full-path="content.xml" manifest:media-type="text/xml"/>
</manifest:manifest>`

const odfHead = `<?xml version="1.0" encoding="UTF-8"?>
<office:document-content xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0" xmlns:table="urn:oasis:names:tc:opendocument:xmlns:table:1.0" xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0" xmlns:svg="urn:oasis:names:tc:opendocument:xmlns:svg-compatible:1.0" office:version="1.2"><office:body>`

func writeDoc(ctx context.Context, in, out string, o opts) error {
	lines, err := readLines(in)
	if err != nil {
		return err
	}
	title := stem(in)
	var b strings.Builder
	switch ext(out) {
	case "docx":
		for _, l := range lines {
			fmt.Fprintf(&b, `<w:p><w:r><w:t xml:space="preserve">%s</w:t></w:r></w:p>`, esc(l))
		}
		return writeZip(out,
			[2]string{"[Content_Types].xml", `<?xml version="1.0" encoding="UTF-8"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`},
			[2]string{"_rels/.rels", `<?xml version="1.0" encoding="UTF-8"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>`},
			[2]string{"word/document.xml", `<?xml version="1.0" encoding="UTF-8"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>` + b.String() + `</w:body></w:document>`})
	case "odt":
		for _, l := range lines {
			fmt.Fprintf(&b, "<text:p>%s</text:p>", esc(l))
		}
		mt := "application/vnd.oasis.opendocument.text"
		return writeZip(out, [2]string{"mimetype", mt}, [2]string{"META-INF/manifest.xml", fmt.Sprintf(odfManifest, mt)},
			[2]string{"content.xml", odfHead + "<office:text>" + b.String() + "</office:text></office:body></office:document-content>"})
	case "odp":
		// One slide per blank-line separated block.
		for _, block := range strings.Split(strings.Join(lines, "\n"), "\n\n") {
			b.WriteString(`<draw:page draw:master-page-name="Default"><draw:frame svg:x="1cm" svg:y="1cm" svg:width="26cm" svg:height="17cm"><draw:text-box>`)
			for _, l := range strings.Split(block, "\n") {
				fmt.Fprintf(&b, "<text:p>%s</text:p>", esc(l))
			}
			b.WriteString(`</draw:text-box></draw:frame></draw:page>`)
		}
		mt := "application/vnd.oasis.opendocument.presentation"
		return writeZip(out, [2]string{"mimetype", mt}, [2]string{"META-INF/manifest.xml", fmt.Sprintf(odfManifest, mt)},
			[2]string{"content.xml", odfHead + "<office:presentation>" + b.String() + "</office:presentation></office:body></office:document-content>"})
	case "rtf":
		b.WriteString(`{\rtf1\ansi\deff0{\fonttbl{\f0 Liberation Sans;}}\f0\fs22 `)
		for _, l := range lines {
			for _, r := range l {
				switch {
				case r == '\\' || r == '{' || r == '}':
					b.WriteString(`\` + string(r))
				case r > 127:
					fmt.Fprintf(&b, `\u%d?`, int16(r))
				default:
					b.WriteRune(r)
				}
			}
			b.WriteString("\\par\n")
		}
		b.WriteString("}")
		return os.WriteFile(out, []byte(b.String()), 0o644)
	case "html":
		fmt.Fprintf(&b, "<!doctype html>\n<meta charset=\"utf-8\">\n<title>%s</title>\n", esc(title))
		for _, l := range lines {
			fmt.Fprintf(&b, "<p>%s</p>\n", esc(l))
		}
		return os.WriteFile(out, []byte(b.String()), 0o644)
	case "fb2":
		for _, l := range lines {
			fmt.Fprintf(&b, "<p>%s</p>", esc(l))
		}
		return os.WriteFile(out, []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><FictionBook xmlns="http://www.gribuser.ru/xml/fictionbook/2.0"><description><title-info><book-title>%s</book-title><lang>und</lang></title-info></description><body><section>%s</section></body></FictionBook>`, esc(title), b.String())), 0o644)
	case "epub":
		for _, l := range lines {
			fmt.Fprintf(&b, "<p>%s</p>\n", esc(l))
		}
		xhtml := func(body string) string {
			return `<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE html><html xmlns="http://www.w3.org/1999/xhtml" xmlns:epub="http://www.idpf.org/2007/ops"><head><title>` + esc(title) + `</title></head><body>` + body + `</body></html>`
		}
		return writeZip(out,
			[2]string{"mimetype", "application/epub+zip"},
			[2]string{"META-INF/container.xml", `<?xml version="1.0"?><container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container"><rootfiles><rootfile full-path="content.opf" media-type="application/oebps-package+xml"/></rootfiles></container>`},
			[2]string{"content.opf", `<?xml version="1.0" encoding="UTF-8"?><package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="id"><metadata xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:identifier id="id">pifile-` + esc(title) + `</dc:identifier><dc:title>` + esc(title) + `</dc:title><dc:language>und</dc:language><meta property="dcterms:modified">2000-01-01T00:00:00Z</meta></metadata><manifest><item id="nav" href="nav.xhtml" media-type="application/xhtml+xml" properties="nav"/><item id="text" href="text.xhtml" media-type="application/xhtml+xml"/></manifest><spine><itemref idref="text"/></spine></package>`},
			[2]string{"nav.xhtml", xhtml(`<nav epub:type="toc"><ol><li><a href="text.xhtml">` + esc(title) + `</a></li></ol></nav>`)},
			[2]string{"text.xhtml", xhtml(b.String())})
	}
	return fmt.Errorf("cannot write %s", ext(out))
}

func writeSheet(ctx context.Context, in, out string, o opts) error {
	f, err := os.Open(in)
	if err != nil {
		return err
	}
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	rows, err := r.ReadAll()
	f.Close()
	if err != nil {
		return err
	}
	var b strings.Builder
	isNum := func(s string) bool { _, err := strconv.ParseFloat(s, 64); return err == nil && s != "" }
	if ext(out) == "ods" {
		for _, row := range rows {
			b.WriteString("<table:table-row>")
			for _, c := range row {
				if isNum(c) {
					fmt.Fprintf(&b, `<table:table-cell office:value-type="float" office:value="%s"><text:p>%s</text:p></table:table-cell>`, c, c)
				} else {
					fmt.Fprintf(&b, `<table:table-cell office:value-type="string"><text:p>%s</text:p></table:table-cell>`, esc(c))
				}
			}
			b.WriteString("</table:table-row>")
		}
		mt := "application/vnd.oasis.opendocument.spreadsheet"
		return writeZip(out, [2]string{"mimetype", mt}, [2]string{"META-INF/manifest.xml", fmt.Sprintf(odfManifest, mt)},
			[2]string{"content.xml", odfHead + `<office:spreadsheet><table:table table:name="Sheet1">` + b.String() + "</table:table></office:spreadsheet></office:body></office:document-content>"})
	}
	for i, row := range rows {
		fmt.Fprintf(&b, `<row r="%d">`, i+1)
		for j, c := range row {
			ref := colName(j) + strconv.Itoa(i+1)
			if isNum(c) {
				fmt.Fprintf(&b, `<c r="%s"><v>%s</v></c>`, ref, c)
			} else {
				fmt.Fprintf(&b, `<c r="%s" t="inlineStr"><is><t xml:space="preserve">%s</t></is></c>`, ref, esc(c))
			}
		}
		b.WriteString("</row>")
	}
	return writeZip(out,
		[2]string{"[Content_Types].xml", `<?xml version="1.0" encoding="UTF-8"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/><Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/></Types>`},
		[2]string{"_rels/.rels", `<?xml version="1.0" encoding="UTF-8"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>`},
		[2]string{"xl/workbook.xml", `<?xml version="1.0" encoding="UTF-8"?><workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="Sheet1" sheetId="1" r:id="rId1"/></sheets></workbook>`},
		[2]string{"xl/_rels/workbook.xml.rels", `<?xml version="1.0" encoding="UTF-8"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/></Relationships>`},
		[2]string{"xl/worksheets/sheet1.xml", `<?xml version="1.0" encoding="UTF-8"?><worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>` + b.String() + `</sheetData></worksheet>`})
}

func colName(i int) string {
	s := ""
	for i++; i > 0; i = (i - 1) / 26 {
		s = string(rune('A'+(i-1)%26)) + s
	}
	return s
}
