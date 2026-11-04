import type { Metadata } from "next";
import "./globals.css";

export const metadata: Metadata = {
  title: "Fantom AI Gateway",
  description: "Unified, OpenAI-compatible AI Gateway",
};

export default function RootLayout({ children }: Readonly<{ children: React.ReactNode }>) {
  return (
    <html lang="en">
      <body>{children}</body>
    </html>
  );
}
