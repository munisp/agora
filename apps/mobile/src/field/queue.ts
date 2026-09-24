/**
 * Offline field-capture queue (SPEC-W46 MB-6).
 *
 * The lead-capture modal used to fail hard when the device was offline —
 * field staff lost captures on poor networks. This module persists queued
 * captures locally and flushes them through the SAME server contract as
 * the field PWA: POST /v1/field/capture with
 * {items:[{client_id, kind, payload, captured_at, gps}]}, deduped
 * server-side on field_capture:{client_id} (contract §4) so any retry of
 * the same flush applies each capture exactly once.
 *
 * Storage: expo-secure-store — the storage layer this app already ships
 * (@react-native-async-storage/async-storage is NOT in package.json and
 * new deps are out of scope). SecureStore values are capped at 2048 bytes
 * on Android (EncryptedSharedPreferences), so each item lives under its
 * own key (a lead payload is a few hundred bytes) and the id index is
 * bounded by QUEUE_MAX — 40 uuids ≈ 1.6 KB, safely under the cap. The
 * queue refuses new items when full rather than silently dropping data;
 * the UI surfaces that honestly.
 *
 * NOT credentials: queued payloads are contact captures, stored with the
 * same protection as the app's other field data. Tokens never touch this
 * module.
 */
import * as SecureStore from "expo-secure-store";
import * as Crypto from "expo-crypto";
import { submitFieldCapture } from "../api/client";
import type { FieldCaptureItem, GpsPoint } from "../api/types";

const INDEX_KEY = "opendesk.field_queue.index";
const ITEM_PREFIX = "opendesk.field_queue.item.";

/** 40 uuids ≈ 1.6 KB of index JSON — under SecureStore's 2048-byte cap. */
export const QUEUE_MAX = 40;

/** Flush chunks — the server caps one batch at 100 items; 50 matches the
 * field PWA's chunk size and stays well inside gateway body limits. */
const FLUSH_CHUNK = 50;

export type QueuedCaptureKind = "lead_capture" | "checkin";

export interface QueuedCapture {
  client_id: string;
  kind: QueuedCaptureKind;
  payload: Record<string, unknown>;
  captured_at: string; // RFC3339 — set at capture time, preserved across flushes
  gps: GpsPoint | null;
}

export class QueueFullError extends Error {
  constructor() {
    super(
      `Offline queue is full (${QUEUE_MAX} captures). Connect to sync before capturing more.`,
    );
    this.name = "QueueFullError";
  }
}

async function readIndex(): Promise<string[]> {
  const raw = await SecureStore.getItemAsync(INDEX_KEY);
  if (!raw) return [];
  try {
    const ids = JSON.parse(raw) as unknown;
    return Array.isArray(ids) ? ids.filter((x): x is string => typeof x === "string") : [];
  } catch {
    return [];
  }
}

async function writeIndex(ids: string[]): Promise<void> {
  if (ids.length === 0) {
    await SecureStore.deleteItemAsync(INDEX_KEY);
    return;
  }
  await SecureStore.setItemAsync(INDEX_KEY, JSON.stringify(ids));
}

/** Enqueue one capture; the client_id idempotency anchor is minted here
 * and NEVER changes across flushes/retries (exactly-once on the server). */
export async function enqueueCapture(
  kind: QueuedCaptureKind,
  payload: Record<string, unknown>,
  gps: GpsPoint | null = null,
): Promise<QueuedCapture> {
  const ids = await readIndex();
  if (ids.length >= QUEUE_MAX) throw new QueueFullError();
  const item: QueuedCapture = {
    client_id: Crypto.randomUUID(),
    kind,
    payload,
    captured_at: new Date().toISOString(),
    gps,
  };
  await SecureStore.setItemAsync(ITEM_PREFIX + item.client_id, JSON.stringify(item));
  await writeIndex([...ids, item.client_id]);
  return item;
}

/** All queued captures in capture order (oldest first). */
export async function listQueue(): Promise<QueuedCapture[]> {
  const ids = await readIndex();
  if (ids.length === 0) return [];
  const raws = await Promise.all(
    ids.map((id) => SecureStore.getItemAsync(ITEM_PREFIX + id)),
  );
  const items: QueuedCapture[] = [];
  const alive: string[] = [];
  for (let i = 0; i < ids.length; i++) {
    const raw = raws[i];
    const id = ids[i];
    if (!raw || !id) continue;
    try {
      items.push(JSON.parse(raw) as QueuedCapture);
      alive.push(id);
    } catch {
      // Corrupt entry — drop it from the index so it can't wedge the queue.
      await SecureStore.deleteItemAsync(ITEM_PREFIX + id).catch(() => {});
    }
  }
  if (alive.length !== ids.length) await writeIndex(alive);
  return items;
}

export async function queueLength(): Promise<number> {
  return (await readIndex()).length;
}

async function removeQueued(clientIds: string[]): Promise<void> {
  if (clientIds.length === 0) return;
  const drop = new Set(clientIds);
  await Promise.all(
    clientIds.map((id) =>
      SecureStore.deleteItemAsync(ITEM_PREFIX + id).catch(() => {}),
    ),
  );
  const ids = await readIndex();
  await writeIndex(ids.filter((id) => !drop.has(id)));
}

export interface FlushReport {
  /** Items the server applied or had already deduped (now removed). */
  delivered: number;
  /** Items the server rejected deterministically (validation errors —
   * they stay queued so the user can see/discard them, never silently
   * dropped). */
  rejected: number;
  /** Items still queued after this attempt (rejected + unreached). */
  remaining: number;
  /** True when every queued item was delivered. */
  synced: boolean;
}

/**
 * Flush the queue in chunks of FLUSH_CHUNK, sequentially (array order is
 * capture order — the server applies items in array order, and chunks
 * preserve it). A failed chunk (network/timeout/5xx) aborts the flush and
 * keeps its items plus everything after it queued; delivered items from
 * earlier chunks are already removed. Per-item "error" results stay
 * queued (honest rejected count) — everything else is exactly-once on
 * client_id, so re-flushing is always safe.
 */
export async function flushQueue(): Promise<FlushReport> {
  const items = await listQueue();
  if (items.length === 0) {
    return { delivered: 0, rejected: 0, remaining: 0, synced: true };
  }
  let delivered = 0;
  let rejected = 0;
  for (let off = 0; off < items.length; off += FLUSH_CHUNK) {
    const chunk = items.slice(off, off + FLUSH_CHUNK);
    const reqItems: FieldCaptureItem[] = chunk.map((i) => ({
      client_id: i.client_id,
      kind: i.kind,
      payload: i.payload,
      captured_at: i.captured_at,
      gps: i.gps,
    }));
    // A throw here (network/timeout/5xx/401) aborts the flush: everything
    // from this chunk on stays queued, and the error propagates so the UI
    // reports the sync failure honestly.
    const results = await submitFieldCapture({ items: reqItems });
    const deliveredIds: string[] = [];
    if (results.length === 0) {
      // No per-item detail (unexpected but safe): the batch returned 200,
      // so every item was accepted — remove the whole chunk.
      for (const i of chunk) deliveredIds.push(i.client_id);
    } else {
      const byId = new Map(results.map((r) => [r.client_id, r]));
      for (const i of chunk) {
        const r = byId.get(i.client_id);
        if (r && (r.status === "applied" || r.status === "deduped")) {
          deliveredIds.push(i.client_id);
        } else {
          rejected += 1;
        }
      }
    }
    await removeQueued(deliveredIds);
    delivered += deliveredIds.length;
  }
  const remaining = await queueLength();
  return { delivered, rejected, remaining, synced: remaining === 0 };
}
