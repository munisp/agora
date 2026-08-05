# Embedding the booking widget (I20)

Agora ships a chromeless booking + chat page at `/embed/{siteSlug}` and a
tiny iframe loader at `/embed.js`, so tenants can put their AI receptionist on
their own website — the self-hosted equivalent of the baseline's ElevenLabs
widget.

## Quick start

Paste this snippet into any HTML page:

```html
<script src="http://localhost:3001/embed.js" data-site="acme" async></script>
```

The dashboard shows a ready-to-copy snippet (with the correct origin and slug)
on **Public Site → Embed on your website**.

## How it works

1. `embed.js` (served from `apps/admin-web/public/embed.js`) reads its own
   `data-*` attributes, creates an `<iframe>` pointing at
   `{origin}/embed/{siteSlug}` and inserts it where the script tag lives
   (or into the element matched by `data-target`).
2. `/embed/{siteSlug}` is a public Next.js page that renders the same booking
   flow and chat widget as `/p/{siteSlug}` — minus the header hero, voice
   button and footer ("chromeless"). It uses the tenant's theme (primary
   colour, logo, hero copy, template) automatically.
3. The site must be **published** (`PUT /v1/site` with `published: true`),
   otherwise the embed returns 404.

## Options

| Attribute     | Default   | Purpose                                        |
| ------------- | --------- | ---------------------------------------------- |
| `data-site`   | (required)| Public site slug, e.g. `acme`                  |
| `data-height` | `640px`   | iframe height                                  |
| `data-width`  | `100%`    | iframe width                                   |
| `data-target` | in-place  | CSS selector of a container to mount the iframe |

Example with a container:

```html
<div id="booking"></div>
<script src="http://localhost:3001/embed.js"
        data-site="acme" data-target="#booking" data-height="720px" async></script>
```

## Notes & limits

- **Self-hosted**: no third-party scripts or cookies; everything is served
  from the Agora web app origin.
- **Chat**: the embedded chat widget talks to the same receptionist
  (`POST /voice/chat`, streamed over SSE when available) as the public page.
- **Voice**: the voice button is hidden in embed mode; if enabled in the
  future, the iframe already carries `allow="microphone"`.
- **Sizing**: the iframe does not auto-resize; pick a fixed `data-height`
  that fits the booking flow.
- **CSP**: host pages need `frame-src` allowing the Agora origin if a
  Content-Security-Policy is in place.
