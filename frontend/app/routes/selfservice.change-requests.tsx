/**
 * /selfservice/change-requests — My self-service change request list.
 *
 * BFF loader: GET /api/v1/selfservice/change-requests
 * Requires selfservice:read permission; shows an error card on 403.
 */

import { data, redirect, Link, useLoaderData } from "react-router";
import type { LoaderFunctionArgs, MetaFunction } from "react-router";
import { apiMe, apiListChangeRequests } from "~/lib/api.server";
import type { MeResponse, ChangeRequest } from "~/lib/api.server";
import { AppLayout } from "~/components/AppLayout";

// ---------------------------------------------------------------------------
// Meta
// ---------------------------------------------------------------------------

export const meta: MetaFunction = () => [
  { title: "変更申請一覧 | HR SaaS" },
];

// ---------------------------------------------------------------------------
// Loader
// ---------------------------------------------------------------------------

interface LoaderData {
  me: MeResponse;
  changeRequests: ChangeRequest[];
  permissionDenied: boolean;
}

export async function loader({ request }: LoaderFunctionArgs) {
  const incomingCookie = request.headers.get("cookie");

  const meResult = await apiMe(incomingCookie);
  if (!meResult.ok || !meResult.data) {
    return redirect("/login");
  }
  const me = meResult.data;

  const crResult = await apiListChangeRequests(incomingCookie, {
    employee_id: me.user.id,
  });

  const responseHeaders = new Headers();
  for (const cookie of meResult.setCookieHeaders) {
    responseHeaders.append("Set-Cookie", cookie);
  }

  if (crResult.status === 403) {
    return data<LoaderData>(
      { me, changeRequests: [], permissionDenied: true },
      { headers: responseHeaders },
    );
  }

  return data<LoaderData>(
    {
      me,
      changeRequests: crResult.data?.change_requests ?? [],
      permissionDenied: false,
    },
    { headers: responseHeaders },
  );
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function statusLabel(status: string): string {
  const map: Record<string, string> = {
    pending: "承認待ち",
    approved: "承認済み",
    rejected: "却下",
    reflected: "反映済み",
  };
  return map[status] ?? status;
}

function statusStyle(status: string): React.CSSProperties {
  switch (status) {
    case "approved":
    case "reflected":
      return { backgroundColor: "#dcfce7", color: "#166534" };
    case "rejected":
      return { backgroundColor: "#fee2e2", color: "#991b1b" };
    case "pending":
      return { backgroundColor: "#fef9c3", color: "#713f12" };
    default:
      return { backgroundColor: "#f3f4f6", color: "#374151" };
  }
}

function targetTypeLabel(t: string): string {
  const map: Record<string, string> = {
    employee_profile: "従業員プロフィール",
    emergency_contact: "緊急連絡先",
    commute: "通勤",
    bank_account: "口座情報",
    dependents: "扶養家族",
  };
  return map[t] ?? t;
}

function formatDate(iso: string): string {
  return iso.slice(0, 10);
}

// ---------------------------------------------------------------------------
// Component
// ---------------------------------------------------------------------------

export default function ChangeRequestsPage() {
  const { me, changeRequests, permissionDenied } = useLoaderData<LoaderData>();

  return (
    <AppLayout displayName={me.user.displayName}>
      <div style={styles.pageHeader}>
        <h1 style={styles.heading}>変更申請一覧</h1>
        <Link to="/selfservice/change-requests/new" style={styles.primaryBtn}>
          新規申請
        </Link>
      </div>

      {permissionDenied && (
        <div role="alert" style={styles.errorCard}>
          <strong>アクセス権限がありません。</strong>
          <p style={{ margin: "0.25rem 0 0" }}>
            変更申請の閲覧には <code>selfservice:read</code> 権限が必要です。
          </p>
        </div>
      )}

      {!permissionDenied && changeRequests.length === 0 && (
        <div style={styles.emptyState}>
          <p>変更申請はまだありません。</p>
          <Link to="/selfservice/change-requests/new" style={styles.linkBtn}>
            新規申請を作成する
          </Link>
        </div>
      )}

      {!permissionDenied && changeRequests.length > 0 && (
        <div style={styles.tableWrapper}>
          <table
            style={styles.table}
            aria-label="変更申請一覧"
          >
            <thead>
              <tr>
                <th scope="col" style={styles.th}>種別</th>
                <th scope="col" style={styles.th}>状態</th>
                <th scope="col" style={styles.th}>申請日</th>
                <th scope="col" style={styles.th}>更新日</th>
                <th scope="col" style={styles.th}>反映日</th>
              </tr>
            </thead>
            <tbody>
              {changeRequests.map((cr) => (
                <tr key={cr.id} style={styles.tr}>
                  <td style={styles.td}>
                    {targetTypeLabel(cr.target_type)}
                  </td>
                  <td style={styles.td}>
                    <span
                      style={{
                        ...styles.statusBadge,
                        ...statusStyle(cr.status),
                      }}
                    >
                      {statusLabel(cr.status)}
                    </span>
                  </td>
                  <td style={styles.td}>{formatDate(cr.created_at)}</td>
                  <td style={styles.td}>{formatDate(cr.updated_at)}</td>
                  <td style={styles.td}>
                    {cr.reflected_at ? formatDate(cr.reflected_at) : "—"}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </AppLayout>
  );
}

// ---------------------------------------------------------------------------
// Styles
// ---------------------------------------------------------------------------

const styles = {
  pageHeader: {
    display: "flex",
    alignItems: "center",
    justifyContent: "space-between",
    flexWrap: "wrap" as const,
    gap: "1rem",
    marginBottom: "1.5rem",
  },
  heading: {
    margin: 0,
    fontSize: "1.75rem",
    fontWeight: 700,
    color: "#1a1a1a",
  },
  primaryBtn: {
    backgroundColor: "#2563eb",
    color: "#ffffff",
    borderRadius: "6px",
    padding: "0.625rem 1.25rem",
    fontSize: "0.9375rem",
    fontWeight: 600,
    textDecoration: "none",
    display: "inline-block",
    whiteSpace: "nowrap" as const,
  },
  errorCard: {
    backgroundColor: "#fef2f2",
    border: "1px solid #fecaca",
    borderRadius: "8px",
    padding: "1rem 1.25rem",
    color: "#7f1d1d",
    marginBottom: "1.25rem",
  },
  emptyState: {
    backgroundColor: "#ffffff",
    borderRadius: "8px",
    boxShadow: "0 1px 4px rgba(0,0,0,0.08)",
    padding: "3rem 1.5rem",
    textAlign: "center" as const,
    color: "#6b7280",
  },
  linkBtn: {
    display: "inline-block",
    marginTop: "0.75rem",
    color: "#2563eb",
    fontSize: "0.9rem",
  },
  tableWrapper: {
    backgroundColor: "#ffffff",
    borderRadius: "8px",
    boxShadow: "0 1px 4px rgba(0,0,0,0.08)",
    overflow: "auto",
    WebkitOverflowScrolling: "touch" as unknown as undefined,
  },
  table: {
    width: "100%",
    minWidth: "540px",
    borderCollapse: "collapse" as const,
    fontSize: "0.9rem",
  },
  th: {
    textAlign: "left" as const,
    padding: "0.75rem 1rem",
    fontSize: "0.8125rem",
    fontWeight: 600,
    color: "#6b7280",
    borderBottom: "1px solid #e5e7eb",
    whiteSpace: "nowrap" as const,
    backgroundColor: "#f9fafb",
  },
  tr: {
    borderBottom: "1px solid #f3f4f6",
  },
  td: {
    padding: "0.75rem 1rem",
    color: "#111827",
    whiteSpace: "nowrap" as const,
  },
  statusBadge: {
    borderRadius: "9999px",
    padding: "0.2rem 0.625rem",
    fontSize: "0.75rem",
    fontWeight: 500,
    display: "inline-block",
  },
} as const;
