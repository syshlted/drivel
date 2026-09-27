// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

//go:build ignore

// Command gen-logo-assets derives everything else in contrib/ from one file:
// the official artwork as delivered.
//
//	go run contrib/gen-logo-assets.go              # the current version
//	go run contrib/gen-logo-assets.go -version 1.2 # after the next redraw
//
// It produces the portrait crop, the braille rendition of that crop, a set of
// 16:9 scaled copies, and the two-colour backdrop the website paints behind its
// pages. Before this existed every one of those was a hand-made file with no
// record of how it had been made, which is why the 1.0 set could be reproduced
// only by measuring it -- see "The recovered pipeline" below.
//
// The braille step shells out to ascii-image-converter, which is the program
// that made the original:
//
//	go install github.com/TheZoraiz/ascii-image-converter@latest
//
// Reimplementing it here would have been perhaps thirty lines -- threshold,
// then pack dots -- and the wrong thirty lines. The committed 1.0 art is the
// reference for what this art *looks like*, and matching it means matching
// that program's choices, not making my own and calling them the same.
//
// # The recovered pipeline
//
// Nothing about the 1.0 files was written down, so each parameter below was
// recovered by reproducing them and is asserted by -verify:
//
//   - The crop is exactly (454,12)-(929,655) of the 1408x768 master, found by
//     template match at RMS 0.61/255 -- near enough to prove it was a plain 1:1
//     crop with no scaling. It is carried across versions as *proportions*,
//     which is what makes "the same section" mean anything when the master
//     arrives at 7680x4189 instead.
//   - The braille is `-b -W 70 --threshold 99`. 70 columns was obvious from the
//     file being 70 wide; the threshold was not, and a sweep of 0..255 has
//     exactly one value that reproduces the committed file byte for byte. The
//     default is 128, so this was deliberate, and guessing it would have given
//     art that was merely similar.
package main

