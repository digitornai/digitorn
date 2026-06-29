import React from "react";
import BrowserOnly from "@docusaurus/BrowserOnly";
import AskAi from "@site/src/components/AskAi";

/**
 * Swizzled Root — wraps the whole app so the floating "Ask AI" widget is
 * present on every page. Rendered client-only (it needs window + an iframe).
 */
export default function Root({ children }: { children: React.ReactNode }) {
  // Ask AI widget temporarily disabled (revisiting the docked-panel layout
  // later). Re-enable by restoring the <BrowserOnly> line below.
  return (
    <>
      {children}
      {/* <BrowserOnly>{() => <AskAi />}</BrowserOnly> */}
    </>
  );
}
