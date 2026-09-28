# The website

`syshlted.github.io/drivel` is built from [`site/`](../../site) by Hugo and
published by `.github/workflows/pages.yml`. This page is what you need before
changing it; [mascot.md](../project/mascot.md) covers the artwork itself.

## It renders the manual by mounting it, never by copying it

`hugo.toml` mounts `../docs/user` as `assets/manual`, and
`content/docs/_content.gotmpl` turns each file into a page. **Editing a file in
`docs/user/` changes the website**, with nothing under `site/` touched — which
is why the deploy workflow watches `docs/user/**` as well as `site/**`.

There is no second copy to keep in step, and that is the point. Three things
make it work:

- **Titles are lifted from the H1.** Those files open with one because they are
  written to be read on GitHub first, where nothing else supplies a heading. The
  adapter reads the title out of it and then **strips that heading from the
  body**, because Hextra prints the title itself and the page would otherwise
  show it twice and ship two `<h1>` elements.
- **The strip is anchored to the start of the file**, not to any line that looks
  like a heading. That is the difference between removing the heading the title
  came from and eating some later one.
- **Relative `*.md` links are rewritten** by `layouts/_markup/render-link.html`:
  a sibling becomes its published URL, and anything reaching outside `docs/user`
  — `../../MANIFESTO.md`, `drivel.1` — goes to the repository, because it has no
  published counterpart. Without this every cross-reference in the manual would
  404.

A file that is not listed in the adapter's order table still gets a page; it
just sorts last. A new doc appearing at the bottom of the nav is a better
failure than one that does not appear at all.

## The Hugo version is pinned, and the theme is a module

`pages.yml` pins `HUGO_VERSION`. **Build with that version or do not trust the
result** — Hugo's template and image APIs move between releases.

```sh
site/ $ hugo server          # local preview
site/ $ hugo build --minify --baseURL "https://syshlted.github.io/drivel/"
```

The theme, [Hextra](https://github.com/imfing/hextra), is pulled as a Hugo
*module*, so `site/` has a **`go.mod` of its own**. It is a separate module from
the program at the repository root: `go build ./...` and the linters never see
it, and `make` has no site target.

## Three rules for the layouts

**Do not invent `hx:`-prefixed classes.** Hextra ships a *precompiled* Tailwind
sheet containing only the utilities its own templates use, so a class invented
here may simply not exist — silently, and only on the one page nobody diffs.
Drivel's own CSS is `assets/css/custom.css`, a path Hextra looks for by name,
and everything in it is prefixed `drivel-`.

**The partial overrides are deliberate**, each with its reason in a comment at
the top of the file:

| Partial | Why it is overridden |
| --- | --- |
| `favicons.html` | The theme's points at Hextra's own mark, which would otherwise be the icon on every tab. |
| `banner.html` | The theme's banner has a close button. This one is the data-loss warning, and a reader who dismissed it has been told nothing. |
| `twitter_cards.html` | The internal template falls back to `.Summary`, which on the home page is a scrape of the rendered content. |
| `custom/head-end.html` | Hands the stylesheet the backdrop's URL — only a template can resolve a mounted asset's published path. |

**A `url()` in `custom.css` is a trap.** Hextra serves that stylesheet from
`/css/custom.css` under `hugo server` and from `/css/compiled/main.<hash>.css`
in a production build, so no relative URL is correct in both — and a
root-relative one breaks under the `/drivel/` path prefix the published site is
served from. Anything needing a resolved URL goes through a template.

## The images are generated, and `site/` holds no masters

Two `//go:build ignore` programs, run with `go run`, neither wired into `make`:

```sh
go install github.com/TheZoraiz/ascii-image-converter@latest   # once
go run contrib/gen-logo-assets.go     # artwork → crop, braille, scaled set, backdrop, cut-out
go run contrib/gen-site-images.go     # cut-out → favicons, navbar mark, social card
```

They are not `make` targets on purpose: nothing about a build depends on their
output, it changes only when the artwork does, and a drift check would mean
promising byte-identical PNG output across Go releases.

The hero image and the page backdrop are **mounted from `contrib/`** rather than
copied into `site/`, the same arrangement as the manual and for the same reason.
The artwork version is named in `site/hugo.toml` and in `gen-site-images.go` —
both, after a redraw.

**Background removal is connectivity, never a brightness threshold.** The
mascot's teeth are near-white and its drool is paler than the paper; both
survive only because they are sealed inside ink outlines that a fill starting at
the frame's border cannot cross. A pass to catch the background pockets trapped
between heads was written, measured and deleted — at any tolerance loose enough
to find them it also took drool, puddles and a highlight out of an eye. Don't
reintroduce it. [mascot.md](../project/mascot.md) has the measurements.

## The work-in-progress warning has one source

`docs/user/status.md`. Everything else — the site-wide banner in `hugo.toml`'s
`params.banner`, both `README`s, the man page, `CHANGELOG.md` — is a short
pointer at it and stays short.

In the manual it is written as a markdown alert (`> [!CAUTION]`), because GitHub
and Hextra both style that already, so one form is loud in the repository and on
the website with no second copy. When it stops being true it comes out of all of
them together.

## Deploys

On push to `master` touching `site/**` or `docs/user/**`. Only `site/public` is
uploaded, so nothing else in the tree becomes a published URL.

Two repository settings the workflow cannot control: the repository must be
public, and **Settings → Pages → Source must be "GitHub Actions"**.
