/**
 * OpenDesk mobile theme tokens — a 1:1 mirror of the admin-web palette
 * (apps/admin-web/app/globals.css): low-saturation warm neutrals with warm
 * umber accents, flat surfaces, no gradients.
 */
export const colors = {
  background: "#faf7f1",
  foreground: "#2e2a25",

  card: "#ffffff",
  cardForeground: "#2e2a25",

  muted: "#f2ecdf",
  mutedForeground: "#6e6558",

  primary: "#7c5b3e",
  primaryForeground: "#faf7f1",
  primaryHover: "#6b4e35",

  secondary: "#e9e1d0",
  secondaryForeground: "#4a4237",

  accent: "#efe7d7",
  accentForeground: "#4a4237",

  border: "#e3dac8",
  input: "#d9cfbc",
  ring: "#a98d68",

  destructive: "#9e4a38",
  destructiveForeground: "#faf7f1",

  success: "#5e7a52",
  successSoft: "#e4ead9",
  warning: "#96762c",
  warningSoft: "#f0e7cf",
  info: "#54697a",
  infoSoft: "#dfe5e8",
  dangerSoft: "#f0ded8",
} as const;

export const radius = {
  sm: 6,
  md: 8,
  lg: 12,
  xl: 16,
} as const;

export const spacing = {
  xs: 4,
  sm: 8,
  md: 12,
  lg: 16,
  xl: 24,
} as const;

export type BadgeTone = "success" | "warning" | "info" | "secondary" | "destructive";

/** Soft background + strong foreground pair for a status badge tone. */
export function toneColors(tone: BadgeTone): { bg: string; fg: string } {
  switch (tone) {
    case "success":
      return { bg: colors.successSoft, fg: colors.success };
    case "warning":
      return { bg: colors.warningSoft, fg: colors.warning };
    case "info":
      return { bg: colors.infoSoft, fg: colors.info };
    case "destructive":
      return { bg: colors.dangerSoft, fg: colors.destructive };
    default:
      return { bg: colors.secondary, fg: colors.secondaryForeground };
  }
}
