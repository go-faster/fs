#!/usr/bin/env python3
"""Rebuild the WORD_D outline constant in generate.py from a JetBrains Mono TTF.

    pip install fonttools
    python3 extract_wordmark.py path/to/JetBrainsMono-Bold.ttf

Prints the Python literal to paste into generate.py, plus the matching
WORD_BBOX. Only needed if the wordmark text or weight changes — generate.py
itself has no dependencies.
"""
import re
import sys

from fontTools.misc.transform import Transform
from fontTools.pens.boundsPen import BoundsPen
from fontTools.pens.svgPathPen import SVGPathPen
from fontTools.pens.transformPen import TransformPen
from fontTools.ttLib import TTFont

TEXT = "go-faster/fs"


def outline(font_path, text=TEXT):
    font = TTFont(font_path)
    cmap, glyphs, hmtx = font.getBestCmap(), font.getGlyphSet(), font["hmtx"]
    bounds = BoundsPen(glyphs)
    parts, pen_x = [], 0
    for ch in text:
        gname = cmap[ord(ch)]
        flip = Transform(1, 0, 0, -1, pen_x, 0)   # y-up font -> y-down SVG
        svg = SVGPathPen(glyphs)
        glyphs[gname].draw(TransformPen(svg, flip))
        if svg.getCommands():
            parts.append(svg.getCommands())
        glyphs[gname].draw(TransformPen(bounds, flip))
        pen_x += hmtx[gname][0]
    return " ".join(parts), bounds.bounds


def main():
    if len(sys.argv) != 2:
        sys.exit(__doc__)
    d, bbox = outline(sys.argv[1])
    # Break only before a subpath 'M' so no numeric token is ever split in half.
    subpaths = re.findall(r"M[^M]*", d)
    assert "".join(subpaths) == d, "lossless split failed"
    print("WORD_D = (")
    for s in subpaths:
        print(f'    "{s.strip()}"')
    print(")")
    print()
    print("WORD_BBOX = ({:.1f}, {:.1f}, {:.1f}, {:.1f})".format(*bbox))


if __name__ == "__main__":
    main()
