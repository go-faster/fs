#!/usr/bin/env python3
"""Generate the go-faster/fs logo.

    python3 generate.py > logo.svg

The wordmark is JetBrains Mono Bold, converted to outlines so the file carries
no font dependency and needs no webfont at render time. Re-run
extract_wordmark.py against a JetBrains Mono TTF to rebuild WORD_D below.
One SVG serves every surface: it carries both palettes and swaps them on
prefers-color-scheme.
"""

# "go-faster/fs" set in JetBrains Mono Bold (SIL OFL 1.1, (c) The JetBrains Mono
# Project Authors) and converted to outlines. Font units, y-down, baseline y=0.
WORD_D = (
    "M152 180V76H307Q351 76 375.0 52.5Q399 29 399 -11V-44L401 -130Q384 -84 346.5 -58.5Q309 -33 255 -33Q171 -33 121.5 -89.5Q72 -146 72 -243V-350Q72 -446 121.5 -503.0Q171 -560 255 -560Q309 -560 346.5 -534.5Q384 -509 401 -462V-550H523V-8Q523 79 465.5 129.5Q408 180 309 180Z"
    "M297 -140Q345 -140 371.5 -168.5Q398 -197 398 -248V-344Q398 -395 371.5 -423.5Q345 -452 297 -452Q249 -452 223.0 -424.5Q197 -397 197 -348V-244Q197 -195 223.0 -167.5Q249 -140 297 -140Z"
    "M900 9Q794 9 732.0 -50.5Q670 -110 670 -211V-339Q670 -441 732.0 -500.0Q794 -559 900 -559Q1006 -559 1068.0 -500.0Q1130 -441 1130 -339V-211Q1130 -110 1068.0 -50.5Q1006 9 900 9Z"
    "M900 -100Q950 -100 977.5 -127.5Q1005 -155 1005 -207V-343Q1005 -395 977.5 -422.5Q950 -450 900 -450Q850 -450 822.5 -422.5Q795 -395 795 -343V-207Q795 -155 822.5 -127.5Q850 -100 900 -100Z"
    "M1340 -272V-388H1660V-272Z"
    "M2000 0V-362H1849V-475H2000V-572Q2000 -644 2048.0 -687.0Q2096 -730 2175 -730H2336V-620H2178Q2154 -620 2139.5 -606.5Q2125 -593 2125 -570V-475H2336V-362H2125V0Z"
    "M2639 10Q2555 10 2506.5 -37.5Q2458 -85 2458 -162Q2458 -242 2513.0 -289.5Q2568 -337 2664 -337H2798V-376Q2798 -459 2701 -459Q2657 -459 2631.0 -442.0Q2605 -425 2602 -394H2482Q2486 -468 2544.5 -514.0Q2603 -560 2702 -560Q2807 -560 2865.0 -511.0Q2923 -462 2923 -373V0H2801V-103Q2794 -50 2751.0 -20.0Q2708 10 2639 10Z"
    "M2679 -92Q2734 -92 2766.0 -119.0Q2798 -146 2798 -191V-256H2670Q2629 -256 2605.0 -233.0Q2581 -210 2581 -174Q2581 -137 2606.5 -114.5Q2632 -92 2679 -92Z"
    "M3277 9Q3179 9 3122.0 -35.5Q3065 -80 3065 -156H3188Q3188 -124 3211.5 -107.0Q3235 -90 3277 -90H3321Q3366 -90 3390.5 -107.0Q3415 -124 3415 -156Q3415 -186 3398.0 -200.5Q3381 -215 3346 -220L3209 -238Q3143 -247 3107.5 -289.0Q3072 -331 3072 -398Q3072 -476 3124.0 -517.5Q3176 -559 3274 -559H3320Q3412 -559 3468.0 -517.0Q3524 -475 3526 -405H3402Q3401 -431 3379.0 -446.5Q3357 -462 3320 -462H3274Q3234 -462 3213.0 -445.5Q3192 -429 3192 -401Q3192 -376 3207.5 -364.0Q3223 -352 3253 -349L3382 -331Q3458 -322 3496.5 -277.5Q3535 -233 3535 -158Q3535 -78 3480.5 -34.5Q3426 9 3321 9Z"
    "M3955 0Q3878 0 3832.5 -44.5Q3787 -89 3787 -165V-437H3635V-550H3787V-705H3913V-550H4132V-437H3913V-168Q3913 -144 3926.5 -128.5Q3940 -113 3964 -113H4127V0Z"
    "M4501 10Q4431 10 4379.0 -17.0Q4327 -44 4298.5 -93.5Q4270 -143 4270 -210V-340Q4270 -407 4298.5 -456.5Q4327 -506 4379.0 -533.0Q4431 -560 4501 -560Q4570 -560 4621.5 -533.0Q4673 -506 4701.5 -457.5Q4730 -409 4730 -344V-244H4390V-206Q4390 -150 4419.0 -121.5Q4448 -93 4501 -93Q4540 -93 4567.5 -106.5Q4595 -120 4600 -146H4724Q4711 -75 4650.0 -32.5Q4589 10 4501 10Z"
    "M4390 -344V-325L4610 -326V-345Q4610 -402 4582.0 -433.0Q4554 -464 4501 -464Q4446 -464 4418.0 -432.5Q4390 -401 4390 -344Z"
    "M4897 0V-550H5019V-443Q5029 -497 5068.5 -528.5Q5108 -560 5171 -560Q5255 -560 5302.0 -506.0Q5349 -452 5349 -355V-303H5224V-349Q5224 -399 5198.0 -425.5Q5172 -452 5124 -452Q5074 -452 5048.0 -422.0Q5022 -392 5022 -337V0Z"
    "M5460 110 5810 -830H5940L5590 110Z"
    "M6200 0V-362H6049V-475H6200V-572Q6200 -644 6248.0 -687.0Q6296 -730 6375 -730H6536V-620H6378Q6354 -620 6339.5 -606.5Q6325 -593 6325 -570V-475H6536V-362H6325V0Z"
    "M6877 9Q6779 9 6722.0 -35.5Q6665 -80 6665 -156H6788Q6788 -124 6811.5 -107.0Q6835 -90 6877 -90H6921Q6966 -90 6990.5 -107.0Q7015 -124 7015 -156Q7015 -186 6998.0 -200.5Q6981 -215 6946 -220L6809 -238Q6743 -247 6707.5 -289.0Q6672 -331 6672 -398Q6672 -476 6724.0 -517.5Q6776 -559 6874 -559H6920Q7012 -559 7068.0 -517.0Q7124 -475 7126 -405H7002Q7001 -431 6979.0 -446.5Q6957 -462 6920 -462H6874Q6834 -462 6813.0 -445.5Q6792 -429 6792 -401Q6792 -376 6807.5 -364.0Q6823 -352 6853 -349L6982 -331Q7058 -322 7096.5 -277.5Q7135 -233 7135 -158Q7135 -78 7080.5 -34.5Q7026 9 6921 9Z"
)

