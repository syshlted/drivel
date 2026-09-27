// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// SPDX-License-Identifier: MPL-2.0

//go:build ignore

// Command gen-site-images cuts the website's icons out of the artwork.
//
//	go run contrib/gen-site-images.go
//
// It is deliberately not wired into `make`. Nothing about a build depends on
// these files -- they change when the artwork does, which is roughly never --
// and a drift check like `make proto-check` would mean committing to
// byte-identical PNG output across Go releases, which is a promise the image
// encoder has never made.
//
// The page backdrop is not here: it is derived from the artwork master rather
// than from the site's cut-out copy, so contrib/gen-logo-assets.go owns it.
//
// The one thing this does *not* do is cut the backdrop out of the artwork. That
// is a judgement call with a lot of exceptions in it (see docs/project/
// mascot.md), so site/assets/images/drivel-logo.png is a committed hand-made
// master and this program only ever reads it.
package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

// markCrop is the square of drivel-logo.png that the icons are cut from: the
// big central head -- the Drive head, the only one that has ever been fed --
// with its ears and its grin whole and the head centred in the frame.
//
// It is stated here as numbers rather than found by an algorithm because it is
// a composition decision. The ears are what make the silhouette legible at
// 16px, which is the size the mascot brief asks the drawing to survive.
var markCrop = image.Rect(617, 22, 617+420, 22+420)

// iconGround is what the icons that may not be transparent are flattened onto.
//
// Apple asks for an opaque apple-touch-icon and composites one onto black
// otherwise; Android does the same for a maskable icon. Dark rather than white
// because the artwork is drawn as a sticker and carries its own white keyline,
// so a dark ground is the one that shows the keyline doing its job.
var iconGround = color.NRGBA{R: 0x17, G: 0x16, B: 0x1b, A: 0xff}

func main() {
	root, err := repoRoot()
	if err != nil {
		die(err)
	}

	master, err := loadPNG(filepath.Join(root, "site/assets/images/drivel-logo.png"))
	if err != nil {
		die(err)
	}
	mark := resize(master, markCrop, 512, 512)

	type job struct {
		path  string
		image image.Image
	}
	jobs := []job{
		// Tab icons keep their transparency: a browser tab is not one colour,
		// and every one of them composites the icon itself.
		{"site/static/favicon-16x16.png", resize(mark, mark.Bounds(), 16, 16)},
		{"site/static/favicon-32x32.png", resize(mark, mark.Bounds(), 32, 32)},

		// Home-screen icons are flattened; see iconGround.
		{"site/static/apple-touch-icon.png", flatten(resize(mark, mark.Bounds(), 180, 180))},
		{"site/static/android-chrome-192x192.png", flatten(resize(mark, mark.Bounds(), 192, 192))},
		{"site/static/android-chrome-512x512.png", flatten(mark)},
	}
	for _, j := range jobs {
		if err := writePNG(filepath.Join(root, j.path), j.image); err != nil {
			die(err)
		}
		b := j.image.Bounds()
		fmt.Printf("  %-46s %dx%d\n", j.path, b.Dx(), b.Dy())
	}

	// favicon.ico is the one a browser asks for without being told to, so it
	// has to exist at the site root whatever the <link> tags say.
	ico, err := icoFile(mark, 16, 32, 48)
	if err != nil {
		die(err)
	}
	p := filepath.Join(root, "site/static/favicon.ico")
	if err := os.WriteFile(p, ico, 0o644); err != nil {
		die(err)
	}
	fmt.Printf("  %-46s 16,32,48\n", "site/static/favicon.ico")

	// The social card is JPEG, alone among these. It is the one image a link
	// preview fetches before anybody has decided to visit, it is a painting
	// rather than a diagram, and it has no transparency left to protect -- so
	// PNG spends three quarters of a megabyte to say what 90KB says.
	card := filepath.Join(root, "site/static/images/drivel-social.jpg")
	if err := writeJPEG(card, socialCard(master)); err != nil {
		die(err)
	}
	fmt.Printf("  %-46s 1200x630\n", "site/static/images/drivel-social.jpg")
}

