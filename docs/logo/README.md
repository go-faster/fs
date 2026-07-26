# Logo

`logo.svg` is the whole thing — one file for every surface.

The go-faster bars with the project wordmark set below them as a fourth stripe,
sharing the bars' width and the same 50-unit gap they keep between themselves.
A single gradient period spans the artwork, so one bright crest travels through
the bars and then the wordmark as one continuous light. The crest runs parallel
to the sheared ends of the bars, so the light rakes the mark at its own angle.

The wordmark is **JetBrains Mono Bold**, converted to outlines. The SVG carries
no font dependency and needs no webfont at render time.

> JetBrains Mono is © The JetBrains Mono Project Authors, licensed under the
> SIL Open Font License 1.1 — <https://github.com/JetBrains/JetBrainsMono>.

## One file, both themes

The background is transparent and the SVG carries both palettes, switching on
`prefers-color-scheme`. On dark it glows: full-range palette, two-stage bloom,
and a specular pass riding the crest. On light it drops a few stops, sheds the
bloom and withdraws the specular pass, so it keeps its contrast instead of
blowing out against white.

```markdown
<img src="docs/logo/logo.svg" alt="go-faster/fs" width="420">
```

Two things to know. The switch follows the reader's system theme, not the
backdrop it happens to sit on — so a dark-theme reader viewing it on a white
card still gets the glowing version. And a renderer that ignores CSS media
queries (some SVG-to-PNG converters) falls back to the dark branch.

## Regenerating

```console
$ python3 generate.py > logo.svg
```

Layout constants (`GAP`, `PAD`), timing (`DUR`) and the palette sit near the top
of the script. It has no dependencies.

To change the wordmark text or weight, re-cut the outlines from a JetBrains Mono
TTF and paste the two constants it prints into `generate.py`:

```console
$ pip install fonttools
$ python3 extract_wordmark.py JetBrainsMono-Bold.ttf
```
