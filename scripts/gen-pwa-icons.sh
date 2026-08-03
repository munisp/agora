#!/usr/bin/env bash
# gen-pwa-icons.sh — regenerate the OpenDesk PWA icons (SPEC-W16 §2).
#
# Draws simple branded placeholder icons with python3 + Pillow (no SVG
# toolchain required) and writes PNGs into each PWA's icons directory:
#
#   apps/admin-web/public/icons/icon-{192,512}.png          (any + maskable)
#   apps/field-pwa/icons/icon-{192,512}.png
#
# Brand tokens (apps/admin-web/app/globals.css):
#   background  #faf7f1  (warm paper)
#   foreground  #2e2a25  (warm ink)
#   primary     #7c5b3e  (warm brown)
#   ring        #a98d68  (warm tan)
#
# Maskable icons keep all artwork inside the 80% safe zone
# (https://web.dev/articles/maskable-icon).
#
# Usage:  bash scripts/gen-pwa-icons.sh
set -euo pipefail

cd "$(dirname "$0")/.."

python3 - <<'PY'
"""Generate branded placeholder PWA icons for admin-web and field-pwa."""
from PIL import Image, ImageDraw

# Brand palette (admin-web design tokens).
BG = (250, 247, 241)        # #faf7f1
PRIMARY = (124, 91, 62)     # #7c5b3e
RING = (169, 141, 104)      # #a98d68
INK = (46, 42, 37)          # #2e2a25

# Field PWA accent (marketing styles.css terracotta) to tell the apps apart.
FIELD_PRIMARY = (184, 85, 47)   # #b8552f


def draw_icon(size: int, primary, glyph: str) -> Image.Image:
    """Rounded warm-paper tile, primary ring, centered glyph letter.

    All artwork stays within the central 80% safe zone so the same PNG is
    valid as both `any` and `maskable`.
    """
    img = Image.new("RGBA", (size, size), (0, 0, 0, 0))
    d = ImageDraw.Draw(img)

    # Full-bleed square background (required for maskable: launchers apply
    # their own mask, so no transparent corners and no pre-rounded shape).
    d.rectangle([0, 0, size - 1, size - 1], fill=BG + (255,))

    # Safe zone: central 80%.
    margin = size * 0.10
    box = [margin, margin, size - margin, size - margin]

    # Primary disc inside the safe zone.
    d.ellipse(box, fill=primary + (255,))

    # Inner ring accent.
    inset = size * 0.045
    ring_box = [box[0] + inset, box[1] + inset, box[2] - inset, box[3] - inset]
    d.ellipse(ring_box, outline=RING + (255,), width=max(2, size // 64))

    # Glyph: draw the letter as a simple geometric mark (no font dependency):
    # a warm-paper "desk" monogram — a horizontal bar over two legs for admin,
    # a location pin dot for field.
    cx = cy = size / 2
    if glyph == "admin":
        bar_w, bar_h = size * 0.30, size * 0.055
        d.rounded_rectangle(
            [cx - bar_w, cy - size * 0.16, cx + bar_w, cy - size * 0.16 + bar_h],
            radius=bar_h / 2, fill=BG + (255,),
        )
        leg_w, leg_h = size * 0.055, size * 0.22
        for lx in (cx - bar_w * 0.75, cx + bar_w * 0.75 - leg_w):
            d.rounded_rectangle(
                [lx, cy - size * 0.10, lx + leg_w, cy - size * 0.10 + leg_h],
                radius=leg_w / 2, fill=BG + (255,),
            )
    else:  # field: pin = circle + tail
        r = size * 0.16
        d.ellipse([cx - r, cy - size * 0.22, cx + r, cy - size * 0.22 + 2 * r], fill=BG + (255,))
        d.polygon(
            [(cx - r * 0.7, cy - size * 0.09), (cx + r * 0.7, cy - size * 0.09), (cx, cy + size * 0.16)],
            fill=BG + (255,),
        )
        dot = size * 0.05
        d.ellipse([cx - dot, cy - size * 0.17, cx + dot, cy - size * 0.17 + 2 * dot], fill=primary + (255,))
    return img


TARGETS = [
    ("apps/admin-web/public/icons", PRIMARY, "admin"),
    ("apps/field-pwa/icons", FIELD_PRIMARY, "field"),
]

for out_dir, primary, glyph in TARGETS:
    import os
    os.makedirs(out_dir, exist_ok=True)
    for size in (192, 512):
        path = f"{out_dir}/icon-{size}.png"
        draw_icon(size, primary, glyph).save(path, "PNG", optimize=True)
        print(f"wrote {path}")
PY
