/* eslint-disable @next/next/no-img-element -- local, pre-sized photos in /public/media (credits in CREDITS.md) */
import Link from "next/link";
import { BellRing, CalendarCheck, LayoutDashboard, MessageCircle, ArrowRight } from "lucide-react";
import { AgoraMark, AgoraWordmark } from "@/components/brand";

export const metadata = { title: { absolute: "Agora · Your front desk, open all night" } };

const STEPS = ["Asked", "Booked", "Reminded", "Arrived"];

const BUSINESSES = [
  {
    photo: "/media/salon.jpg",
    alt: "A stylist braiding a client's hair in a salon",
    title: "Salons and beauty",
    body: "Braids, cuts, nails and make-up, each stylist with their own diary.",
  },
  {
    photo: "/media/barber.jpg",
    alt: "A barber and a smiling customer in a barbershop",
    title: "Barbers",
    body: "Bookings and walk-ins in one queue, and reminders that cut no-shows.",
  },
  {
    photo: "/media/clinic.jpg",
    alt: "A doctor talking with a patient during a check-up in Lagos",
    title: "Clinics",
    body: "Patients book check-ups, get reminders and ask questions at any hour.",
  },
];

const FEATURES = [
  { icon: MessageCircle, title: "Answers from your own information", body: "Prices, hours, directions and policies, in your words." },
  { icon: CalendarCheck, title: "Books, moves and cancels", body: "Straight into your diary, with the right gaps between appointments." },
  { icon: BellRing, title: "Reminds your customers", body: "By SMS, WhatsApp or email, so fewer people forget." },
  { icon: LayoutDashboard, title: "Everything on one page", body: "Every booking and conversation, ready when you open your phone." },
];

function Bubble({ from, children }: { from: "customer" | "agora"; children: React.ReactNode }) {
  return (
    <div
      className={
        from === "customer"
          ? "max-w-[80%] self-start rounded-2xl rounded-tl-sm bg-paper px-3.5 py-2 text-[13px] leading-snug text-foreground shadow-sm"
          : "max-w-[85%] self-end rounded-2xl rounded-tr-sm bg-[#efe3cf] px-3.5 py-2 text-[13px] leading-snug text-foreground"
      }
    >
      {children}
    </div>
  );
}

