import { cn } from "@/lib/utils";

/**
 * Agora's mark: a doorway arch (the agora, a gathering place) with a figure being welcomed in.
 * `tone` "dark" is for light backgrounds, "light" for photos and the umber band.
 */
export function AgoraMark({ className, tone = "dark" }: { className?: string; tone?: "dark" | "light" }) {
  const ground = tone === "dark" ? "var(--color-umber)" : "var(--color-paper)";
  const line = tone === "dark" ? "var(--color-paper)" : "var(--color-umber)";
  return (
    <svg viewBox="0 0 40 40" className={cn("h-9 w-9", className)} role="img" aria-label="Agora">
      <rect width="40" height="40" rx="10" fill={ground} />
      <path d="M11 31V19a9 9 0 0 1 18 0v12" fill="none" stroke={line} strokeWidth="2.6" strokeLinecap="round" />
      <circle cx="20" cy="20" r="3" fill="var(--color-tan)" />
      <path d="M15.5 31c.6-3.6 2.3-5.5 4.5-5.5s3.9 1.9 4.5 5.5" fill="var(--color-tan)" />
    </svg>
  );
}

export function AgoraWordmark({ className, tone = "dark" }: { className?: string; tone?: "dark" | "light" }) {
  return (
    <span className={cn("inline-flex items-center gap-2.5", className)}>
      <AgoraMark tone={tone} />
      <span
        className={cn("font-display text-2xl leading-none tracking-tight", tone === "dark" ? "text-umber" : "text-paper")}
      >
        Agora
      </span>
    </span>
  );
}
