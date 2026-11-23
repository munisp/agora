"""SPEC-W115 C5: caller document intake for the voice runtime.

Three surfaces, one OCR pipeline:

  - POST /voice/documents/upload            web chat attachment (public)
  - POST /voice/sessions/{id}/document-link mint an expiring share link
                                            (voice mid-call; session-secret
                                            authorized, K15(b) idiom)
  - POST /voice/documents/claim/{token}     public upload via a share link
  - GET  /voice/sessions/{id}/documents     agent/staff view of a session's
                                            shared documents (session-secret
                                            authorized)

FAIL-CLOSED (C4): every endpoint answers 503 when OCR_SERVICE_URL /
OCR_INTERNAL_TOKEN are unset; link minting additionally requires
VOICE_DOCUMENT_LINK_SECRET. No silent degradation, no mock path.

The share-link token is stateless: base64url(json{sid,t,exp,n}) + "." +
HMAC-SHA256 hex over the payload, verified in constant time. Short TTL
(default 15 min) + the APISIX limit-count on /voice/* bound the abuse
window; a claimed document is correlated to its session via the token's
`sid`, never via client-supplied session ids.
"""

from __future__ import annotations

import base64
import hashlib
import hmac
import json
import time
import uuid
from typing import Any, Awaitable, Callable

import httpx
from fastapi import FastAPI, HTTPException, Request

from .logging import get_logger
from .session_state import store_get, store_save_session

log = get_logger("documents")

#: Caller-shareable kinds (SPEC-W116 §4: ALL non-KYC kinds — pay stub was
#: the example, not the feature). KYC kinds are NEVER caller-shareable
#: here — they keep their own consent-gated intake (kyc-service, SPEC-W58).
CALLER_DOCUMENT_KINDS = ("pay_stub", "receipt", "document")

_KIND_LABELS = {"pay_stub": "pay stub", "receipt": "receipt", "document": "document"}

#: Magic-byte prefixes -> declared mime (fail-fast pre-check before the
#: bytes ever leave the process; ocr-service re-validates authoritatively).
_MAGIC = (
    (b"\xff\xd8\xff", "image/jpeg"),
    (b"\x89PNG\r\n\x1a\n", "image/png"),
    (b"%PDF", "application/pdf"),
)


def sniff_mime(data: bytes) -> str | None:
    for prefix, mime in _MAGIC:
        if data.startswith(prefix):
            return mime
    # RIFF....WEBP
    if len(data) >= 12 and data[:4] == b"RIFF" and data[8:12] == b"WEBP":
        return "image/webp"
    return None


# ---------------------------------------------------------------------------
# share-link tokens (C5)
# ---------------------------------------------------------------------------
def mint_document_link(
    *, secret: str, session_id: str, tenant_id: str, ttl_s: int, now: float | None = None
) -> tuple[str, float]:
    """Returns (token, expires_at_epoch). Stateless HMAC token."""
    exp = (now if now is not None else time.time()) + ttl_s
    payload = {
        "sid": session_id,
        "t": tenant_id,
        "exp": int(exp),
        "n": uuid.uuid4().hex[:12],
    }
    raw = base64.urlsafe_b64encode(
        json.dumps(payload, separators=(",", ":")).encode()
    ).decode().rstrip("=")
    sig = hmac.new(secret.encode(), raw.encode(), hashlib.sha256).hexdigest()
    return f"{raw}.{sig}", exp


class LinkTokenError(ValueError):
    """Invalid or expired share-link token (=> 401)."""


def verify_document_link(
    *, secret: str, token: str, now: float | None = None
) -> dict[str, Any]:
    try:
        raw, sig = token.split(".", 1)
    except ValueError:
        raise LinkTokenError("malformed token") from None
    expected = hmac.new(secret.encode(), raw.encode(), hashlib.sha256).hexdigest()
    if not hmac.compare_digest(sig, expected):
        raise LinkTokenError("bad signature")
    try:
        padded = raw + "=" * (-len(raw) % 4)
        payload = json.loads(base64.urlsafe_b64decode(padded.encode()))
    except Exception:
        raise LinkTokenError("undecodable payload") from None
    if not isinstance(payload, dict) or not payload.get("sid") or not payload.get("t"):
        raise LinkTokenError("incomplete payload")
    exp = payload.get("exp")
    if not isinstance(exp, int):
        raise LinkTokenError("missing expiry")
    if (now if now is not None else time.time()) > exp:
        raise LinkTokenError("token expired")
    return payload


