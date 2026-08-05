# Agent-driven UI actions (SPEC-W9 Part B)

The receptionist can do more than talk: on the web widget it can **drive the
visitor's page** — show a page, highlight the booking form, or pre-select a
service — by invoking one of three UI action tools. Actions are validated on
the server, transported with the chat reply, and executed client-side by the
widget. Nothing is ever executed server-side.

## Action reference

| Tool (LLM-facing) | Action payload | Client behaviour |
| --- | --- | --- |
| `navigate_to_page(path)` | `{"type":"navigate","path":"/rooms"}` | Host page: `window.location.assign(path)` after a same-origin guard |
| `highlight_element(selector)` | `{"type":"highlight","selector":"#booking-form"}` | `querySelector` → smooth `scrollIntoView` → 2s terracotta outline pulse |
| `prefill_booking(offering_id)` | `{"type":"prefill_booking","offering_id":"<uuid>"}` | Booking form pre-selects the offering; `CustomEvent('opendesk:prefill', {detail:{offering_id}})` is also dispatched |

### Validation rules (server-side, `app/ui_actions.py`)

- **navigate** — `path` must start with `/`; no scheme, no host
  (`//evil.example`, `https://…`, `javascript:…` are all rejected);
  no whitespace; ≤2048 chars. Same-origin only.
- **highlight** — `selector` may only contain
  `[a-zA-Z0-9\-_#. :\[\]="'>]` and is capped at 120 chars — enough for
  id/class/attribute/descendant selectors, excluding everything that could
  smuggle markup or script.
- **prefill_booking** — `offering_id` must be a UUID (normalized to
  canonical lowercase form).

Invalid input ⇒ the tool returns an error string to the LLM (so it can
correct itself) and the action is **dropped** — it never reaches the client.

## Transport

- **Buffered** `POST /voice/chat`: the response gains an additive
  `ui_actions: [...]` array with the validated actions the LLM invoked this
  turn (empty by default — existing clients are unaffected).
- **SSE streaming** (`stream: true`): one `data: {"ui_action": {...}}` frame
  per action is emitted after the reply deltas and **before** the terminal
  `data: {"done": true, ...}` frame.
- The system prompt gets a short addendum ("you can offer to show pages,
  highlight the booking form…") injected from `app/chat.py`, only when the
  UI action tools are registered for the turn.

## Widget execution

Two halves, both failure-isolated (every action runs in `try/catch` — a bad
action never breaks the widget or the host page):

1. **Embed page bridge** (`apps/admin-web/app/embed/ui-actions-bridge.tsx`,
   mounted by `/embed/[siteSlug]`): passively taps `window.fetch` and scans
   `/voice/chat` responses (buffered JSON and SSE streams alike) on a
   **clone**, so the chat widget's own parsing is untouched. The tap is
   namespaced (`__opendeskUiActionsFetchTap`) so a second embed on the same
   page cannot double-wrap fetch, and the original `window.fetch` is
   restored when the widget unmounts. Actions are re-validated, then:
   - `highlight` runs in the iframe document when the selector matches
     (booking form, offering cards…);
   - `prefill_booking` dispatches `opendesk:prefill` **and** pre-selects the
     offering by clicking its card — the exact same code path as a visitor
     click on the booking form's "Choose a service" step;
   - `navigate` (and anything not handled locally) is forwarded to the host
     page via `postMessage`.
2. **Host loader** (`apps/admin-web/public/embed.js`): listens for
   `postMessage` from the widget iframe only (origin **and** source checked
   against the iframe it created), re-validates, and applies the action to
   the host page — `navigate` with the same-origin guard, `highlight` with
   the pulse CSS (injected once), `prefill_booking` re-dispatched as
   `opendesk:prefill` on the host document for custom integrations.

## Security model

- **Server-validated**: the only way an action enters the pipeline is
  through the tool layer, which validates against the exact rules above.
- **Same-origin navigation**: paths are single-leading-slash only; the host
  loader repeats the check before `location.assign`. No scheme, no host, no
  protocol-relative URLs, ever.
- **Selector sanitization**: the restrictive charset makes selectors pure
  CSS lookups — no URL fetches, no script execution. `querySelector`
  failures are swallowed.
- **Client re-validation**: both the bridge and the loader re-check every
  action before executing (defense in depth against a compromised
  transport).
- **postMessage trust**: the loader only honors messages whose `origin`
  matches the Agora host and whose `source` is the widget iframe it
  created.
- **Graceful degradation**: unknown action types, missing elements and DOM
  errors are all no-ops.

## Examples per vertical

- **Hospitality** — "Can I see the rooms?" → `navigate_to_page("/rooms")`;
  "Which one is the deluxe suite?" → `highlight_element("[data-offering='deluxe']")`.
- **Salon / barbershop** — "I want the full braids package" →
  `prefill_booking("<offering-uuid>")` — the booking form jumps straight to
  picking a time.
- **Clinic** — "Where do I book?" → `highlight_element("#booking-form")`
  scrolls the form into view with a warm pulse.
- **Auto repair / any vertical** — "Show me your pricing" →
  `navigate_to_page("/#pricing")`.

## Custom host-page integrations

A host site can react to prefill itself:

```html
<script>
  document.addEventListener("opendesk:prefill", function (e) {
    console.log("agent pre-selected offering", e.detail.offering_id);
  });
</script>
```
