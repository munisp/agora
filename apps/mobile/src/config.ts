/**
 * App configuration, sourced from app.json `expo.extra` via expo-constants.
 * Operators override these per environment (EAS build profiles / app config
 * — see README "Configuration").
 */
import Constants from "expo-constants";

/** React Native global: true in development builds. Declared here because
 * the repo typechecks without the Expo/RN type tree (see
 * src/types/expo-shims.d.ts). */
declare const __DEV__: boolean;

/**
 * SPEC-W45 K25: release builds fail CLOSED on cleartext endpoints. An
 * http:// API base or Keycloak issuer ships tokens and customer data in
 * the clear, so config load throws unless this is a dev build (__DEV__).
 */
function assertSecureUrl(kind: string, url: string): string {
  // Fail-closed: only an explicitly-dev build (__DEV__ === true) may use
  // cleartext; an undefined __DEV__ is treated as a release build.
  const isDev = typeof __DEV__ !== "undefined" && __DEV__;
  if (!isDev && url.toLowerCase().startsWith("http://")) {
    throw new Error(
      `[config] ${kind} must use https:// in release builds (got ${url}). ` +
        "Set a TLS endpoint via app.json expo.extra / EAS build profile.",
    );
  }
  return url;
}

export interface KeycloakConfig {
  issuer: string;
  clientId: string;
  scopes: string[];
}

interface ExtraShape {
  apiBase?: unknown;
  keycloak?: {
    issuer?: unknown;
    clientId?: unknown;
    scopes?: unknown;
  };
}

function extra(): ExtraShape {
  const cfg = Constants.expoConfig as { extra?: ExtraShape } | null | undefined;
  return (cfg && cfg.extra) || {};
}

/**
 * Base URL of the booking-service BFF as exposed by the APISIX gateway,
 * e.g. "https://gw.example.com/api/bookings". Mirrors the admin-web
 * convention (API_BASE_URL → gateway → /api/bookings/* routes).
 */
export function apiBase(): string {
  const v = extra().apiBase;
  const url =
    typeof v === "string" && v.length > 0
      ? v.replace(/\/+$/, "")
      : "http://localhost:9080/api/bookings";
  return assertSecureUrl("apiBase", url);
}

export function keycloakConfig(): KeycloakConfig {
  const k = extra().keycloak || {};
  return {
    issuer: assertSecureUrl(
      "keycloak.issuer",
      typeof k.issuer === "string" && k.issuer.length > 0
        ? k.issuer.replace(/\/+$/, "")
        : "http://localhost:8080/realms/opendesk",
    ),
    clientId:
      typeof k.clientId === "string" && k.clientId.length > 0
        ? k.clientId
        : "opendesk-field",
    scopes: Array.isArray(k.scopes)
      ? k.scopes.filter((s): s is string => typeof s === "string")
      : ["openid", "profile", "email", "offline_access"],
  };
}