WORD_BBOX = (72.0, -830.0, 7135.0, 180.0)   # ink bounds of the outline above

# --- mark geometry (the go-faster bars) ---
MARK = [
    "M594.91,369.52q-1.77,17-1.91,34.45a3.47,3.47,0,0,1-3.43,3.46l-217,2.36a3.49,3.49,0,0,1-3.24-4.86l15.53-36.17a5.23,5.23,0,0,1,3.94-2.68l202.64-.4A3.48,3.48,0,0,1,594.91,369.52Z",
    "M617.07,282.08c-4.08,10.65-8.31,21.44-11.28,32.36-.41,1.51-2.41,2.56-4,2.56h-468a3.54,3.54,0,0,1-2.76-5.72l21.08-31.34a5.41,5.41,0,0,1,3.75-1.76l456.6-.84C614.94,277.33,618,279.79,617.07,282.08Z",
    "M669.69,191.7a.9.9,0,0,1-.2.57,401.62,401.62,0,0,0-23.81,34.09,3.44,3.44,0,0,1-2.93,1.64H282.9c-3,0-4.64-3.92-2.83-6.61l19.44-29a8.55,8.55,0,0,1,4.62-2.37H667.65A2.13,2.13,0,0,1,669.69,191.7Z",
]
MARK_W, MARK_H = 539.38, 219.79
MARK_OFF = (-130.31, -190.0)       # translate applied to the paths above

# --- layout ---
# The wordmark sits below the bars as a fourth stripe: its ink spans exactly the
# mark's width, one bar-gap below it.
INK_L, INK_T, INK_R, INK_B = WORD_BBOX
scale = MARK_W / (INK_R - INK_L)
GAP = 50.0                         # the same gap the bars keep between themselves
WORD_X = -INK_L * scale            # left ink edge lands on x=0
WORD_Y = MARK_H + GAP - INK_T * scale

PAD = 36.0
vb = (-PAD, -PAD, MARK_W + 2 * PAD, WORD_Y + INK_B * scale + 2 * PAD)
DUR = "4s"

# The crest runs parallel to the sheared ends of the bars (21.08 across, 31.34
# up), so the light rakes the artwork at its own angle. The sweep vector is the
# perpendicular; its length is one gradient period.
SHEAR = 21.08 / 31.34
SWEEP = (vb[0], 0.0, vb[2], vb[2] * SHEAR)


