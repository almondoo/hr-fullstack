import {
  isRouteErrorResponse,
  Links,
  Meta,
  Outlet,
  Scripts,
  ScrollRestoration,
} from "react-router";
import type { HeadersFunction, LinksFunction } from "react-router";
import { buildSecurityHeaders } from "~/lib/security-headers.server";

export const headers: HeadersFunction = () => {
  return buildSecurityHeaders();
};

/**
 * Global baseline styles injected as a <style> tag.
 *
 * Goals:
 * - Visible focus ring for keyboard navigation (WCAG 2.1 AA 2.4.7)
 * - Responsive box model reset
 * - Mobile-friendly tap target minimum size hint
 */
const globalStyles = `
  *, *::before, *::after { box-sizing: border-box; }
  body { margin: 0; -webkit-text-size-adjust: 100%; }
  :focus-visible {
    outline: 3px solid #2563eb;
    outline-offset: 2px;
  }
  a:focus-visible,
  button:focus-visible,
  input:focus-visible,
  select:focus-visible,
  textarea:focus-visible {
    outline: 3px solid #2563eb;
    outline-offset: 2px;
  }
  /* Skip-link visible on focus */
  a[href="#main-content"]:focus {
    position: fixed !important;
    top: 0.75rem !important;
    left: 0.75rem !important;
    width: auto !important;
    height: auto !important;
    overflow: visible !important;
    background: #1e40af;
    color: #fff;
    padding: 0.5rem 1rem;
    border-radius: 6px;
    font-size: 0.9rem;
    font-weight: 600;
    z-index: 9999;
    text-decoration: none;
  }
  /* Responsive images */
  img, video { max-width: 100%; height: auto; }
`;

export const links: LinksFunction = () => [];

export function Layout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="ja">
      <head>
        <meta charSet="utf-8" />
        <meta name="viewport" content="width=device-width, initial-scale=1" />
        <Meta />
        <Links />
        {/* Baseline a11y + responsive styles */}
        <style dangerouslySetInnerHTML={{ __html: globalStyles }} />
      </head>
      <body>
        {children}
        <ScrollRestoration />
        <Scripts />
      </body>
    </html>
  );
}

export default function App() {
  return <Outlet />;
}

export function ErrorBoundary({ error }: { error: unknown }) {
  let message = "予期しないエラーが発生しました。";
  let details = "";

  if (isRouteErrorResponse(error)) {
    message =
      error.status === 404
        ? "ページが見つかりません。"
        : `エラー ${error.status}`;
    details = error.data as string;
  } else if (error instanceof Error) {
    details = error.message;
  }

  return (
    <main
      style={{
        fontFamily: "system-ui, sans-serif",
        padding: "2rem",
        maxWidth: "600px",
        margin: "0 auto",
      }}
    >
      <h1>{message}</h1>
      {details && <p style={{ color: "#666" }}>{details}</p>}
    </main>
  );
}
