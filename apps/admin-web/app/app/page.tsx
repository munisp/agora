import { redirect } from "next/navigation";
import Link from "next/link";
import { auth } from "@/lib/auth";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";

/** /app -> redirect to the user's first tenant workspace. */
export default async function AppIndexPage() {
  const session = await auth();
  if (!session) redirect("/sign-in");

  const first = (session.tenantSlugs ?? [])[0];
  if (first) redirect(`/app/${first}`);

  return (
    <div className="flex min-h-screen items-center justify-center px-4">
      <Card className="w-full max-w-md text-center">
        <CardHeader>
          <CardTitle>No organisation yet</CardTitle>
        </CardHeader>
        <CardContent className="space-y-4">
          <p className="text-sm text-muted-foreground">
            Your account is not a member of any tenant. Ask an owner to invite
            you, or create your own free workspace now.
          </p>
          <div className="flex justify-center gap-2">
            <Link href="/signup">
              <Button>Create a workspace</Button>
            </Link>
            <Link href="/">
              <Button variant="outline">Back to home</Button>
            </Link>
          </div>
        </CardContent>
      </Card>
    </div>
  );
}
