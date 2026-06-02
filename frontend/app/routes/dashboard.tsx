import { data, redirect, Form } from "react-router";
import type { LoaderFunctionArgs, MetaFunction } from "react-router";
import { useLoaderData } from "react-router";
import { apiMe } from "~/lib/api.server";
import type { MeResponse } from "~/lib/api.server";

// ---------------------------------------------------------------------------
// Meta
// ---------------------------------------------------------------------------

export const meta: MetaFunction = () => [
  { title: "ダッシュボード | HR SaaS" },
];

// ---------------------------------------------------------------------------
// Loader (auth guard)
// ---------------------------------------------------------------------------

export async function loader({ request }: LoaderFunctionArgs) {
  const incomingCookie = request.headers.get("cookie");
  const result = await apiMe(incomingCookie);

  if (!result.ok || !result.data) {
    // Unauthenticated — redirect to login
    return redirect("/login");
  }

  // Forward any refreshed session cookies
  const responseHeaders = new Headers();
  for (const cookie of result.setCookieHeaders) {
    responseHeaders.append("Set-Cookie", cookie);
  }

  return data<MeResponse>(result.data, { headers: responseHeaders });
}

// ---------------------------------------------------------------------------
// Component
// ---------------------------------------------------------------------------

export default function DashboardPage() {
  const me = useLoaderData<MeResponse>();

  return (
    <div style={styles.page}>
      <header style={styles.header}>
        <div style={styles.headerInner}>
          <span style={styles.logo}>HR SaaS</span>
          <nav>
            <Form method="post" action="/logout">
              <button type="submit" style={styles.logoutBtn}>
                ログアウト
              </button>
            </Form>
          </nav>
        </div>
      </header>

      <main style={styles.main}>
        <h1 style={styles.heading}>ダッシュボード</h1>

        <section style={styles.card} aria-label="ユーザー情報">
          <h2 style={styles.sectionTitle}>ログインユーザー</h2>
          <dl style={styles.dl}>
            <div style={styles.dlRow}>
              <dt style={styles.dt}>名前</dt>
              <dd style={styles.dd}>{me.user.displayName}</dd>
            </div>
            <div style={styles.dlRow}>
              <dt style={styles.dt}>メールアドレス</dt>
              <dd style={styles.dd}>{me.user.email}</dd>
            </div>
            <div style={styles.dlRow}>
              <dt style={styles.dt}>ロール</dt>
              <dd style={styles.dd}>{me.role}</dd>
            </div>
          </dl>
        </section>

        <section style={styles.card} aria-label="テナント情報">
          <h2 style={styles.sectionTitle}>テナント</h2>
          <dl style={styles.dl}>
            <div style={styles.dlRow}>
              <dt style={styles.dt}>テナント名</dt>
              <dd style={styles.dd}>{me.tenant.name}</dd>
            </div>
            <div style={styles.dlRow}>
              <dt style={styles.dt}>テナントID</dt>
              <dd style={styles.dd}>{me.tenant.slug}</dd>
            </div>
          </dl>
        </section>

        <section style={styles.card} aria-label="権限">
          <h2 style={styles.sectionTitle}>権限</h2>
          {me.permissions.length > 0 ? (
            <ul style={styles.permList}>
              {me.permissions.map((p) => (
                <li key={p} style={styles.permItem}>
                  {p}
                </li>
              ))}
            </ul>
          ) : (
            <p style={{ color: "#666" }}>権限なし</p>
          )}
        </section>
      </main>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Styles
// ---------------------------------------------------------------------------

const styles = {
  page: {
    minHeight: "100vh",
    backgroundColor: "#f5f5f5",
    fontFamily: "system-ui, -apple-system, sans-serif",
  },
  header: {
    backgroundColor: "#1e40af",
    color: "#ffffff",
    padding: "0 1.5rem",
  },
  headerInner: {
    maxWidth: "900px",
    margin: "0 auto",
    height: "56px",
    display: "flex",
    alignItems: "center",
    justifyContent: "space-between",
  },
  logo: {
    fontSize: "1.125rem",
    fontWeight: 700,
    letterSpacing: "0.05em",
  },
  logoutBtn: {
    background: "transparent",
    border: "1px solid rgba(255,255,255,0.5)",
    borderRadius: "6px",
    color: "#ffffff",
    padding: "0.375rem 0.875rem",
    fontSize: "0.875rem",
    cursor: "pointer",
  },
  main: {
    maxWidth: "900px",
    margin: "0 auto",
    padding: "2rem 1.5rem",
  },
  heading: {
    margin: "0 0 1.5rem",
    fontSize: "1.75rem",
    fontWeight: 700,
    color: "#1a1a1a",
  },
  card: {
    backgroundColor: "#ffffff",
    borderRadius: "8px",
    boxShadow: "0 1px 4px rgba(0,0,0,0.08)",
    padding: "1.5rem",
    marginBottom: "1.25rem",
  },
  sectionTitle: {
    margin: "0 0 1rem",
    fontSize: "1rem",
    fontWeight: 600,
    color: "#374151",
    borderBottom: "1px solid #e5e7eb",
    paddingBottom: "0.5rem",
  },
  dl: {
    margin: 0,
    display: "grid",
    rowGap: "0.5rem",
  },
  dlRow: {
    display: "grid",
    gridTemplateColumns: "180px 1fr",
    gap: "0.5rem",
  },
  dt: {
    fontSize: "0.875rem",
    fontWeight: 500,
    color: "#6b7280",
  },
  dd: {
    margin: 0,
    fontSize: "0.9375rem",
    color: "#111827",
  },
  permList: {
    margin: 0,
    padding: "0 0 0 1.25rem",
    display: "flex",
    flexWrap: "wrap" as const,
    gap: "0.5rem",
    listStyle: "none",
    paddingLeft: 0,
  },
  permItem: {
    backgroundColor: "#dbeafe",
    color: "#1d4ed8",
    borderRadius: "9999px",
    padding: "0.2rem 0.75rem",
    fontSize: "0.8125rem",
    fontWeight: 500,
  },
} as const;
