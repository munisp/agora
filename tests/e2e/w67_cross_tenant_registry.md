# W67 e2e — Cross-tenant document-fraud registry (manual script)

Manual end-to-end verification for SPEC-W67. The automated battery
(`services/ocr-service/tests/test_w67_zero_disclosure.py`,
`services/fraud-engine/tests/test_w67_s4_disclosure.py`) covers the
contract gates against fakes; THIS script is the live-stack walkthrough
(two real tenants, real Postgres/FalkorDB/Kafka). Environment-blocked items
in CI (live Kafka round-trip, live Neo4j/FalkorDB, docker) are expected to
be run by an operator on a compose host.

Conventions: `$A` / `$B` are two tenant uuids (`$A` = earlier tenant,
`$B` = later tenant). `OCR=http://localhost:7020`,
`FRAUD=http://localhost:7017`. All internal calls use `$OCR_INTERNAL_TOKEN`
/ `$FRAUD_INTERNAL_TOKEN`.

## 0. Prerequisites

1. Stack up (docker compose) with Postgres, Kafka, graph-sync,
   fraud-engine, ocr-service.
2. **PRE-DEPLOY GATE (must be checked off before the secret exists in any
   real environment):** counsel confirmation of the NDPA lawful basis
   (legitimate interest / security of processing) + DPIA note — see
   `docs/compliance-ndpa.md` "Lawful basis — PRE-DEPLOY gate".
3. Set `OCR_REGISTRY_HMAC_SECRET` on ocr-service (a real random secret;
   never reuse `KYC_HASH_SECRET`). Without it the registry is DISABLED
   (one startup log line, upserts skipped) and steps 2+ observe nothing —
   that IS the fail-closed posture check (step 0.4).
4. Fail-closed check: with the secret UNSET, run steps 1-2 below and
   observe **no** `CrossTenantDocumentMatch` event on the topic and no
   `seen_elsewhere_count` on any Document node. Then set the secret and
   restart ocr-service.
5. A test document image: `doc.png` (any PNG; the SAME file bytes are the
   cross-tenant signal).

## 1. Earlier tenant uploads (first-use)

```bash
curl -s -X POST $OCR/v1/ocr/extract \
  -H "X-Internal-Token: $OCR_INTERNAL_TOKEN" \
  -F "file=@doc.png" -F "kind=document" -F "tenant_id=$A" | tee a.json
```

Expected: `200`, `status=extracted`. **No**
`com.opendesk.ocr.CrossTenantDocumentMatch` on `opendesk.ocr.events`
(first-use-wins: the first tenant is never alerted):

```bash
kafka-console-consumer --bootstrap-server localhost:9092 \
  --topic opendesk.ocr.events --from-beginning --timeout-ms 15000 \
  | grep CrossTenantDocumentMatch   # expect: NO output
```

## 2. Later tenant uploads the SAME bytes (match)

```bash
curl -s -X POST $OCR/v1/ocr/extract \
  -H "X-Internal-Token: $OCR_INTERNAL_TOKEN" \
  -F "file=@doc.png" -F "kind=document" -F "tenant_id=$B" | tee b.json
```

Expected observations:

1. **Response invariance (oracle gate):** `$B`'s response is identical in
   status, shape and content to a no-match upload — diff `$B`'s body
   against a control upload of DIFFERENT bytes at `$B`, ignoring only the
   `document_id` value:
   ```bash
   curl -s -X POST $OCR/v1/ocr/extract -H "X-Internal-Token: $OCR_INTERNAL_TOKEN" \
     -F "file=@other.png" -F "kind=document" -F "tenant_id=$B" | tee b2.json
   python3 - <<'EOF'
   import json
   a = json.load(open('b.json')); b = json.load(open('b2.json'))
   a.pop('document_id'); b.pop('document_id')
   assert a == b, 'response must not differ match vs no-match'
   print('OK: response invariant')
   EOF
   ```
2. **Exactly ONE match event** on `opendesk.ocr.events`, type
   `com.opendesk.ocr.CrossTenantDocumentMatch`, `tenantid`/`subject` = `$B` /
   `$B`'s document id, and `data` keys EXACTLY
   `{document_id, sha256, kind, tenant_count}` with `tenant_count == 2`.
