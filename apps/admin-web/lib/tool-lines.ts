/**
 * Plain-language transcript lines for the receptionist's actions. Stored tool turns look like
 * `get_availability(offering_id: 4a5d…, team_member_id: af1b…, from_iso: 2026-11-24T10:00:00Z, to_iso: …)`;
 * owners should read "Checked free times for RSA Registration & Onboarding Call with Aisha Bello,
 * Tuesday 24 November, 10:00–11:00" instead.
 */

export type NameLookup = { offerings: Record<string, string>; team: Record<string, string> };

function parseCall(text: string): { name: string; args: Record<string, string> } | null {
  const m = /^\s*([a-z_][a-z0-9_]*)\s*\((.*)\)\s*$/is.exec(text || "");
  if (!m) return null;
  const args: Record<string, string> = {};
  // key: value pairs separated by ", " where the next token is "key:"
  const body = m[2];
  const re = /([a-z_][a-z0-9_]*)\s*:\s*(.*?)(?=,\s*[a-z_][a-z0-9_]*\s*:|$)/gis;
  let a: RegExpExecArray | null;
  while ((a = re.exec(body))) args[a[1].toLowerCase()] = a[2].trim().replace(/^["']|["']$/g, "");
  return { name: m[1].toLowerCase(), args };
}

function when(iso?: string, opts: Intl.DateTimeFormatOptions = {}): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  return d.toLocaleString("en-GB", { weekday: "long", day: "numeric", month: "long", hour: "numeric", minute: "2-digit", ...opts });
}

function time(iso?: string): string {
  if (!iso) return "";
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? "" : d.toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit" });
}

function day(iso?: string): string {
  if (!iso) return "";
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? "" : d.toLocaleDateString("en-GB", { weekday: "long", day: "numeric", month: "long" });
}

export function describeToolLine(text: string, names: NameLookup): string {
  const call = parseCall(text);
  if (!call) return text;
  const { name, args } = call;
  const offering = args.offering_id ? names.offerings[args.offering_id] : "";
  const person = args.team_member_id ? names.team[args.team_member_id] : "";
  const svc = offering ? ` ${offering}` : " an appointment";
  const withWho = person ? ` with ${person}` : "";
  switch (name) {
    case "get_business_info":
      return "Looked up the business details";
    case "get_availability": {
      const range = args.from_iso ? `, ${day(args.from_iso)}, ${time(args.from_iso)}–${time(args.to_iso)}` : "";
      return `Checked free times for${svc}${withWho}${range}`;
    }
    case "book_appointment":
      return `Booked${svc}${withWho}${args.starts_at ? ` for ${when(args.starts_at)}` : ""}`;
    case "lookup_appointment":
      return "Looked up the caller's bookings";
    case "reschedule_appointment":
      return `Moved the booking${args.starts_at ? ` to ${when(args.starts_at)}` : ""}`;
    case "cancel_appointment":
      return "Cancelled a booking";
    case "request_verification_code":
      return "Sent a verification code to the caller's phone";
    case "verify_caller_code":
      return "Checked the caller's verification code";
    case "capture_location":
      return "Noted the caller's location";
    case "request_human":
    case "escalate_to_human":
      return "Asked a member of staff to take over";
    default: {
      const words = name.replace(/_/g, " ");
      return words.charAt(0).toUpperCase() + words.slice(1);
    }
  }
}
