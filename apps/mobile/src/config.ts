/**
 * App configuration, sourced from app.json `expo.extra` via expo-constants.
 * Operators override these per environment (EAS build profiles / app config
 * — see README "Configuration").
 */
import Constants from "expo-constants";

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
  if (typeof v === "string" && v.length > 0) return v.replace(/\/+$/, "");
  return "http://localhost:9080/api/bookings";
}

export function keycloakConfig(): KeycloakConfig {
  const k = extra().keycloak || {};
  return {
    issuer:
      typeof k.issuer === "string" && k.issuer.length > 0
        ? k.issuer.replace(/\/+$/, "")
        : "http://localhost:8080/realms/opendesk",
    clientId:
      typeof k.clientId === "string" && k.clientId.length > 0
        ? k.clientId
        : "opendesk-field",
    scopes: Array.isArray(k.scopes)
      ? k.scopes.filter((s): s is string => typeof s === "string")
      : ["openid", "profile", "email", "offline_access"],
  };
}
