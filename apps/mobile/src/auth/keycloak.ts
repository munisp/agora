/**
 * Keycloak sign-in via expo-auth-session (OpenID Connect Authorization Code
 * + PKCE — no client secret on device).
 *
 * ASSUMPTION: the discovery URL is derived from the configured issuer as
 * `${issuer}/.well-known/openid-configuration`. The repo's realm is
 * "opendesk" (docker-compose keycloak service); the exact realm name and
 * the public client id for this app ("opendesk-field", public client with
 * PKCE S256 enforced and redirect scheme opendesk-field://*) must be
 * provisioned in Keycloak — see README "Keycloak client setup". All URLs
 * come from app config (app.json extra.keycloak), never hardcoded per
 * environment.
 */
import * as AuthSession from "expo-auth-session";
import * as WebBrowser from "expo-web-browser";
import { keycloakConfig } from "../config";

// Lets the auth-session browser close itself after the redirect (Android).
WebBrowser.maybeCompleteAuthSession();

export interface SignInResult {
  accessToken: string;
  refreshToken: string | null;
  idToken: string | null;
  expiresIn: number | null;
  email: string | null;
}

function discoveryUrl(issuer: string): string {
  return `${issuer}/.well-known/openid-configuration`;
}

/** The in-app redirect URI, e.g. opendesk-field://auth/callback. */
export function redirectUri(): string {
  return AuthSession.makeRedirectUri({ path: "auth/callback" });
}

interface JwtClaims {
  email?: string;
  preferred_username?: string;
}

const B64_CHARS =
  "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";

/**
 * Minimal base64(url) decoder. Hermes does not guarantee global atob, so
 * we decode locally rather than polyfilling.
 */
function base64Decode(input: string): string {
  const b64 = input.replace(/-/g, "+").replace(/_/g, "/");
  let out = "";
  let buffer = 0;
  let bits = 0;
  for (const ch of b64) {
    if (ch === "=") break;
    const val = B64_CHARS.indexOf(ch);
    if (val < 0) continue;
    buffer = (buffer << 6) | val;
    bits += 6;
    if (bits >= 8) {
      bits -= 8;
      out += String.fromCharCode((buffer >> bits) & 0xff);
    }
  }
  // JWT payloads are ASCII/UTF-8 JSON; decode UTF-8 percent-style.
  try {
    return decodeURIComponent(
      out
        .split("")
        .map((c) => "%" + ("00" + c.charCodeAt(0).toString(16)).slice(-2))
        .join(""),
    );
  } catch {
    return out;
  }
}

/** Decode (not verify — verification happens server-side) a JWT payload. */
function decodeClaims(jwt: string | null): JwtClaims {
  if (!jwt) return {};
  const parts = jwt.split(".");
  const payload = parts[1];
  if (!payload) return {};
  try {
    return JSON.parse(base64Decode(payload)) as JwtClaims;
  } catch {
    return {};
  }
}

/**
 * Interactive sign-in: opens the Keycloak login page in the system browser
 * (ASWebAuthenticationSession / Custom Tabs), completes the PKCE exchange,
 * and returns the token set. Throws on cancel/deny or endpoint failure.
 */
export async function signInWithKeycloak(): Promise<SignInResult> {
  const cfg = keycloakConfig();
  // ASSUMPTION: standard OIDC discovery path (see header note).
  const discovery = await AuthSession.fetchDiscoveryAsync(discoveryUrl(cfg.issuer));

  const request = new AuthSession.AuthRequest({
    clientId: cfg.clientId,
    redirectUri: redirectUri(),
    scopes: cfg.scopes,
    responseType: AuthSession.ResponseType.Code,
    usePKCE: true,
    codeChallengeMethod: AuthSession.CodeChallengeMethod.S256,
  });

  const result = await request.promptAsync(discovery);
  if (result.type !== "success") {
    throw new Error(
      result.type === "cancel" || result.type === "dismiss"
        ? "Sign-in cancelled"
        : `Sign-in failed (${result.type})`,
    );
  }

  const code = result.params.code;
  if (!code) throw new Error("Sign-in failed (no authorization code returned)");

  const tokenResponse = await AuthSession.exchangeCodeAsync(
    {
      clientId: cfg.clientId,
      code,
      redirectUri: redirectUri(),
      extraParams: request.codeVerifier
        ? { code_verifier: request.codeVerifier }
        : undefined,
    },
    discovery,
  );

  const claims = decodeClaims(tokenResponse.idToken ?? null);
  return {
    accessToken: tokenResponse.accessToken,
    refreshToken: tokenResponse.refreshToken ?? null,
    idToken: tokenResponse.idToken ?? null,
    expiresIn: tokenResponse.expiresIn ?? null,
    email: claims.email ?? claims.preferred_username ?? null,
  };
}

/**
 * Refresh the access token using the stored refresh token. Returns null
 * when there is no refresh token or the grant is dead (caller re-logins).
 */
export async function refreshAccessToken(refreshToken: string): Promise<{
  accessToken: string;
  refreshToken: string | null;
  expiresIn: number | null;
} | null> {
  try {
    const cfg = keycloakConfig();
    const discovery = await AuthSession.fetchDiscoveryAsync(discoveryUrl(cfg.issuer));
    const res = await AuthSession.refreshAsync(
      { clientId: cfg.clientId, refreshToken },
      discovery,
    );
    return {
      accessToken: res.accessToken,
      refreshToken: res.refreshToken ?? refreshToken,
      expiresIn: res.expiresIn ?? null,
    };
  } catch {
    return null;
  }
}

/** Best-effort token revocation on sign-out (never throws). */
export async function revokeTokens(tokens: Array<string | null>): Promise<void> {
  const cfg = keycloakConfig();
  try {
    const discovery = await AuthSession.fetchDiscoveryAsync(discoveryUrl(cfg.issuer));
    for (const token of tokens) {
      if (!token) continue;
      try {
        await AuthSession.revokeAsync(
          { clientId: cfg.clientId, token },
          discovery,
        );
      } catch {
        // revocation is best-effort
      }
    }
  } catch {
    // discovery unreachable — local sign-out still proceeds
  }
}
