import type { Metadata } from "next";
import "./globals.css";

export const metadata: Metadata = {
  title: "Fantom — AI Routing Control Plane",
  description: "Manage AI providers, model routes, credentials and gateway traffic from one control plane.",
};

export default function RootLayout({ children }: Readonly<{ children: React.ReactNode }>) {
  return (
    <html lang="en">
      <body>{children}</body>
    </html>
  );
}
