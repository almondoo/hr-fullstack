import { data, redirect } from "react-router";
import type { LoaderFunctionArgs, MetaFunction } from "react-router";
import { useLoaderData } from "react-router";
import { apiMe } from "~/lib/api.server";
import type { MeResponse } from "~/lib/api.server";
import { AppLayout } from "~/components/AppLayout";

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
    <AppLayout displayName={me.user.displayName}>
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

      {/* Quick navigation cards */}
      <section style={styles.card} aria-label="クイックアクション">
        <h2 style={styles.sectionTitle}>クイックアクション</h2>
        <div style={styles.actionGrid}>
          <a href="/attendance" style={styles.actionLink}>
            <span style={styles.actionTitle}>勤怠管理</span>
            <span style={styles.actionDesc}>打刻・勤怠記録の確認</span>
          </a>
          <a href="/attendance/clock-in" style={styles.actionLink}>
            <span style={styles.actionTitle}>打刻</span>
            <span style={styles.actionDesc}>出退勤を記録する</span>
          </a>
          <a href="/selfservice/change-requests" style={styles.actionLink}>
            <span style={styles.actionTitle}>変更申請</span>
            <span style={styles.actionDesc}>プロフィール変更申請の確認</span>
          </a>
          <a href="/selfservice/change-requests/new" style={styles.actionLink}>
            <span style={styles.actionTitle}>変更申請を作成</span>
            <span style={styles.actionDesc}>新しい変更申請を提出</span>
          </a>
        </div>
      </section>
    </AppLayout>
  );
}

// ---------------------------------------------------------------------------
// Styles
// ---------------------------------------------------------------------------

const styles = {
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
    gridTemplateColumns: "minmax(120px, 180px) 1fr",
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
    wordBreak: "break-word" as const,
  },
  permList: {
    margin: 0,
    padding: 0,
    display: "flex",
    flexWrap: "wrap" as const,
    gap: "0.5rem",
    listStyle: "none",
  },
  permItem: {
    backgroundColor: "#dbeafe",
    color: "#1d4ed8",
    borderRadius: "9999px",
    padding: "0.2rem 0.75rem",
    fontSize: "0.8125rem",
    fontWeight: 500,
  },
  actionGrid: {
    display: "grid",
    gridTemplateColumns: "repeat(auto-fill, minmax(200px, 1fr))",
    gap: "1rem",
  },
  actionLink: {
    display: "flex",
    flexDirection: "column" as const,
    gap: "0.25rem",
    backgroundColor: "#f0f9ff",
    border: "1px solid #bae6fd",
    borderRadius: "8px",
    padding: "1rem",
    textDecoration: "none",
    color: "inherit",
    transition: "background-color 0.15s",
  },
  actionTitle: {
    fontSize: "0.9375rem",
    fontWeight: 600,
    color: "#0c4a6e",
  },
  actionDesc: {
    fontSize: "0.8125rem",
    color: "#475569",
  },
} as const;
