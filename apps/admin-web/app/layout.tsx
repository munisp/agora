import type { Metadata, Viewport } from "next";
import { Hanken_Grotesk, Newsreader } from "next/font/google";
import { ToastProvider } from "@/components/ui/toast";
import { PwaRegister } from "@/components/pwa-register";
import { PwaInstallPrompt } from "@/components/pwa-install-prompt";
import "./globals.css";

// Brand type: Newsreader for headings, Hanken Grotesk for text (self-hosted by next/font at build time).
const newsreader = Newsreader({ subsets: ["latin"], variable: "--font-newsreader", display: "swap" });
const hanken = Hanken_Grotesk({ subsets: ["latin"], variable: "--font-hanken", display: "swap" });

export const metadata: Metadata = {
  title: {
    default: "Agora · Your front desk, open all night",
    template: "%s · Agora",
  },
  description:
    "Agora answers your customers, books their appointments and sends reminders, day and night, so you can get on with the work.",
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
    <html lang="en" className={`${newsreader.variable} ${hanken.variable}`}>
      <body>
        <ToastProvider>{children}</ToastProvider>
        <PwaRegister />
        <PwaInstallPrompt />
      </body>
    </html>
  );
}