export default function LandingPage() {
  return (
    <div className="bg-background text-foreground">
      {/* Hero */}
      <section className="relative isolate min-h-[92vh] overflow-hidden bg-umber-deep text-paper">
        <img
          src="/media/hero-salon.jpg"
          alt="Stylists at work on a client in a busy salon"
          className="absolute inset-0 -z-10 h-full w-full object-cover opacity-70"
        />
        <div className="absolute inset-0 -z-10 bg-gradient-to-r from-umber-deep via-umber-deep/80 to-umber-deep/10" />
        <header className="mx-auto flex max-w-6xl items-center justify-between px-6 py-6">
          <AgoraWordmark tone="light" />
          <nav className="flex items-center gap-6 text-sm">
            <a href="#how" className="hidden text-paper/80 hover:text-paper sm:inline">How it works</a>
            <a href="#who" className="hidden text-paper/80 hover:text-paper sm:inline">Who it&apos;s for</a>
            <Link href="/sign-in" className="rounded-full border border-paper/40 px-4 py-2 font-medium hover:bg-paper hover:text-umber">
              Sign in
            </Link>
          </nav>
        </header>
        <div className="mx-auto max-w-6xl px-6 pb-24 pt-16 sm:pt-28">
          <p className="eyebrow !text-tan">AI receptionist for salons, clinics and studios</p>
          <h1 className="mt-5 max-w-3xl text-5xl leading-[1.05] sm:text-7xl">Your front desk, open all night.</h1>
          <p className="mt-6 max-w-xl text-lg leading-relaxed text-paper/85">
            Agora answers your customers&apos; questions, books their appointments and sends reminders, in chat, on
            WhatsApp and on the phone, so you can get on with the work.
          </p>
          <div className="mt-10 flex flex-wrap gap-3">
            <Link href="/sign-in" className="inline-flex items-center gap-2 rounded-full bg-tan px-6 py-3 font-medium text-umber-deep hover:bg-[#d8b886]">
              Open your dashboard <ArrowRight className="h-4 w-4" />
            </Link>
            <Link href="/p/acme" className="inline-flex items-center gap-2 rounded-full border border-paper/40 px-6 py-3 font-medium hover:bg-paper/10">
              See a live booking page
            </Link>
          </div>
        </div>
        <div className="woven-band absolute inset-x-0 bottom-0" />
      </section>

      {/* One booking */}
      <section id="how" className="paper-pattern">
        <div className="mx-auto grid max-w-6xl items-center gap-14 px-6 py-24 lg:grid-cols-2">
          <div>
            <p className="eyebrow">One booking on Agora</p>
            <h2 className="mt-4 text-4xl leading-tight text-umber sm:text-5xl">A customer asks at 11&nbsp;pm.</h2>
            <p className="mt-6 max-w-md text-lg leading-relaxed text-muted-foreground">
              You&apos;re closed. Agora isn&apos;t. It knows your services, prices and opening hours, and it books
              straight into your diary.
            </p>
            <ol className="mt-12 flex max-w-md items-start">
              {STEPS.map((step, i) => (
                <li key={step} className="flex flex-1 flex-col items-start">
                  <div className="flex w-full items-center">
                    <span className={i === 0 ? "h-4 w-4 rounded-full bg-umber" : "h-4 w-4 rounded-full border border-umber/40 bg-paper"} />
                    {i < STEPS.length - 1 ? <span className="h-px flex-1 bg-umber/25" /> : null}
                  </div>
                  <span className={i === 0 ? "mt-2 text-sm font-semibold text-umber" : "mt-2 text-sm text-muted-foreground"}>{step}</span>
                </li>
              ))}
            </ol>
          </div>
          <div className="relative mx-auto w-full max-w-[340px]">
            <div className="rounded-[2.4rem] border-[10px] border-[#22211c] bg-[#efe8d8] shadow-2xl">
              <div className="flex items-center gap-3 rounded-t-[1.7rem] bg-umber px-4 py-3.5 text-paper">
                <span className="flex h-9 w-9 items-center justify-center rounded-full bg-tan font-display text-lg text-umber-deep">A</span>
                <div className="leading-tight">
                  <p className="text-sm font-semibold">Acme Studio</p>
                  <p className="text-[11px] text-paper/70">Receptionist · replies instantly</p>
                </div>
              </div>
              <div className="flex min-h-[460px] flex-col gap-2.5 px-3.5 pb-24 pt-4">
                <Bubble from="customer">Hi, are you open on Saturday? How much is a haircut?</Bubble>
                <Bubble from="agora">Yes, Saturday 9:00 to 18:00. A haircut is ₦8,000 and takes 30 minutes. Shall I book you in?</Bubble>
                <Bubble from="customer">10am please 🙏</Bubble>
                <div className="max-w-[85%] self-end rounded-2xl rounded-tr-sm border border-umber/15 bg-paper p-3 text-[13px] shadow-sm">
                  <p className="font-semibold text-success">Booked ✓</p>
                  <p className="mt-1">Haircut · Saturday 10:00</p>
                  <p className="font-display text-lg text-umber">₦8,000</p>
                  <p className="mt-1 text-[11px] text-muted-foreground">I&apos;ll remind you on Friday evening.</p>
                </div>
              </div>
            </div>
            <div className="absolute -bottom-10 -left-16 hidden w-52 rounded-lg border border-border bg-paper p-4 shadow-xl sm:block">
              <p className="eyebrow !text-[0.62rem] !text-muted-foreground">On your dashboard</p>
              <p className="mt-2 text-sm font-medium">New appointment</p>
              <p className="font-display text-xl text-umber">Sat, 10:00</p>
              <p className="mt-1 text-sm text-rust">Haircut · Adaeze O.</p>
            </div>
          </div>
        </div>
      </section>

      {/* Who it's for */}
      <section id="who" className="bg-paper">
        <div className="mx-auto max-w-6xl px-6 py-24">
          <p className="eyebrow">Made for appointment businesses</p>
          <h2 className="mt-4 max-w-2xl text-4xl leading-tight text-umber sm:text-5xl">Built for the way you already work.</h2>
          <div className="mt-14 grid gap-8 md:grid-cols-3">
            {BUSINESSES.map((b) => (
              <article key={b.title} className="group">
                <div className="overflow-hidden rounded-xl">
                  <img src={b.photo} alt={b.alt} loading="lazy" className="aspect-[4/3] w-full object-cover transition duration-500 group-hover:scale-105" />
                </div>
                <h3 className="mt-5 font-display text-2xl text-umber">{b.title}</h3>
                <p className="mt-2 leading-relaxed text-muted-foreground">{b.body}</p>
              </article>
            ))}
          </div>
        </div>
      </section>

      {/* What it does */}
      <section className="bg-umber text-paper">
        <div className="mx-auto max-w-6xl px-6 py-24">
          <p className="eyebrow !text-tan">What it does</p>
          <h2 className="mt-4 max-w-2xl text-4xl leading-tight sm:text-5xl">The front desk, handled.</h2>
          <div className="mt-14 grid gap-x-10 gap-y-12 sm:grid-cols-2 lg:grid-cols-4">
            {FEATURES.map((f) => (
              <div key={f.title}>
                <f.icon className="h-7 w-7 text-tan" strokeWidth={1.6} />
                <h3 className="mt-5 font-display text-xl">{f.title}</h3>
                <p className="mt-2 leading-relaxed text-paper/75">{f.body}</p>
              </div>
            ))}
          </div>
        </div>
      </section>

      {/* For your customers */}
      <section className="paper-pattern">
        <div className="mx-auto grid max-w-6xl items-center gap-14 px-6 py-24 lg:grid-cols-2">
          <img src="/media/message.jpg" alt="A woman smiling as she messages on her phone" loading="lazy" className="aspect-[4/3] w-full rounded-xl object-cover shadow-lg" />
          <div>
            <p className="eyebrow">For your customers</p>
            <h2 className="mt-4 text-4xl leading-tight text-umber sm:text-5xl">No app to download.</h2>
            <p className="mt-6 max-w-md text-lg leading-relaxed text-muted-foreground">
              Customers chat on your booking page, call your number or message you on WhatsApp. Agora answers in
              plain, friendly language and hands over to you whenever a person is needed.
            </p>
          </div>
        </div>
      </section>

      {/* Closing */}
      <section className="relative isolate overflow-hidden bg-umber-deep text-paper">
        <img src="/media/closing-barbershop.jpg" alt="A lively barbershop full of customers" loading="lazy" className="absolute inset-0 -z-10 h-full w-full object-cover opacity-45" />
        <div className="absolute inset-0 -z-10 bg-umber-deep/60" />
        <div className="mx-auto max-w-6xl px-6 py-28 text-center">
          <h2 className="mx-auto max-w-3xl text-4xl leading-tight sm:text-6xl">Spend the day with your customers, not the phone.</h2>
          <div className="mt-10 flex flex-wrap justify-center gap-3">
            <Link href="/sign-in" className="inline-flex items-center gap-2 rounded-full bg-tan px-6 py-3 font-medium text-umber-deep hover:bg-[#d8b886]">
              Open your dashboard <ArrowRight className="h-4 w-4" />
            </Link>
            <Link href="/p/acme" className="inline-flex items-center gap-2 rounded-full border border-paper/40 px-6 py-3 font-medium hover:bg-paper/10">
              Try the receptionist
            </Link>
          </div>
        </div>
      </section>

      <footer className="bg-paper">
        <div className="woven-band" />
        <div className="mx-auto flex max-w-6xl flex-wrap items-center justify-between gap-4 px-6 py-8 text-sm text-muted-foreground">
          <span className="inline-flex items-center gap-2"><AgoraMark className="h-6 w-6" /> Agora · your front desk, open all night</span>
          <span>Photos from Pexels</span>
        </div>
      </footer>
    </div>
  );
}
