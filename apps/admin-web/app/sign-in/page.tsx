import { redirect } from "next/navigation";
import { auth, signIn } from "@/lib/auth";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";

export const metadata = { title: "Sign in" };

export default async function SignInPage({
  searchParams,
}: {
  searchParams: Promise<{ callbackUrl?: string }>;
}) {
  const { callbackUrl } = await searchParams;
  const session = await auth();
  if (session) redirect(callbackUrl ?? "/app");

  return (
    <div className="flex min-h-screen items-center justify-center bg-muted/40 px-4">
      <Card className="w-full max-w-sm">
        <CardHeader className="items-center text-center">
          <span className="mb-2 flex h-10 w-10 items-center justify-center rounded-md bg-primary text-sm font-bold text-primary-foreground">
            OD
          </span>
          <CardTitle className="text-xl">Welcome to Agora</CardTitle>
          <CardDescription>
            Sign in with your organisation account to manage your AI
            receptionist.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <form
            action={async () => {
              "use server";
              await signIn("keycloak", {
                redirectTo: callbackUrl ?? "/app",
              });
            }}
          >
            <Button type="submit" className="w-full" size="lg">
              Continue with SSO
            </Button>
          </form>
          <p className="mt-4 text-center text-xs text-muted-foreground">
            Authentication is delegated to the platform identity provider
            (Keycloak, OIDC + PKCE).
          </p>
        </CardContent>
      </Card>
    </div>
  );
}
