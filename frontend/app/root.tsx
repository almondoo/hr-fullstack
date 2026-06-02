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

export const links: LinksFunction = () => [];

export function Layout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="ja">
      <head>
        <meta charSet="utf-8" />
        <meta name="viewport" content="width=device-width, initial-scale=1" />
        <Meta />
        <Links />
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
