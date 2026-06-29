import React, { useEffect, useState } from "react";

/**
 * "Ask AI" — floating launcher + DOCKED side panel that embeds the real
 * Digitorn premium chat (the app's /embed/ask route) in an iframe.
 *
 * Cursor-style behaviour: opening does not overlay a modal — it docks a panel
 * on the right and pushes the page content left (and collapses the docs
 * sidebar to give the content its width back). That layout shift lives in
 * custom.css, keyed off `html[data-askai="open"]`; this component only toggles
 * that attribute, owns the panel chrome, syncs the theme, and lazily mounts
 * the iframe on first open (kept after, so reopening is instant).
 */

function embedBase(): string {
  if (typeof window === "undefined") return "https://digitorn.ai/embed/ask";
  const h = window.location.hostname;
  if (h === "localhost" || h === "127.0.0.1") {
    return "http://localhost:3000/embed/ask";
  }
  return "https://digitorn.ai/embed/ask";
}

function useDocTheme(): "light" | "dark" {
  const [theme, setTheme] = useState<"light" | "dark">("dark");
  useEffect(() => {
    const read = () =>
      setTheme(
        document.documentElement.getAttribute("data-theme") === "light"
          ? "light"
          : "dark",
      );
    read();
    const obs = new MutationObserver(read);
    obs.observe(document.documentElement, {
      attributes: true,
      attributeFilter: ["data-theme"],
    });
    return () => obs.disconnect();
  }, []);
  return theme;
}

const EASE = "cubic-bezier(0.22, 1, 0.36, 1)";

export default function AskAi() {
  const [open, setOpen] = useState(false);
  const [mounted, setMounted] = useState(false); // iframe lazy-mount latch
  const theme = useDocTheme();
  const src = `${embedBase()}?theme=${theme}`;

  // Drive the page-shift (push content + collapse sidebar) via a root attr.
  useEffect(() => {
    if (open) setMounted(true);
    const root = document.documentElement;
    if (open) root.setAttribute("data-askai", "open");
    else root.removeAttribute("data-askai");
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && setOpen(false);
    window.addEventListener("keydown", onKey);
    return () => {
      window.removeEventListener("keydown", onKey);
      root.removeAttribute("data-askai");
    };
  }, [open]);

  return (
    <>
      {/* Launcher — always mounted, fades/scales out when the panel opens */}
      <button
        type="button"
        onClick={() => setOpen(true)}
        aria-label="Ask AI"
        style={{
          position: "fixed",
          right: 20,
          bottom: 20,
          zIndex: 1000,
          display: "inline-flex",
          alignItems: "center",
          gap: 8,
          height: 44,
          padding: "0 18px",
          borderRadius: 999,
          border: "none",
          cursor: "pointer",
          fontSize: 14,
          fontWeight: 600,
          background: "var(--digi-accent)",
          color: "var(--digi-on-accent)",
          boxShadow: "0 10px 30px -10px var(--digi-glow)",
          opacity: open ? 0 : 1,
          transform: open ? "scale(0.9) translateY(6px)" : "scale(1)",
          pointerEvents: open ? "none" : "auto",
          transition: `opacity 200ms ease, transform 240ms ${EASE}`,
        }}
      >
        <span aria-hidden style={{ fontSize: 15 }}>
          ✦
        </span>
        Ask AI
      </button>

      {/* Docked panel — slides in from the right, in sync with the page shift */}
      <aside
        role="dialog"
        aria-label="Ask AI"
        aria-hidden={!open}
        style={{
          position: "fixed",
          top: 0,
          right: 0,
          zIndex: 1000,
          height: "100svh",
          width: "min(440px, 100vw)",
          display: "flex",
          flexDirection: "column",
          background: "var(--digi-bg)",
          borderLeft: "1px solid var(--digi-border)",
          boxShadow: "-20px 0 60px -34px rgba(0,0,0,0.55)",
          transform: open ? "translateX(0)" : "translateX(100%)",
          transition: `transform 320ms ${EASE}`,
          willChange: "transform",
          pointerEvents: open ? "auto" : "none",
        }}
      >
        <div
          style={{
            display: "flex",
            alignItems: "center",
            gap: 10,
            padding: "12px 14px",
            borderBottom: "1px solid var(--digi-border)",
          }}
        >
          <span aria-hidden style={{ color: "var(--digi-accent)", fontSize: 15 }}>
            ✦
          </span>
          <span
            style={{
              fontSize: 14,
              fontWeight: 600,
              color: "var(--digi-text-bright)",
            }}
          >
            Ask AI
          </span>
          <button
            type="button"
            onClick={() => setOpen(false)}
            aria-label="Close"
            style={{
              marginLeft: "auto",
              width: 30,
              height: 30,
              borderRadius: 8,
              border: "1px solid var(--digi-border)",
              background: "var(--digi-surface)",
              color: "var(--digi-text-muted)",
              cursor: "pointer",
              fontSize: 16,
              lineHeight: 1,
            }}
          >
            ×
          </button>
        </div>
        {mounted ? (
          <iframe
            title="Ask the Digitorn docs"
            src={src}
            allow="microphone; clipboard-write"
            style={{ flex: 1, width: "100%", border: "none" }}
          />
        ) : (
          <div style={{ flex: 1 }} />
        )}
      </aside>
    </>
  );
}