import (
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/jpeg" // the 1.0 master, for -verify
	"image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// The crop, as fractions of the master. See "The recovered pipeline" above.
const (
	cropX = 454.0 / 1408.0
	cropY = 12.0 / 768.0
	cropW = 475.0 / 1408.0
	cropH = 643.0 / 768.0
)

// scaledSizes are the 16:9 copies. The masters are 1.833:1 -- both 1.0 and 1.1
// arrived that way -- so none of these is a plain scale: the artwork is fitted
// to the width and the leftover height is padded in the artwork's own
// background colour, which is seamless because the drawing does not reach its
// own edges. Cropping to fill instead would take 3% off each side, and the
// outermost heads live there.
var scaledSizes = []struct {
	w, h int
	name string
}{
	{3840, 2160, "4K UHD"},
	{2560, 1440, "QHD / 1440p"},
	{1920, 1080, "FHD / 1080p"},
	{1280, 720, "HD / 720p"},
	{960, 540, ""},
	{640, 360, ""},
	{320, 180, ""},
}

func main() {
	version := flag.String("version", "1.1", "artwork version: reads contrib/Drivel_Logo_<V>-Official.png")
	cols := flag.Int("cols", 70, "braille width in characters")
	threshold := flag.Int("threshold", 99, "braille threshold passed to ascii-image-converter")
	backdropW := flag.Int("backdrop-width", 7680, "width of the generated two-colour backdrop")
	verify := flag.Bool("verify", false, "reproduce the 1.0 artefacts and check them against the committed files")
	flag.Parse()

	root, err := repoRoot()
	if err != nil {
		die(err)
	}
	if *verify {
		if err := verify10(root, *cols); err != nil {
			die(err)
		}
		return
	}

	master := filepath.Join(root, "contrib", fmt.Sprintf("Drivel_Logo_%s-Official.png", *version))
	src, err := loadRGBA(master)
	if err != nil {
		die(err)
	}
	b := src.Bounds()
	fmt.Printf("master  %s  %dx%d\n", filepath.Base(master), b.Dx(), b.Dy())

	// 1. The crop.
	crop := cropRect(b)
	cropped := sub(src, crop)
	cropPath := filepath.Join(root, "contrib", fmt.Sprintf("Drivel_Logo_%s-Cropped_1.png", *version))
	if err := writePNG(cropPath, cropped); err != nil {
		die(err)
	}
	report(root, cropPath, crop.Dx(), crop.Dy())

	// 2. The braille rendition of it.
	txtPath := filepath.Join(root, "contrib", fmt.Sprintf("Drivel_Logo_%s-Cropped_1.txt", *version))
	if err := braille(cropPath, txtPath, *cols, *threshold); err != nil {
		die(err)
	}
	rows, err := brailleRows(txtPath)
	if err != nil {
		die(err)
	}
	fmt.Printf("  %-52s %d cols x %d rows\n", rel(root, txtPath), len(rows[0]), len(rows))

	// 3. The 16:9 copies, padded in the artwork's own background colour.
	ground := borderColour(src)
	fmt.Printf("  background sampled as #%02x%02x%02x\n", ground.R, ground.G, ground.B)
	for _, s := range scaledSizes {
		out := letterbox(src, s.w, s.h, ground)
		p := filepath.Join(root, "contrib", "scaled",
			fmt.Sprintf("Drivel_Logo_%s-%dx%d.png", *version, s.w, s.h))
		if err := writePNG(p, out); err != nil {
			die(err)
		}
		report(root, p, s.w, s.h)
		if s.name != "" {
			fmt.Printf("  %-52s %s\n", "", s.name)
		}
	}

	// 4. The backdrop, at the crop's aspect ratio rather than the braille
	//    grid's -- they differ by under a percent, and the instruction was the
	//    crop's.
	//
	//    It lands here beside the rest of the derived artwork rather than in
	//    site/, and the site mounts this one file. Hugo can mount a single path
	//    as an asset, so the alternative -- writing a copy under site/assets --
	//    would put the same image in the tree twice, which is the arrangement
	//    the manual pages already refuse for the same reason.
	bw := *backdropW
	bh := int(math.Round(float64(bw) * float64(crop.Dy()) / float64(crop.Dx())))
	back := brailleBitmap(rows, bw, bh)
	backPath := filepath.Join(root, "contrib", fmt.Sprintf("Drivel_Logo_%s-Braille.png", *version))
	if err := writePNG(backPath, back); err != nil {
		die(err)
	}
	report(root, backPath, bw, bh)
}

// cropRect turns the stored proportions into pixels for this master.
func cropRect(b image.Rectangle) image.Rectangle {
	x := b.Min.X + int(math.Round(cropX*float64(b.Dx())))
	y := b.Min.Y + int(math.Round(cropY*float64(b.Dy())))
	w := int(math.Round(cropW * float64(b.Dx())))
	h := int(math.Round(cropH * float64(b.Dy())))
	return image.Rect(x, y, x+w, y+h).Intersect(b)
}

func sub(src *image.NRGBA, r image.Rectangle) *image.NRGBA {
	out := image.NewNRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	draw.Draw(out, out.Bounds(), src, r.Min, draw.Src)
	return out
}

// braille runs ascii-image-converter over the crop and lands its output at path.
//
// The tool names its own output file and writes it into a directory, so it runs
// in a scratch directory and the single file it leaves there is moved into
// place. A trailing newline is added: the tool omits one, and the committed 1.0
// file has it, so this keeps the artefact identical to its predecessor rather
// than merely equivalent.
func braille(imgPath, path string, cols, threshold int) error {
	exe, err := exec.LookPath("ascii-image-converter")
	if err != nil {
		return fmt.Errorf("ascii-image-converter is not on PATH -- install it with:\n" +
			"\tgo install github.com/TheZoraiz/ascii-image-converter@latest")
	}
	tmp, err := os.MkdirTemp("", "drivel-braille-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp) //nolint:errcheck // scratch

	cmd := exec.Command(exe, "--braille",
		"--width", fmt.Sprint(cols),
		"--threshold", fmt.Sprint(threshold),
		"--save-txt", tmp, imgPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ascii-image-converter: %w\n%s", err, out)
	}
	found, err := filepath.Glob(filepath.Join(tmp, "*.txt"))
	if err != nil || len(found) != 1 {
		return fmt.Errorf("expected one .txt from ascii-image-converter, got %v", found)
	}
	body, err := os.ReadFile(found[0])
	if err != nil {
		return err
	}
	if !strings.HasSuffix(string(body), "\n") {
		body = append(body, '\n')
	}
	return os.WriteFile(path, body, 0o644)
}

func brailleRows(path string) ([][]rune, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rows [][]rune
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		r := []rune(line)
		for _, c := range r {
			if c < 0x2800 || c > 0x28ff {
				return nil, fmt.Errorf("%s: line %d holds %q, which is not a braille pattern", path, i+1, c)
			}
		}
		rows = append(rows, r)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%s: no braille rows", path)
	}
	for i, r := range rows {
		if len(r) != len(rows[0]) {
			return nil, fmt.Errorf("%s: line %d is %d cells, line 1 is %d", path, i+1, len(r), len(rows[0]))
		}
	}
	return rows, nil
}

// brailleBitmap turns the braille art into a two-colour image of the given size.
//
// Every U+28xx codepoint *is* a 2x4 grid of dots, so the character grid carries
// a bitmap exactly -- no threshold here, nothing to choose. Which way round it
// goes was measured rather than guessed: correlated against the source image a
// set dot sits at mean luminance 162 and a clear one at 64, so the dots are the
// white paper the animal was drawn on and **the drawing is the dots that are
// clear**. Painting the dots gives a photographic negative.
//
// The palette is transparent then opaque black, which makes this a 1-bit PNG
// and, more usefully, a stencil: the site paints it through a CSS mask so the
// ink can follow the theme, which an image with a painted ground could not do.
// Scaling is nearest-neighbour because anything else invents a third colour.
func brailleBitmap(rows [][]rune, w, h int) *image.Paletted {
	gw, gh := len(rows[0])*2, len(rows)*4
	// Bit i of U+28xx lights the dot at this offset in the cell. The order is
	// braille's (dots 1-6, then the two eight-dot extras), not raster order.
	offsets := [8][2]int{{0, 0}, {0, 1}, {0, 2}, {1, 0}, {1, 1}, {1, 2}, {0, 3}, {1, 3}}
	dot := make([]bool, gw*gh)
	for cy, row := range rows {
		for cx, r := range row {
			for i, d := range offsets {
				if (r-0x2800)&(1<<i) != 0 {
					dot[(cy*4+d[1])*gw+cx*2+d[0]] = true
				}
			}
		}
	}

	img := image.NewPaletted(image.Rect(0, 0, w, h),
		color.Palette{color.NRGBA{}, color.NRGBA{A: 0xff}})
	for y := 0; y < h; y++ {
		sy := y * gh / h
		for x := 0; x < w; x++ {
			if !dot[sy*gw+x*gw/w] {
				img.SetColorIndex(x, y, 1)
			}
		}
	}
	return img
}

// letterbox fits the artwork inside w by h and pads the remainder with ground.
func letterbox(src *image.NRGBA, w, h int, ground color.NRGBA) *image.NRGBA {
	b := src.Bounds()
	scale := math.Min(float64(w)/float64(b.Dx()), float64(h)/float64(b.Dy()))
	iw := int(math.Round(float64(b.Dx()) * scale))
	ih := int(math.Round(float64(b.Dy()) * scale))
	if iw > w {
		iw = w
	}
	if ih > h {
		ih = h
	}
	art := resize(src, iw, ih)

	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.Draw(out, out.Bounds(), &image.Uniform{ground}, image.Point{}, draw.Src)
	at := image.Pt((w-iw)/2, (h-ih)/2)
	draw.Draw(out, image.Rectangle{at, at.Add(image.Pt(iw, ih))}, art, image.Point{}, draw.Src)
	return out
}

// borderColour is the median colour of the master's outermost ring.
//
// The median rather than a corner: one stray pixel in a corner would tint every
// letterbox bar, and a median over a few thousand samples cannot be moved by
// one. It is sampled rather than hard-coded so that a redraw whose paper is a
// different white still pads seamlessly.
func borderColour(src *image.NRGBA) color.NRGBA {
	b := src.Bounds()
	var rs, gs, bs []int
	add := func(x, y int) {
		c := src.NRGBAAt(x, y)
		rs = append(rs, int(c.R))
		gs = append(gs, int(c.G))
		bs = append(bs, int(c.B))
	}
	for x := b.Min.X; x < b.Max.X; x++ {
		add(x, b.Min.Y)
		add(x, b.Max.Y-1)
	}
	for y := b.Min.Y; y < b.Max.Y; y++ {
		add(b.Min.X, y)
		add(b.Max.X-1, y)
	}
	med := func(v []int) uint8 { sort.Ints(v); return uint8(v[len(v)/2]) }
	return color.NRGBA{R: med(rs), G: med(gs), B: med(bs), A: 0xff}
}

// resize scales with a separable triangle filter in premultiplied alpha.
//
// Separable because the alternative is not: a 7680-wide master downsampled to
// seven sizes with a two-dimensional kernel is hundreds of millions of samples
// per output, and this is two passes of a few million.
func resize(src *image.NRGBA, w, h int) *image.NRGBA {
	b := src.Bounds()
	mid := make([]float32, w*b.Dy()*4)
	scaleX := float64(b.Dx()) / float64(w)
	rx := math.Max(scaleX, 1)
	for y := 0; y < b.Dy(); y++ {
		row := src.Pix[y*src.Stride:]
		for x := 0; x < w; x++ {
			cx := (float64(x)+0.5)*scaleX - 0.5
			var acc [4]float64
			var sum float64
			for ix := int(math.Floor(cx - rx)); ix <= int(math.Ceil(cx+rx)); ix++ {
				wt := 1 - math.Abs(float64(ix)-cx)/rx
				if wt <= 0 {
					continue
				}
				i := clamp(ix, 0, b.Dx()-1) * 4
				a := float64(row[i+3]) / 255
				acc[0] += wt * float64(row[i]) * a
				acc[1] += wt * float64(row[i+1]) * a
				acc[2] += wt * float64(row[i+2]) * a
				acc[3] += wt * float64(row[i+3])
				sum += wt
			}
			o := (y*w + x) * 4
			for c := 0; c < 4; c++ {
				mid[o+c] = float32(acc[c] / sum)
			}
		}
	}

	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	scaleY := float64(b.Dy()) / float64(h)
	ry := math.Max(scaleY, 1)
	for y := 0; y < h; y++ {
		cy := (float64(y)+0.5)*scaleY - 0.5
		for x := 0; x < w; x++ {
			var acc [4]float64
			var sum float64
			for iy := int(math.Floor(cy - ry)); iy <= int(math.Ceil(cy+ry)); iy++ {
				wt := 1 - math.Abs(float64(iy)-cy)/ry
				if wt <= 0 {
					continue
				}
				o := (clamp(iy, 0, b.Dy()-1)*w + x) * 4
				for c := 0; c < 4; c++ {
					acc[c] += wt * float64(mid[o+c])
				}
				sum += wt
			}
			a := acc[3] / sum
			out := color.NRGBA{A: uint8(clampF(math.Round(a), 0, 255))}
			if a > 0 {
				out.R = uint8(clampF(math.Round(acc[0]/sum/(a/255)), 0, 255))
				out.G = uint8(clampF(math.Round(acc[1]/sum/(a/255)), 0, 255))
				out.B = uint8(clampF(math.Round(acc[2]/sum/(a/255)), 0, 255))
			}
			dst.SetNRGBA(x, y, out)
		}
	}
	return dst
}

// verify10 reproduces the 1.0 artefacts from the 1.0 master and compares them
// against the files that were committed by hand, which is the only evidence
// that the constants at the top of this file are the ones that were used.
//
// It is a flag rather than a test because it needs both the 1.0 master and an
// installed ascii-image-converter, and `go test ./...` may have neither. Run it
// when changing any of those constants.
func verify10(root string, cols int) error {
	master := filepath.Join(root, "contrib", "Drivel_Logo_1.0.jpg")
	// All three, up front, with one message. The 1.0 artwork was removed from
	// the tree once 1.1 replaced it, so the ordinary outcome of running this is
	// now that it cannot run -- which should say so plainly and name the way
	// back, rather than failing halfway through on whichever file it reached.
	for _, p := range []string{master,
		filepath.Join(root, "contrib", "Drivel_Logo_1.0-Cropped_1.jpg"),
		filepath.Join(root, "contrib", "Drivel_Logo_1.0-cropped_1.txt"),
	} {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("%s is not in the tree, so this check cannot run.\n"+
				"The 1.0 artwork was removed when 1.1 replaced it; recover it with\n"+
				"\tgit checkout <commit-before-its-removal> -- contrib/Drivel_Logo_1.0*",
				rel(root, p))
		}
	}
	f, err := os.Open(master)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // read-only
	src, _, err := image.Decode(f)
	if err != nil {
		return err
	}
	got := cropRect(src.Bounds())
	want := image.Rect(454, 12, 929, 655)
	if got != want {
		return fmt.Errorf("crop proportions give %v, but the 1.0 crop was %v", got, want)
	}
	fmt.Printf("crop      %v  matches the committed 1.0 crop\n", got)

	tmp, err := os.MkdirTemp("", "drivel-verify-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp) //nolint:errcheck // scratch
	out := filepath.Join(tmp, "art.txt")
	if err := braille(filepath.Join(root, "contrib", "Drivel_Logo_1.0-Cropped_1.jpg"), out, cols, 99); err != nil {
		return err
	}
	a, err := os.ReadFile(out)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(filepath.Join(root, "contrib", "Drivel_Logo_1.0-cropped_1.txt"))
	if err != nil {
		return err
	}
	if string(a) != string(b) {
		return fmt.Errorf("braille output differs from the committed 1.0 art (%d vs %d bytes)", len(a), len(b))
	}
	fmt.Printf("braille   %d bytes, identical to the committed 1.0 art\n", len(a))
	return nil
}

func loadRGBA(path string) (*image.NRGBA, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only
	src, err := png.Decode(f)
	if err != nil {
		return nil, err
	}
	// Flattened to 8 bits once, so the resize loops index a byte slice instead
	// of going through image.Image.At for every sample. The masters arrive as
	// 16-bit RGBA, which is four times the memory for detail no output keeps.
	if n, ok := src.(*image.NRGBA); ok {
		return n, nil
	}
	out := image.NewNRGBA(src.Bounds())
	draw.Draw(out, out.Bounds(), src, src.Bounds().Min, draw.Src)
	return out, nil
}

func writePNG(path string, img image.Image) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(f, img); err != nil {
		f.Close() //nolint:errcheck // the encode error is the one that matters
		return err
	}
	return f.Close()
}

func report(root, path string, w, h int) {
	st, err := os.Stat(path)
	size := int64(0)
	if err == nil {
		size = st.Size()
	}
	fmt.Printf("  %-52s %5dx%-5d %7.1f KB\n", rel(root, path), w, h, float64(size)/1024)
}

func rel(root, path string) string {
	if r, err := filepath.Rel(root, path); err == nil {
		return r
	}
	return path
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above %q: run this from inside the repository", dir)
		}
		dir = parent
	}
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clampF(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

func die(err error) {
	fmt.Fprintln(os.Stderr, "gen-logo-assets:", err)
	os.Exit(1)
}
