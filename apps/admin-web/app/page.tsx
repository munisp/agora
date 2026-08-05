import { auth } from "@/auth";
import { redirect } from "next/navigation";

/** Root: send signed-in users to their org dashboard, others to sign-in. */
export default async function RootPage() {
  const session = await auth();
  const orgs = (session?.user as { orgs?: string[] } | undefined)?.orgs ?? [];
  if (session?.user && orgs.length > 0) {
    redirect(`/app/${orgs[0]}`);
  }
  redirect("/sign-in");
}