# ---------------------------------------------------------------------------
# OCR proxy (production httpx client; tests inject a fake with the same
# async extract() signature — sanctioned DI seam, no production mock path)
# ---------------------------------------------------------------------------
class OcrDocumentClient:
    def __init__(self, *, base_url: str, internal_token: str, timeout_s: float):
        self._base = base_url.rstrip("/")
        self._token = internal_token
        self._client = httpx.AsyncClient(timeout=timeout_s)

    async def aclose(self) -> None:
        await self._client.aclose()

    async def extract(
        self,
        *,
        tenant_id: str,
        kind: str,
        filename: str,
        data: bytes,
        mime: str,
        source_channel: str,
    ) -> dict[str, Any]:
        resp = await self._client.post(
            f"{self._base}/v1/ocr/extract",
            files={"file": (filename, data, mime)},
            data={"kind": kind, "tenant_id": tenant_id,
                  "source_channel": source_channel},
            headers={"X-Internal-Token": self._token},
        )
        if resp.status_code != 200:
            raise OcrUpstreamError(resp.status_code)
        return resp.json()


class OcrUpstreamError(RuntimeError):
    def __init__(self, status_code: int):
        super().__init__(f"ocr upstream answered {status_code}")
        self.status_code = status_code


# ---------------------------------------------------------------------------
# human verdict summary (pure)
# ---------------------------------------------------------------------------
_REASON_PHRASES = {
    "net_exceeds_gross": "the net pay is higher than the gross pay",
    "period_inverted": "the pay period dates are reversed",
    "arithmetic_inconsistent": "the pay figures do not add up",
    "period_unparseable": "the pay period dates could not be read",
    "currency_missing": "the currency is missing",
    "no_validator_for_kind": "this document type has no automatic checks",
}


def _human_reason(reason: str) -> str:
    if reason.startswith("missing_required_field:"):
        field = reason.split(":", 1)[1].replace("_", " ")
        return f"the {field} is missing"
    return _REASON_PHRASES.get(reason, reason.replace("_", " "))


def verdict_summary(kind: str, verdict: str, reasons: list[str]) -> str:
    """One spoken/display sentence. Honest: a heuristic consistency check,
    never 'authentic' / 'genuine'."""
    label = _KIND_LABELS.get(kind, "document")
    if verdict == "validated":
        return f"Your {label} was received and passed all consistency checks."
    if verdict == "rejected":
        detail = f": {_human_reason(reasons[0])}" if reasons else ""
        return f"We could not accept this {label}{detail}."
    # needs_review
    return f"Your {label} was received; a team member will review it shortly."


# ---------------------------------------------------------------------------
# route wiring
# ---------------------------------------------------------------------------
TenantResolver = Callable[[str], Awaitable[str]]  # site_slug -> tenant uuid