3. **Zero disclosure:** the event JSON contains NO occurrence of `$A`
   (earlier tenant uuid) and NO occurrence of `$A`'s document id:
   ```bash
   # (capture the event from kafka-console-consumer into match.json first)
   grep -c "$A" match.json && echo "LEAK" || echo "OK: no earlier-tenant id"
   ```
4. **Metrics:** `curl -s $OCR/metrics | grep registry` shows
   `registry_keys_total >= 1` and `registry_matches_total >= 1`.

## 3. Projection + S4 alert

```bash
# graph-sync consumes the match event; verify the OWN-node projection:
# (:Document {tenant_id: $B, document_id: <b doc>}).seen_elsewhere_count == 2
# (:Document {tenant_id: $A, ...}) has NO seen_elsewhere_count property.

# D9 tenant-bound sweep raises S4 for the later tenant only:
curl -s -X POST $FRAUD/v1/detect/run -H "X-Internal-Token: $FRAUD_INTERNAL_TOKEN" \
  -d '{"detector": "d9_document_abuse"}'
```

Expected: one NEW alert for `$B`: type `document_abuse`, severity `high`,
evidence `signal: "cross_tenant_exact_duplicate"`, alert id
`document_abuse:$B:none:xtenant:{sha256}`, `person_id` null. Evidence
contains ONLY `$B`'s own document id(s) + the count — grep the alert
evidence for `$A` and for `phash`: both absent. `$A` receives NO alert. No
quarantine action is taken (D9 never auto-quarantines).

## 4. Idempotency

1. Re-upload the same bytes at `$B` (re-extract): NO second match event;
   NO second alert (re-run the D9 sweep: `alerts_created` 0, deduped 1).
2. Re-deliver the match event to graph-sync (replay from Kafka or re-POST):
   processed-marker dedup — `seen_elsewhere_count` stays 2, one node.
3. Backfill re-run (§6): zero new memberships, zero new match events.

## 5. Erasure consistency

1. **Subject erasure:** (kyc kind) consent-erase the subject at `$A`
   (`com.opendesk.consent.ErasureRequested` on `opendesk.consent.erasure.v1`
   or the identity flow). The ocr row tombstones (`status=erased`, hash
   retained). Registry untouched: a THIRD tenant `$C` uploading the same
   bytes still matches with `tenant_count == 3`.
2. **TenantDelete:** delete tenant `$A` (identity tenant teardown). ONLY
   A's own membership is removed; `$B`'s membership — and therefore the
   parent fingerprint row — survives (the row stays alive while ANY
   membership remains): `$C`'s upload after this point still matches with
   `tenant_count == 2` (not 3). Then delete `$B` AND `$C` as well: with
   the LAST membership gone, the parent registry row drops at zero — a
   fourth tenant `$D` uploading the same bytes gets NO match event (no
   prior member remains).
3. Registry rows carry no subject linkage — confirm
   `ocr_doc_registry_tenants` has only `(registry_key, tenant_id,
   first_seen, alerted)`.

## 6. Backfill + rotation

1. **Backfill:** follow `docs/runbooks/security.md` §7.2 — dry_run first
   (writes/emits nothing; note `scanned`), then page to `next_cursor =
   null`. **Expect a one-time alert burst** for pre-existing cross-tenant
   duplicates (each LATER tenant alerted once per shared hash).
2. **Rotation:** follow §7.3 — new `OCR_REGISTRY_HMAC_SECRET`, wipe the
   derived registry state (GUC-gated DELETE), re-key backfill, verify
   metrics match the dry_run sizing. Open S4 alerts are refreshed in place
   (same alert_id), not duplicated.

## 7. Negative auth checks

```bash
curl -s -o /dev/null -w '%{http_code}\n' -X POST \
  $OCR/v1/internal/ocr/backfill-document-registry -d '{}'          # 401
curl -s -o /dev/null -w '%{http_code}\n' -X POST \
  $OCR/v1/internal/ocr/backfill-document-registry \
  -H 'X-Internal-Token: wrong' -d '{}'                              # 401
# With OCR_INTERNAL_TOKEN unset on the service: both become 503.
```

The route must NOT be reachable through the public APISIX gateway (no
`api-ocr` path covers `/v1/internal/*`).