// resize samples src's sub-rectangle r into a w by h image with a separable
// triangle filter, working in premultiplied alpha.
//
// Premultiplied matters: averaging straight RGBA across the artwork's cut-out
// edge blends the colour of fully transparent pixels into the visible ones and
// rings the whole animal with a halo. The filter radius follows the scale in
// each axis, so the same function downsamples to 16px and upsamples 420 to 512
// without either turning to mush.
func resize(src image.Image, r image.Rectangle, w, h int) *image.NRGBA {
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	sx, sy := float64(r.Dx())/float64(w), float64(r.Dy())/float64(h)
	rx, ry := math.Max(sx, 1), math.Max(sy, 1)

	for y := 0; y < h; y++ {
		cy := (float64(y)+0.5)*sy - 0.5
		y0, y1 := int(math.Floor(cy-ry)), int(math.Ceil(cy+ry))
		for x := 0; x < w; x++ {
			cx := (float64(x)+0.5)*sx - 0.5
			x0, x1 := int(math.Floor(cx-rx)), int(math.Ceil(cx+rx))

			var pr, pg, pb, pa, sum float64
			for yy := y0; yy <= y1; yy++ {
				wy := 1 - math.Abs(float64(yy)-cy)/ry
				if wy <= 0 {
					continue
				}
				for xx := x0; xx <= x1; xx++ {
					wx := 1 - math.Abs(float64(xx)-cx)/rx
					if wx <= 0 {
						continue
					}
					weight := wx * wy
					// Clamp to the edge rather than treating outside as
					// transparent, which would fade the border inward.
					px := clamp(r.Min.X+xx, r.Min.X, r.Max.X-1)
					py := clamp(r.Min.Y+yy, r.Min.Y, r.Max.Y-1)
					cr, cg, cb, ca := src.At(px, py).RGBA() // premultiplied
					pr += weight * float64(cr)
					pg += weight * float64(cg)
					pb += weight * float64(cb)
					pa += weight * float64(ca)
					sum += weight
				}
			}
			if sum == 0 {
				continue
			}
			dst.SetNRGBA(x, y, unpremultiply(pr/sum, pg/sum, pb/sum, pa/sum))
		}
	}
	return dst
}

func unpremultiply(r, g, b, a float64) color.NRGBA {
	if a <= 0 {
		return color.NRGBA{}
	}
	to8 := func(v float64) uint8 {
		return uint8(clampF(math.Round(v/a*255), 0, 255))
	}
	return color.NRGBA{R: to8(r), G: to8(g), B: to8(b), A: uint8(clampF(math.Round(a/257), 0, 255))}
}

// flatten composites an image onto iconGround, losing the alpha channel.
func flatten(src image.Image) image.Image {
	dst := image.NewNRGBA(src.Bounds())
	draw.Draw(dst, dst.Bounds(), &image.Uniform{iconGround}, image.Point{}, draw.Src)
	draw.Draw(dst, dst.Bounds(), src, src.Bounds().Min, draw.Over)
	return dst
}

// socialCard lays the whole animal on a 1200x630 card for og:image.
//
// The proportions are the ones Open Graph consumers crop to; anything else gets
// trimmed by whichever chat client rendered it. The ground is dark for the same
// reason the home-screen icons are: it is the side the keyline was drawn for.
func socialCard(master image.Image) image.Image {
	const w, h, pad = 1200, 630, 48
	card := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.Draw(card, card.Bounds(), &image.Uniform{iconGround}, image.Point{}, draw.Src)

	b := master.Bounds()
	scale := math.Min(float64(w-2*pad)/float64(b.Dx()), float64(h-2*pad)/float64(b.Dy()))
	iw, ih := int(float64(b.Dx())*scale), int(float64(b.Dy())*scale)
	art := resize(master, b, iw, ih)
	at := image.Pt((w-iw)/2, (h-ih)/2)
	draw.Draw(card, image.Rectangle{at, at.Add(image.Pt(iw, ih))}, art, image.Point{}, draw.Over)
	return card
}

// icoFile packs the mark at several sizes into a Windows icon.
//
// The entries are PNG rather than BMP, which every browser since IE11 reads and
// which keeps the alpha channel without the AND-mask contortion the BMP form
// needs.
func icoFile(mark image.Image, sizes ...int) ([]byte, error) {
	le16 := func(v uint16) []byte { return []byte{byte(v), byte(v >> 8)} }
	le32 := func(v uint32) []byte {
		return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}
	}

	var dir, body []byte
	dir = append(dir, le16(0)...)                  // reserved
	dir = append(dir, le16(1)...)                  // 1 = icon
	dir = append(dir, le16(uint16(len(sizes)))...) // entries
	offset := 6 + 16*len(sizes)

	for _, s := range sizes {
		var enc bytes.Buffer
		if err := png.Encode(&enc, resize(mark, mark.Bounds(), s, s)); err != nil {
			return nil, err
		}
		// 0 means 256 in this field; none of our sizes reach it, but say so.
		dim := byte(s)
		if s >= 256 {
			dim = 0
		}
		dir = append(dir, dim, dim, 0, 0)             // width, height, palette, reserved
		dir = append(dir, le16(1)...)                 // colour planes
		dir = append(dir, le16(32)...)                // bits per pixel
		dir = append(dir, le32(uint32(enc.Len()))...) // size of this image
		dir = append(dir, le32(uint32(offset))...)    // where it starts
		offset += enc.Len()
		body = append(body, enc.Bytes()...)
	}
	return append(dir, body...), nil
}

func loadPNG(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only
	return png.Decode(f)
}

func writeJPEG(path string, img image.Image) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 86}); err != nil {
		f.Close() //nolint:errcheck // the encode error is the one that matters
		return err
	}
	return f.Close()
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

// repoRoot finds the tree this file lives in, so the program can be run from
// anywhere rather than only from the root.
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

func clampF(v, lo, hi float64) float64 {
	return math.Max(lo, math.Min(hi, v))
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "gen-site-images:", err)
	os.Exit(1)
}
