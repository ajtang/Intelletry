"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { useOperator } from "@/lib/hooks";

const LINKS = [
  { href: "/", label: "Fleet Overview" },
  { href: "/map", label: "Fleet Map" },
  { href: "/vehicles", label: "Vehicles" },
  { href: "/operations", label: "Operations Feed" },
];

export function TopBar() {
  const pathname = usePathname();
  const [operator, setOperator] = useOperator();
  return (
    <header className="topbar">
      <Link href="/" className="brand">
        Intelletry
      </Link>
      <nav className="nav" aria-label="Main">
        {LINKS.map((l) => {
          const active = l.href === "/" ? pathname === "/" : pathname.startsWith(l.href);
          return (
            <Link key={l.href} href={l.href} aria-current={active ? "page" : undefined}>
              {l.label}
            </Link>
          );
        })}
      </nav>
      <label className="operator" title="Recorded in the alert audit trail. This is not sign-in.">
        Operator
        <input value={operator} maxLength={128} placeholder="your name" onChange={(e) => setOperator(e.target.value)} />
      </label>
    </header>
  );
}
