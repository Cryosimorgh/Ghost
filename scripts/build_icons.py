"""Build reproducible PNG/ICO icon variants from assets/ghost.svg."""
from pathlib import Path
from io import BytesIO
from PIL import Image
import cairosvg

root = Path(__file__).resolve().parents[1]
svg = (root / 'assets' / 'ghost.svg').read_bytes()
img = Image.open(BytesIO(cairosvg.svg2png(bytestring=svg, output_width=1024, output_height=1024))).convert('RGBA')
for size in (16, 24, 32, 48, 64, 128, 256, 512):
    img.resize((size, size), Image.Resampling.LANCZOS).save(root / 'assets' / f'ghost-{size}.png')
img.save(root / 'assets' / 'ghost.ico', format='ICO', sizes=[(16,16),(24,24),(32,32),(48,48),(64,64),(128,128),(256,256)])
for size in (32, 256):
    (root / 'web' / f'ghost-{size}.png').write_bytes((root / 'assets' / f'ghost-{size}.png').read_bytes())
(root / 'web' / 'ghost.svg').write_bytes(svg)
