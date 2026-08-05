# Agora marketing site

Dependency-free, no-build-step static marketing site for the Agora platform.

## Files

| File | Purpose |
|---|---|
| `index.html` | Single-page site: hero, middleware strip, omnichannel strip, feature grid, verticals showcase (filterable), Growth (CAC App) section, how-it-works, pricing, CTA banner, footer |
| `growth.html` | Agora Growth (CAC App) showcase: Nigeria-first acquisition hero, 8 sector CAC-target cards, USSD/WhatsApp/SMS/voice/web channel strip, geo-targeting / referral engine / attribution features, NCC DND 2442 + NDPA compliance badges, demo CTA |
| `styles.css` | All styling — warm low-saturation palette (cream/terracotta/olive/ink), system font stack, mobile-first responsive, `prefers-reduced-motion` support |
| `main.js` | Tiny progressive-enhancement script: vertical filter chips + mobile nav close behaviour. The page is fully usable without it |

No external requests: no CDN fonts, images, or scripts. Total payload < 150 KB.

## Local preview

Any static file server works. From this directory:

```bash
# Python (no install needed on most systems)
python3 -m http.server 8000

# or Node
npx serve .
```

Then open <http://localhost:8000>.

You can also simply open `index.html` directly in a browser — there is no build
step and no module loading, so `file://` works too.

## Deployment

Serve the directory with any static host (nginx, Caddy, GitHub Pages, S3 + CDN, …).
No environment variables, server-side code, or build pipeline required.
