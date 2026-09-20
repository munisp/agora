import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { SignupClient } from "./signup-client";

export const metadata = { title: "Create your organisation" };

/**
 * SPEC-W45 STK O5: tenant self-signup. Requires a signed-in user (the
 * identity create-tenant endpoint binds the caller as owner and forces the
 * free plan); visitors without a session are sent through sign-in first.
 */
export default async function SignupPage() {
  const session = await auth();
  if (!session) redirect("/sign-in?callbackUrl=/signup");
  return <SignupClient />;
}
