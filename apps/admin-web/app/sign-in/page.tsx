/* eslint-disable @next/next/no-img-element -- local, pre-sized photo in /public/media */
import Link from "next/link";
import { redirect } from "next/navigation";
import { ArrowRight, Lock } from "lucide-react";
import { auth, signIn } from "@/lib/auth";
import { AgoraWordmark } from "@/components/brand";

export const metadata = { title: "Sign in" };

export default async function SignInPage({
  searchParams,
}: {
  searchParams: Promise<{ callbackUrl?: string }>;
}) {
  const { callbackUrl } = await searchParams;
  const session = await auth();
  // An expired session (refresh failed) must reach the sign-in button: redirecting it to the dashboard looped, since the
  // dashboard sends expired sessions here (ERR_TOO_MANY_REDIRECTS).
  if (session && !session.error) redirect(callbackUrl ?? "/app");

  return (
    <div className="grid min-h-screen bg-background lg:grid-cols-[1.1fr_1fr]">
      <aside className="relative isolate hidden overflow-hidden bg-umber-deep text-paper lg:block">
        <img
          src="/media/signin.jpg"
          alt="A woman smiling at her phone outdoors"
          className="absolute inset-0 -z-10 h-full w-full object-cover opacity-80"
        />
        <div className="absolute inset-0 -z-10 bg-gradient-to-t from-umber-deep via-umber-deep/30 to-transparent" />
        <div className="flex h-full flex-col justify-between p-10">
          <Link href="/">
            <AgoraWordmark tone="light" />
          </Link>
          <blockquote className="max-w-md">
            <p className="font-display text-3xl leading-snug">
              &ldquo;Booked for Saturday at ten. See you then!&rdquo;
            </p>
            <p className="mt-3 text-sm text-paper/75">Your receptionist, at 11 pm, while you sleep.</p>
          </blockquote>
        </div>
        <div className="woven-band absolute inset-x-0 bottom-0" />
      </aside>

      <main className="paper-pattern flex items-center justify-center px-6 py-16">
        <div className="w-full max-w-sm">
          <Link href="/" className="lg:hidden">
            <AgoraWordmark />
          </Link>
          <p className="eyebrow mt-10 lg:mt-0">Welcome back</p>
          <h1 className="mt-3 text-4xl leading-tight text-umber">Sign in to your front desk</h1>
          <p className="mt-4 leading-relaxed text-muted-foreground">
            See today&apos;s bookings, the conversations your receptionist had, and what it learned about your
            business.
          </p>
          <form
            className="mt-10"
            action={async () => {
              "use server";
              await signIn("keycloak", {
                redirectTo: callbackUrl ?? "/app",
              });
            }}
          >
            <button
              type="submit"
              className="inline-flex w-full items-center justify-center gap-2 rounded-full bg-umber px-6 py-3.5 font-medium text-paper transition hover:bg-primary-hover"
            >
              Sign in <ArrowRight className="h-4 w-4" />
            </button>
          </form>
          <p className="mt-5 flex items-center gap-2 text-xs text-muted-foreground">
            <Lock className="h-3.5 w-3.5" /> You&apos;ll enter your email and password on Agora&apos;s secure sign-in page.
          </p>
          <div className="mt-12 border-t border-border pt-6 text-sm text-muted-foreground">
            New to Agora?{" "}
            <Link href="/p/acme" className="font-medium text-umber underline-offset-4 hover:underline">
              Try the receptionist on a demo salon
            </Link>
          </div>
        </div>
      </main>
    </div>
  );
}
