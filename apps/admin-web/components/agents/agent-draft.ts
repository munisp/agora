import type {
  Agent,
  AgentDefinition,
  AgentStatus,
  CaptureField,
} from "@/lib/types";

/**
 * SPEC-W38 — shared wizard/edit state for the agents section. The draft is a
 * flat, form-friendly shape; `definitionFromDraft` projects it onto the
 * declarative agent definition (F2) sent to conversation-service.
 */

/** Built-in tool names the voice runtime exposes (same list as voice-agent). */
export const AGENT_TOOLS = [
  "get_business_info",
  "get_availability",
  "book_appointment",
  "lookup_appointment",
  "reschedule_appointment",
  "cancel_appointment",
] as const;

/** Curated Piper voices (SPEC-W38 F4) — merged with GET /voice/voices. */
export const STATIC_VOICES: { id: string; language: string }[] = [
  { id: "en_US-lessac-medium", language: "en" },
  { id: "en_US-amy-medium", language: "en" },
  { id: "en_GB-alan-medium", language: "en" },
  { id: "en_US-ryan-high", language: "en" },
];

/** E.164: leading +, non-zero first digit, 7–15 digits total. */
export const E164_RE = /^\+[1-9]\d{6,14}$/;

export interface AgentDraft {
  name: string;
  slug: string;
  purpose: string;
  phoneNumber: string;
  persona: string;
  voiceId: string;
  language: string;
  /** empty = all built-in tools allowed (no allowlist filter) */
  toolAllowlist: string[];
  knowledgePacks: string[];
  captureSchemaName: string;
  captureFields: CaptureField[];
  status: AgentStatus;
}

export const EMPTY_DRAFT: AgentDraft = {
  name: "",
  slug: "",
  purpose: "",
  phoneNumber: "",
  persona: "",
  voiceId: STATIC_VOICES[0].id,
  language: "en",
  toolAllowlist: [],
  knowledgePacks: [],
  captureSchemaName: "Call capture",
  captureFields: [],
  status: "active",
};

export function slugify(name: string): string {
  return name
    .toLowerCase()
    .trim()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 64);
}

/** Project the draft onto the F2 declarative definition (empty keys omitted). */
export function definitionFromDraft(draft: AgentDraft): AgentDefinition {
  const definition: AgentDefinition = {};
  if (draft.persona.trim()) definition.persona = draft.persona.trim();
  if (draft.voiceId) {
    definition.voice = {
      provider: "piper",
      voice_id: draft.voiceId,
      language: draft.language || "en",
    };
  }
  if (draft.toolAllowlist.length > 0)
    definition.tool_allowlist = [...draft.toolAllowlist];
  if (draft.knowledgePacks.length > 0)
    definition.knowledge_packs = [...draft.knowledgePacks];
  return definition;
}

/** Request body for POST /v1/agents (pinned contract §2). */
export function createBodyFromDraft(draft: AgentDraft) {
  return {
    name: draft.name.trim(),
    ...(draft.slug.trim() ? { slug: draft.slug.trim() } : {}),
    ...(draft.purpose.trim() ? { purpose: draft.purpose.trim() } : {}),
    ...(draft.phoneNumber.trim()
      ? { phone_number: draft.phoneNumber.trim() }
      : {}),
    definition: definitionFromDraft(draft),
  };
}

/** Request body for PATCH /v1/agents/{id}. */
export function patchBodyFromDraft(draft: AgentDraft) {
  return {
    name: draft.name.trim(),
    purpose: draft.purpose.trim(),
    phone_number: draft.phoneNumber.trim() || null,
    status: draft.status,
    definition: definitionFromDraft(draft),
  };
}

/** Hydrate the edit form from an existing agent (+ its capture schema). */
export function draftFromAgent(
  agent: Agent,
  captureFields: CaptureField[] = [],
  captureSchemaName = "Call capture",
): AgentDraft {
  const def = agent.definition ?? {};
  return {
    name: agent.name ?? "",
    slug: agent.slug ?? "",
    purpose: agent.purpose ?? "",
    phoneNumber: agent.phone_number ?? "",
    persona: def.persona ?? "",
    voiceId: def.voice?.voice_id ?? STATIC_VOICES[0].id,
    language: def.voice?.language ?? "en",
    toolAllowlist: def.tool_allowlist ?? [],
    knowledgePacks: def.knowledge_packs ?? [],
    captureSchemaName,
    captureFields,
    status: agent.status ?? "active",
  };
}

/**
 * List endpoints may return a bare array or a keyed envelope — accept both so
 * the UI keeps working while the conversation-service contract settles.
 */
export function unwrapList<T>(data: unknown, ...keys: string[]): T[] {
  if (Array.isArray(data)) return data as T[];
  if (data && typeof data === "object") {
    for (const key of keys) {
      const value = (data as Record<string, unknown>)[key];
      if (Array.isArray(value)) return value as T[];
    }
  }
  return [];
}
