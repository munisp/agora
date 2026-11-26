/**
 * A thin strip saying a business is a demonstration (e.g. "Demo prepared for Leadway by Agora"), shown on its public
 * page and dashboard. Driven by the server env DEMO_BANNERS — JSON {"<slug>": "<text>"} — so a demo is marked without
 * a code change and unmarked by removing its entry. Renders nothing for every other business.
 */
export function demoBannerText(slug: string): string | null {
  try {
    const map = JSON.parse(process.env.DEMO_BANNERS || "{}") as Record<string, unknown>;
    const text = map[slug];
    return typeof text === "string" && text.trim() ? text.trim() : null;
  } catch {
    return null;
  }
}

export function DemoBanner({ slug }: { slug: string }) {
  const text = demoBannerText(slug);
  if (!text) return null;
  return (
    <div
      role="note"
      className="w-full px-4 py-1.5 text-center text-xs font-medium tracking-wide"
      style={{ backgroundColor: "#c9a574", color: "#2b2119" }}
    >
      {text}
    </div>
  );
}
