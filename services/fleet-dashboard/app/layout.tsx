import type { Metadata } from "next";
import type { ReactNode } from "react";
import { TopBar } from "@/components/TopBar";
import "./globals.css";

export const metadata: Metadata = {
  title: "Intelletry Fleet Operations",
  description: "Connected-vehicle fleet health, diagnostics and alert operations.",
};

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="en">
      <body>
        <TopBar />
        <main className="page">{children}</main>
      </body>
    </html>
  );
}
