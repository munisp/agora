import type { Metadata, Viewport } from "next";
import { ToastProvider } from "@/components/ui/toast";
import { PwaRegister } from "@/components/pwa-register";
import { PwaInstallPrompt } from "@/components/pwa-install-prompt";
import "./globals.css";

export const metadata: Metadata = {
  title: {
    default: "Agora — Open-source AI receptionist",
    template: "%s · Agora",
  },
  description:
    "Agora is the open-source, multi-tenant AI receptionist platform: voice + text concierge, bookings, catalog, knowledge base and payments.",
  manifest: "/manifest.webmanifest",
  icons: { icon: "/icons/agora-icon.svg" },
  appleWebApp: { capable: true, title: "Agora Admin" },
};

// PWA theme color (Next.js requires themeColor under viewport, not metadata).
// Aligned with the admin-web design tokens in globals.css (--color-primary).
export const viewport: Viewport = {
  themeColor: "#7c5b3e",
};

export default function RootLayout({
  children,
}: Readonly<{ children: React.ReactNode }>) {
  return (
    <html lang="en">
      <body>
        <ToastProvider>{children}</ToastProvider>
        <PwaRegister />
        <PwaInstallPrompt />
      </body>
    </html>
  );
}
