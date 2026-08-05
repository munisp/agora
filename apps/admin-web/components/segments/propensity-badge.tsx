/**
 * SPEC-W29 WS-C / SPEC-W30 WS-D: propensity & risk score chips for the
 * Person 360 header.
 *
 * Color ramp is strictly sage (low) → amber (mid) → terracotta (high) —
 * never red/blue (brand rule). Scores are 0..1 (SPEC-W29 §2, SPEC-W30 §2);
 * absent scores (pre-sweep tenants) render nothing rather than a fake zero.
 */
import { BRAND } from "./types";

export type ScoreTone = "low" | "mid" | "high";

/** Magnitude ramp shared by every score chip (thirds of the 0..1 range). */
export function scoreTone(score: number): ScoreTone {
  if (score >= 2 / 3) return "high";
  if (score >= 1 / 3) return "mid";
  return "low";
}

/** Chip colors per tone — soft fill, solid ink-ish text, matching border. */
export const TONE_COLORS: Record<
  ScoreTone,
  { fg: string; bg: string; border: string; solid: string }
> = {
  low: { fg: BRAND.sage, bg: `${BRAND.sage}1f`, border: `${BRAND.sage}55`, solid: BRAND.sage },
  mid: { fg: "#a8762f", bg: `${BRAND.amber}26`, border: `${BRAND.amber}66`, solid: BRAND.amber },
  high: {
    fg: BRAND.terracotta,
    bg: `${BRAND.terracotta}1a`,
    border: `${BRAND.terracotta}59`,
    solid: BRAND.terracotta,
  },
};

export function formatScore(score: number): string {
  return `${Math.round(score * 100)}%`;
}

/**
 * One score chip: label + percentage, colored by magnitude. `title`
 * explains what the score means (scores are data, not authority —
 * SPEC-W29 §0.3).
 */
export function PropensityBadge({
  label,
  score,
  title,
}: {
  label: string;
  score: number;
  title?: string;
}) {
  const tone = TONE_COLORS[scoreTone(score)];
  return (
    <span
      title={title ?? `${label}: ${formatScore(score)}`}
      className="inline-flex items-center gap-1.5 rounded-full border px-2.5 py-0.5 text-xs font-medium"
      style={{
        color: tone.fg,
        backgroundColor: tone.bg,
        borderColor: tone.border,
      }}
    >
      <span
        aria-hidden
        className="inline-block h-1.5 w-1.5 rounded-full"
        style={{ backgroundColor: tone.solid }}
      />
      {label} {formatScore(score)}
    </span>
  );
}

/**
 * The W29 propensity chip row (churn / convert / turnout). Only scores
 * actually present on the person render — a tenant whose sweep has not run
 * yet shows no chips at all.
 */
export function PropensityBadges({
  churn,
  convert,
  turnout,
}: {
  churn?: number;
  convert?: number;
  turnout?: number;
}) {
  if (churn === undefined && convert === undefined && turnout === undefined) {
    return null;
  }
  return (
    <div className="flex flex-wrap items-center gap-2" aria-label="Propensity scores">
      {churn !== undefined ? (
        <PropensityBadge
          label="Churn"
          score={churn}
          title="Likelihood this person lapses (no booking within the tenant-typical interval)."
        />
      ) : null}
      {convert !== undefined ? (
        <PropensityBadge
          label="Convert"
          score={convert}
          title="Likelihood this person books from an outreach touch."
        />
      ) : null}
      {turnout !== undefined ? (
        <PropensityBadge
          label="Turnout"
          score={turnout}
          title="Campaign tenants: likelihood this person shows up / votes when contacted."
        />
      ) : null}
    </div>
  );
}
