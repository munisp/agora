"use client";

import * as React from "react";
import { DEFAULT_NAV_LABELS, type NavLabels } from "@/lib/terminology";

const NavLabelsContext = React.createContext<NavLabels>(DEFAULT_NAV_LABELS);

/** Shares the tenant's pack wording (resolved once in the dashboard layout) with client pages. */
export function TerminologyProvider({ labels, children }: { labels: NavLabels; children: React.ReactNode }) {
  return <NavLabelsContext.Provider value={labels}>{children}</NavLabelsContext.Provider>;
}

export function useNavLabels(): NavLabels {
  return React.useContext(NavLabelsContext);
}