def wire_document_routes(
    app: FastAPI,
    *,
    settings: Any,
    sessions: Any,
    ocr_client: Any | None = None,
    tenant_resolver: TenantResolver | None = None,
) -> None:
    """Register the SPEC-W115 document endpoints. `ocr_client` and
    `tenant_resolver` are injectable for tests; production wiring (in
    control_plane.create_app) passes the real httpx client and the
    tenant_context-based resolver."""

    if ocr_client is None:
        ocr_client = OcrDocumentClient(
            base_url=settings.ocr_service_url or "http://ocr:7020",
            internal_token=settings.ocr_internal_token,
            timeout_s=settings.ocr_timeout_s,
        )
    app.state.ocr_document_client = ocr_client

    def _ocr_ready() -> bool:
        return bool(settings.ocr_service_url and settings.ocr_internal_token)

    async def _resolve_tenant(site_slug: str) -> str:
        if tenant_resolver is not None:
            return await tenant_resolver(site_slug)
        raise HTTPException(status_code=503, detail="tenant resolution unavailable")

    async def _read_caller_upload(request: Request) -> tuple[str, bytes, str, str]:
        """(filename, data, kind, mime) with fail-fast validation."""
        form = await request.form()
        upload = form.get("file")
        kind = form.get("kind")
        if upload is None or not getattr(upload, "filename", None):
            raise HTTPException(status_code=400, detail="multipart field 'file' required")
        if kind not in CALLER_DOCUMENT_KINDS:
            raise HTTPException(
                status_code=400,
                detail=f"kind must be one of {list(CALLER_DOCUMENT_KINDS)}",
            )
        data = await upload.read()
        if not data:
            raise HTTPException(status_code=400, detail="empty file")
        if len(data) > settings.max_document_bytes:
            raise HTTPException(status_code=413, detail="file too large")
        mime = sniff_mime(data)
        if mime is None:
            raise HTTPException(
                status_code=415,
                detail="unsupported file type (jpeg, png, webp or pdf only)",
            )
        return upload.filename, data, kind, mime

    async def _attach_to_session(session_id: str, entry: dict[str, Any]) -> bool:
        """Append the shared-document ref to the session (fail-open on
        save errors — the OCR row is the durable record)."""
        session = await store_get(sessions, session_id)
        if session is None:
            return False
        session.shared_documents.append(entry)
        session.touch()
        try:
            await store_save_session(sessions, session)
        except Exception as exc:  # noqa: BLE001
            log.warning("documents.session_save_failed", error=str(exc)[:200])
        return True

    @app.post("/voice/documents/upload")
    async def upload_caller_document(request: Request) -> dict[str, Any]:
        """Web chat attachment intake (public; APISIX limit-count fronted)."""
        if not _ocr_ready():
            raise HTTPException(status_code=503, detail="document intake not configured")
        form = await request.form()
        site_slug = form.get("site_slug")
        if not isinstance(site_slug, str) or not site_slug:
            raise HTTPException(status_code=400, detail="site_slug required")
        filename, data, kind, mime = await _read_caller_upload(request)
        tenant_id = await _resolve_tenant(site_slug)
        try:
            result = await ocr_client.extract(
                tenant_id=tenant_id, kind=kind, filename=filename,
                data=data, mime=mime, source_channel="web",
            )
        except OcrUpstreamError as exc:
            log.warning("documents.ocr_upstream_failed", status=exc.status_code)
            raise HTTPException(status_code=502, detail="document analysis failed") from exc
        verdict = result.get("validation_verdict") or "needs_review"
        reasons = result.get("validation_reasons") or []
        session_id = form.get("session_id")
        if isinstance(session_id, str) and session_id:
            await _attach_to_session(session_id, {
                "document_id": result.get("document_id"),
                "kind": kind,
                "validation_verdict": verdict,
                "validation_reasons": reasons,
                "channel": "web",
                "shared_at": time.time(),
            })
        return {
            "document_id": result.get("document_id"),
            "kind": kind,
            "validation_verdict": verdict,
            "validation_reasons": reasons,
            "summary": verdict_summary(kind, verdict, reasons),
        }

    @app.post("/voice/sessions/{session_id}/document-link")
    async def mint_link(session_id: str, request: Request) -> dict[str, Any]:
        """Mint an expiring caller share link for a live session. The
        session secret (K15(b) resume credential) authorizes the mint —
        a public caller who only knows the conversation id cannot attach
        documents to someone else's session."""
        if not (_ocr_ready() and settings.document_link_secret):
            raise HTTPException(status_code=503, detail="document sharing not configured")
        session = await store_get(sessions, session_id)
        if session is None:
            raise HTTPException(status_code=404, detail="session not found")
        try:
            body = await request.json()
        except Exception:
            body = {}
        secret = (body or {}).get("session_secret") or request.headers.get("X-Session-Secret")
        if not session.check_secret(secret):
            raise HTTPException(status_code=401, detail="invalid session secret")
        tenant_id = await _resolve_tenant(session.site_slug)
        token, exp = mint_document_link(
            secret=settings.document_link_secret,
            session_id=session_id,
            tenant_id=tenant_id,
            ttl_s=settings.document_link_ttl_s,
        )
        return {
            "upload_url": f"/voice/documents/claim/{token}",
            "expires_at": exp,
            "expires_in_s": settings.document_link_ttl_s,
        }

    @app.post("/voice/documents/claim/{token}")
    async def claim_link(token: str, request: Request) -> dict[str, Any]:
        """Caller upload via a share link (public; the token IS the auth)."""
        if not (_ocr_ready() and settings.document_link_secret):
            raise HTTPException(status_code=503, detail="document sharing not configured")
        try:
            payload = verify_document_link(
                secret=settings.document_link_secret, token=token
            )
        except LinkTokenError as exc:
            raise HTTPException(status_code=401, detail=str(exc)) from exc
        filename, data, kind, mime = await _read_caller_upload(request)
        session_id = payload["sid"]
        if await store_get(sessions, session_id) is None:
            # honest: the whole point of the link is session correlation
            raise HTTPException(status_code=404, detail="session not found")
        try:
            result = await ocr_client.extract(
                tenant_id=payload["t"], kind=kind, filename=filename,
                data=data, mime=mime, source_channel="voice",
            )
        except OcrUpstreamError as exc:
            log.warning("documents.ocr_upstream_failed", status=exc.status_code)
            raise HTTPException(status_code=502, detail="document analysis failed") from exc
        verdict = result.get("validation_verdict") or "needs_review"
        reasons = result.get("validation_reasons") or []
        await _attach_to_session(session_id, {
            "document_id": result.get("document_id"),
            "kind": kind,
            "validation_verdict": verdict,
            "validation_reasons": reasons,
            "channel": "voice",
            "shared_at": time.time(),
        })
        return {
            "document_id": result.get("document_id"),
            "kind": kind,
            "validation_verdict": verdict,
            "validation_reasons": reasons,
            "summary": verdict_summary(kind, verdict, reasons),
        }

    @app.get("/voice/sessions/{session_id}/documents")
    async def list_session_documents(session_id: str, request: Request) -> dict[str, Any]:
        """Agent/staff view of a session's shared documents (session-secret
        authorized — the list carries extracted verdicts, i.e. caller PII
        class data, so it is never public-by-id)."""
        session = await store_get(sessions, session_id)
        if session is None:
            raise HTTPException(status_code=404, detail="session not found")
        secret = request.headers.get("X-Session-Secret")
        if not session.check_secret(secret):
            raise HTTPException(status_code=401, detail="invalid session secret")
        return {"documents": [dict(d) for d in session.shared_documents]}
