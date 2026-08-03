# PWA icons

The PNG icons in this directory are **generated, not hand-authored**. They are
byte-deterministic outputs of:

```bash
bash scripts/gen-pwa-icons.sh   # from the repo root; requires python3 + PIL
```

Run the script after cloning (or in CI) to produce `icon-192.png` and
`icon-512.png` here and in `apps/field-pwa/icons/`. The script writes both
`any`-purpose and maskable variants (80% safe zone) referenced by
`../manifest.webmanifest`.

The binaries are intentionally not committed through the text-only sync
channel used for this repository mirror; the generator reproduces them
byte-identically (verified by md5 in the Wave-16 gate).
