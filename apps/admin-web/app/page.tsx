import Link from "next/link";
import {
  CalendarCheck,
  PhoneCall,
  BookOpen,
  Users,
  CreditCard,
  Globe,
  ArrowRight,
} from "lucide-react";
import {
  Card,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Button } from "@/components/ui/button";

const features = [
  {
    icon: PhoneCall,
    title: "Voice + text concierge",
    body: "An AI receptionist that answers calls and chats, books, reschedules and cancels — fully open-source voice stack (LiveKit, whisper, Piper).",
  },
  {
    icon: CalendarCheck,
    title: "Bookings that just work",
    body: "Offerings with duration, buffers and capacity; phone-confirmed bookings; live updates pushed to your dashboard.",
  },
  {
    icon: Users,
    title: "Team availability",
    body: "Weekly availability rules per team member keep the agent honest about who is actually free.",
  },
  {
    icon: BookOpen,
    title: "Grounded knowledge",
    body: "Hybrid search over your own documents grounds every answer the receptionist gives.",
  },
  {
    icon: Globe,
    title: "Branded public site",
    body: "Publish a booking page per tenant on your own slug — themeable, embeddable, no code.",
  },
  {
    icon: CreditCard,
    title: "Ledger-grade payments",
    body: "Deposits, no-show fees and payouts accounted on a TigerBeetle ledger with Mojaloop payout rails.",
  },
];

export default function MarketingPage() {
  return (
    <div className="min-h-screen">
      <header className="border-b border-border bg-card">
        <div className="mx-auto flex max-w-6xl items-center justify-between px-6 py-4">
          <div className="flex items-center gap-2">
            <span className="flex h-8 w-8 items-center justify-center rounded-md bg-primary text-sm font-bold text-primary-foreground">
              OD
            </span>
            <span className="text-lg font-semibold">Agora</span>
          </div>
          <nav className="flex items-center gap-3">
            <Link href="#features">
              <Button variant="ghost" size="sm">
                Features
              </Button>
            </Link>
            <Link href="/sign-in">
              <Button size="sm">Sign in</Button>
            </Link>
          </nav>
        </div>
      </header>

      <main>
        <section className="mx-auto max-w-6xl px-6 py-24 text-center">
          <p className="mb-4 inline-block rounded-full border border-border bg-secondary px-3 py-1 text-xs font-medium text-secondary-foreground">
            100% open source · self-hostable · multi-tenant
          </p>
          <h1 className="mx-auto max-w-3xl text-5xl font-bold leading-tight tracking-tight">
            The front desk that never sleeps
          </h1>
          <p className="mx-auto mt-6 max-w-2xl text-lg text-muted-foreground">
            Agora gives every business an AI receptionist that answers the
            phone, chats with customers and manages the calendar — built
            entirely on open-source infrastructure you can run yourself.
          </p>
          <div className="mt-10 flex items-center justify-center gap-4">
            <Link href="/sign-in">
              <Button size="lg">
                Open your dashboard
                <ArrowRight className="h-4 w-4" />
              </Button>
            </Link>
            <a
              href="https://github.com/opendesk/opendesk"
              target="_blank"
              rel="noreferrer noopener"
            >
              <Button size="lg" variant="outline">
                View source
              </Button>
            </a>
          </div>
        </section>

        <section id="features" className="border-t border-border bg-muted/50">
          <div className="mx-auto max-w-6xl px-6 py-20">
            <h2 className="text-center text-3xl font-semibold">
              Everything a receptionist does
            </h2>
            <p className="mx-auto mt-3 max-w-xl text-center text-muted-foreground">
              One platform replaces the phone tree, the booking spreadsheet and
              the answering service.
            </p>
            <div className="mt-12 grid gap-6 sm:grid-cols-2 lg:grid-cols-3">
              {features.map((f) => (
                <Card key={f.title}>
                  <CardHeader>
                    <span className="mb-2 flex h-9 w-9 items-center justify-center rounded-md bg-secondary text-secondary-foreground">
                      <f.icon className="h-5 w-5" />
                    </span>
                    <CardTitle>{f.title}</CardTitle>
                    <CardDescription>{f.body}</CardDescription>
                  </CardHeader>
                </Card>
              ))}
            </div>
          </div>
        </section>

        <section className="border-t border-border">
          <div className="mx-auto max-w-6xl px-6 py-20 text-center">
            <h2 className="text-3xl font-semibold">
              Ready when your customers are
            </h2>
            <p className="mx-auto mt-3 max-w-xl text-muted-foreground">
              Deploy the stack with one compose file, sign in through your own
              identity provider, and publish your first booking page in
              minutes.
            </p>
            <div className="mt-8">
              <Link href="/sign-in">
                <Button size="lg">
                  Get started
                  <ArrowRight className="h-4 w-4" />
                </Button>
              </Link>
            </div>
          </div>
        </section>
      </main>

      <footer className="border-t border-border bg-card">
        <div className="mx-auto flex max-w-6xl items-center justify-between px-6 py-6 text-sm text-muted-foreground">
          <span>Agora — open-source AI receptionist platform</span>
          <span>Apache-2.0</span>
        </div>
      </footer>
    </div>
  );
}