def gradient(gid, stops, offset, scale):
    """Emit the shared sweep restated in the local user space of the group that
    references it, so every layer stays locked to one continuous crest.

    A userSpaceOnUse paint server resolves against the user space of the element
    it paints, so a transformed group would otherwise drag the gradient with it.
    """
    ox, oy = offset
    x1, y1 = (SWEEP[0] - ox) / scale, (SWEEP[1] - oy) / scale
    vx, vy = SWEEP[2] / scale, SWEEP[3] / scale
    return f'''<linearGradient id="{gid}" gradientUnits="userSpaceOnUse" spreadMethod="repeat"
                    x1="{x1:.2f}" y1="{y1:.2f}" x2="{x1 + vx:.2f}" y2="{y1 + vy:.2f}">
{stops}
      <animateTransform attributeName="gradientTransform" type="translate"
                        from="0 0" to="{vx:.2f} {vy:.2f}" dur="{DUR}" repeatCount="indefinite"/>
    </linearGradient>'''


FLOW_STOPS = "\n".join(
    f'      <stop offset="{o}" class="{c}"/>' for o, c in [
        ("0", "f0"), ("0.16", "f1"), ("0.34", "f2"), ("0.44", "f3"),
        ("0.50", "f4"), ("0.56", "f3"), ("0.66", "f2"), ("0.84", "f1"), ("1", "f0"),
    ])

GLINT_STOPS = "\n".join(
    f'      <stop offset="{o}" stop-color="#a8e8ff" stop-opacity="{a}"/>'
    for o, a in [("0", "0"), ("0.40", "0"), ("0.50", "0.2"), ("0.60", "0"), ("1", "0")])


def main():
    word_off = (WORD_X, WORD_Y)
    grads = "\n    ".join([
        gradient("flow", FLOW_STOPS, MARK_OFF, 1.0),
        gradient("flowText", FLOW_STOPS, word_off, scale),
        gradient("glint", GLINT_STOPS, MARK_OFF, 1.0),
        gradient("glintText", GLINT_STOPS, word_off, scale),
    ])

    bars = "\n".join(f'        <path d="{d}"/>' for d in MARK)
    word_tf = f'translate({WORD_X:.2f} {WORD_Y:.2f}) scale({scale:.6f})'

    def layer(paint, cls):
        return f'''      <g class="{cls}" fill="url(#{paint})" transform="translate({MARK_OFF[0]} {MARK_OFF[1]})">
{bars}
      </g>
      <g class="{cls}" fill="url(#{paint}Text)" transform="{word_tf}">
        <path d="{WORD_D}"/>
      </g>'''

    return f'''<svg xmlns="http://www.w3.org/2000/svg"
     viewBox="{vb[0]:.0f} {vb[1]:.0f} {vb[2]:.0f} {vb[3]:.0f}" width="{vb[2]:.0f}" height="{vb[3]:.0f}"
     role="img" aria-label="go-faster/fs">
  <title>go-faster/fs</title>
  <style>
    /* Dark is the base. On light surfaces the palette drops a few stops, the
       bloom comes off and the specular pass is withdrawn, so the artwork keeps
       its contrast instead of blowing out against white. */
    .f0 {{ stop-color: #00808f }}
    .f1 {{ stop-color: #00a29c }}
    .f2 {{ stop-color: #01add8 }}
    .f3 {{ stop-color: #33c4e6 }}
    .f4 {{ stop-color: #74d9f2 }}
    .art {{ filter: url(#bloom) }}
    @media (prefers-color-scheme: light) {{
      .f0 {{ stop-color: #04697a }}
      .f1 {{ stop-color: #00868c }}
      .f2 {{ stop-color: #0090bd }}
      .f3 {{ stop-color: #00a8d2 }}
      .f4 {{ stop-color: #2ec2e0 }}
      .art {{ filter: none }}
      .glint {{ display: none }}
    }}
  </style>
  <defs>
    <!-- One gradient period spans the whole logo, so a single bright crest
         travels through the bars and then the wordmark as one light. -->
    {grads}

    <!-- Two-stage bloom: a wide halo for ambient glow and a tight one that
         keeps edges hot, with the crisp artwork composited back on top. -->
    <filter id="bloom" x="-25%" y="-25%" width="150%" height="150%"
            color-interpolation-filters="sRGB">
      <feGaussianBlur in="SourceGraphic" stdDeviation="13" result="wide"/>
      <feComponentTransfer in="wide" result="wideDim">
        <feFuncA type="linear" slope="0.48"/>
      </feComponentTransfer>
      <feGaussianBlur in="SourceGraphic" stdDeviation="3" result="near"/>
      <feComponentTransfer in="near" result="nearDim">
        <feFuncA type="linear" slope="0.4"/>
      </feComponentTransfer>
      <feMerge>
        <feMergeNode in="wideDim"/>
        <feMergeNode in="nearDim"/>
        <feMergeNode in="SourceGraphic"/>
      </feMerge>
    </filter>
  </defs>

  <g class="art">
{layer("flow", "base")}
{layer("glint", "glint")}
  </g>
</svg>
'''


if __name__ == "__main__":
    import sys
    sys.stdout.write(main())
