"""Builds the Lichii Shader Sync icon: Lichii's lychee + a circular sync badge.
python -E -P tools/make_icon.py LICHII_256.png OUTDIR
Writes OUTDIR/icon.ico (16-256 px), icon-256.png and icon-1024.png (for the README / GUI header)."""
import sys, os, math
from PIL import Image, ImageDraw

src, out = sys.argv[1], sys.argv[2]
os.makedirs(out, exist_ok=True)
S = 1024

def badge(size):
    """Green disc with a white rim and two white circular arrows, drawn at 4x then reduced."""
    k = 4; n = size * k
    im = Image.new('RGBA', (n, n), (0, 0, 0, 0))
    d = ImageDraw.Draw(im)
    c = n / 2
    rim = n * 0.06
    d.ellipse([0, 0, n - 1, n - 1], fill=(255, 255, 255, 255))
    d.ellipse([rim, rim, n - 1 - rim, n - 1 - rim], fill=(46, 160, 67, 255))
    r = n * 0.27          # arrow radius
    t = n * 0.085         # stroke width
    for start in (200, 20):  # two arcs, each ~130 degrees, leaving gaps for the heads
        end = start + 125
        d.arc([c - r, c - r, c + r, c + r], start, end, fill='white', width=int(t))
        # arrow head at the arc end, pointing along the direction of travel (clockwise in image space)
        a = math.radians(end)
        px, py = c + r * math.cos(a), c + r * math.sin(a)
        tx, ty = -math.sin(a), math.cos(a)          # tangent (increasing angle)
        nx, ny = math.cos(a), math.sin(a)           # outward normal
        h = n * 0.13
        tip = (px + tx * h * 0.9, py + ty * h * 0.9)
        l = (px + nx * h * 0.75 - tx * h * 0.15, py + ny * h * 0.75 - ty * h * 0.15)
        rr = (px - nx * h * 0.75 - tx * h * 0.15, py - ny * h * 0.75 - ty * h * 0.15)
        d.polygon([tip, l, rr], fill='white')
    return im.resize((size, size), Image.LANCZOS)

base = Image.open(src).convert('RGBA').resize((S, S), Image.LANCZOS)
b = badge(int(S * 0.46))
canvas = base.copy()
# soft shadow under the badge
sh = Image.new('RGBA', b.size, (0, 0, 0, 0))
ImageDraw.Draw(sh).ellipse([8, 14, b.width - 1, b.height - 1], fill=(0, 0, 0, 70))
x, y = S - b.width - int(S * 0.01), S - b.height - int(S * 0.01)
canvas.alpha_composite(sh, (x + 6, y + 10))
canvas.alpha_composite(b, (x, y))
canvas.save(os.path.join(out, 'icon-1024.png'))
canvas.resize((256, 256), Image.LANCZOS).save(os.path.join(out, 'icon-256.png'))
sizes = [(16, 16), (24, 24), (32, 32), (48, 48), (64, 64), (128, 128), (256, 256)]
canvas.save(os.path.join(out, 'icon.ico'), sizes=sizes)
print('ok')
