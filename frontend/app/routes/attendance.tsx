/**
 * /attendance — My attendance records for the current month.
 *
 * BFF pattern: loader fetches /api/v1/attendance/records with the session
 * Cookie forwarded.  Requires attendance:read permission; gracefully shows
 * an error card if the Go API returns 403.
 */

import { data, redirect, Link, useLoaderData } from "react-router";
import type { LoaderFunctionArgs, MetaFunction } from "react-router";
import { apiMe, apiListAttendanceRecords } from "~/lib/api.server";
import type { MeResponse, AttendanceRecord } from "~/lib/api.server";
import { AppLayout } from "~/components/AppLayout";

// ---------------------------------------------------------------------------
// Meta
// ---------------------------------------------------------------------------

export const meta: MetaFunction = () => [
  { title: "勤怠管理 | HR SaaS" },
];

// ---------------------------------------------------------------------------
// Loader
// ---------------------------------------------------------------------------

interface LoaderData {
  me: MeResponse;
  records: AttendanceRecord[];
  from: string;
  to: string;
  permissionDenied: boolean;
}

function isoMonth(date: Date): string {
  return date.toISOString().slice(0, 7); // "YYYY-MM"
}

function firstDay(yearMonth: string): string {
  return `${yearMonth}-01`;
}

function lastDay(yearMonth: string): string {
  const [y, m] = yearMonth.split("-").map(Number);
  const d = new Date(y, m, 0); // last day of month
  return d.toISOString().slice(0, 10);
}

export async function loader({ request }: LoaderFunctionArgs) {
  const incomingCookie = request.headers.get("cookie");

  // Auth guard
  const meResult = await apiMe(incomingCookie);
  if (!meResult.ok || !meResult.data) {
    return redirect("/login");
  }
  const me = meResult.data;

  // Determine query range: use ?month=YYYY-MM or default to current month
  const url = new URL(request.url);
  const monthParam = url.searchParams.get("month") ?? isoMonth(new Date());
  const from = firstDay(monthParam);
  const to = lastDay(monthParam);

  const recordsResult = await apiListAttendanceRecords(incomingCookie, {
    employee_id: me.user.id,
    from,
    to,
  });

  const responseHeaders = new Headers();
  for (const cookie of meResult.setCookieHeaders) {
    responseHeaders.append("Set-Cookie", cookie);
  }

  if (recordsResult.status === 403) {
    return data<LoaderData>(
      { me, records: [], from, to, permissionDenied: true },
      { headers: responseHeaders },
    );
  }

  return data<LoaderData>(
    {
      me,
      records: recordsResult.data?.records ?? [],
      from,
      to,
      permissionDenied: false,
    },
    { headers: responseHeaders },
  );
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function formatTime(iso: string | null): string {
  if (!iso) return "—";
  const d = new Date(iso);
  return d.toLocaleTimeString("ja-JP", { hour: "2-digit", minute: "2-digit" });
}

function formatMinutes(min: number): string {
  const h = Math.floor(min / 60);
  const m = min % 60;
  return h > 0 ? `${h}時間${m}分` : `${m}分`;
}

function statusLabel(status: string): string {
  const map: Record<string, string> = {
    present: "出勤",
    absent: "欠勤",
    holiday: "休日",
    closed: "確定",
  };
  return map[status] ?? status;
}

function statusColor(status: string): React.CSSProperties {
  switch (status) {
    case "present":
      return { backgroundColor: "#dcfce7", color: "#166534" };
    case "absent":
      return { backgroundColor: "#fee2e2", color: "#991b1b" };
    case "holiday":
      return { backgroundColor: "#fef9c3", color: "#713f12" };
    default:
      return { backgroundColor: "#f3f4f6", color: "#374151" };
  }
}

// ---------------------------------------------------------------------------
// Component
// ---------------------------------------------------------------------------

export default function AttendancePage() {
  const { me, records, from, to, permissionDenied } =
    useLoaderData<LoaderData>();

  const monthDisplay = from.slice(0, 7).replace("-", "年") + "月";

  // Compute previous/next month navigation
  const [fromYear, fromMonth] = from.split("-").map(Number);
  const prevDate = new Date(fromYear, fromMonth - 2, 1);
  const nextDate = new Date(fromYear, fromMonth, 1);
  const prevMonth = isoMonth(prevDate);
  const nextMonth = isoMonth(nextDate);

  return (
    <AppLayout displayName={me.user.displayName}>
      <div style={styles.pageHeader}>
        <h1 style={styles.heading}>勤怠管理</h1>
        <Link to="/attendance/clock-in" style={styles.primaryBtn}>
          打刻する
        </Link>
      </div>

      {/* Month navigation */}
      <nav
        aria-label="月別ナビゲーション"
        style={styles.monthNav}
      >
        <Link
          to={`/attendance?month=${prevMonth}`}
          style={styles.monthNavBtn}
          aria-label={`前の月 (${prevMonth})`}
        >
          &#8249; 前月
        </Link>
        <h2 style={styles.monthHeading} aria-live="polite">
          {monthDisplay}の勤怠
        </h2>
        <Link
          to={`/attendance?month=${nextMonth}`}
          style={styles.monthNavBtn}
          aria-label={`次の月 (${nextMonth})`}
        >
          翌月 &#8250;
        </Link>
      </nav>

      {permissionDenied && (
        <div role="alert" style={styles.errorCard}>
          <strong>アクセス権限がありません。</strong>
          <p style={{ margin: "0.25rem 0 0" }}>
            勤怠記録の閲覧には <code>attendance:read</code> 権限が必要です。
          </p>
        </div>
      )}

      {!permissionDenied && records.length === 0 && (
        <div style={styles.emptyState}>
          <p>{from.slice(0, 7)} の勤怠記録はまだありません。</p>
          <Link to="/attendance/clock-in" style={styles.linkBtn}>
            打刻して記録を作成する
          </Link>
        </div>
      )}

      {!permissionDenied && records.length > 0 && (
        <div style={styles.tableWrapper}>
          {/* Table with responsive horizontal scroll */}
          <table
            style={styles.table}
            aria-label={`${monthDisplay}の勤怠記録`}
          >
            <thead>
              <tr>
                <th scope="col" style={styles.th}>日付</th>
                <th scope="col" style={styles.th}>出勤</th>
                <th scope="col" style={styles.th}>退勤</th>
                <th scope="col" style={styles.th}>休憩</th>
                <th scope="col" style={styles.th}>勤務時間</th>
                <th scope="col" style={styles.th}>残業</th>
                <th scope="col" style={styles.th}>状態</th>
              </tr>
            </thead>
            <tbody>
              {records.map((rec) => (
                <tr key={rec.id} style={styles.tr}>
                  <td style={styles.td}>{rec.work_date}</td>
                  <td style={styles.td}>{formatTime(rec.clock_in)}</td>
                  <td style={styles.td}>{formatTime(rec.clock_out)}</td>
                  <td style={styles.td}>{formatMinutes(rec.break_minutes)}</td>
                  <td style={styles.td}>{formatMinutes(rec.work_minutes)}</td>
                  <td style={{ ...styles.td, color: rec.overtime_minutes > 0 ? "#dc2626" : undefined }}>
                    {rec.overtime_minutes > 0
                      ? formatMinutes(rec.overtime_minutes)
                      : "—"}
                  </td>
                  <td style={styles.td}>
                    <span
                      style={{
                        ...styles.statusBadge,
                        ...statusColor(rec.status),
                      }}
                    >
                      {statusLabel(rec.status)}
                    </span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <p style={styles.rangeNote}>
        表示期間: {from} 〜 {to}
      </p>
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
  monthNav: {
    display: "flex",
    alignItems: "center",
    justifyContent: "space-between",
    backgroundColor: "#ffffff",
    borderRadius: "8px",
    boxShadow: "0 1px 4px rgba(0,0,0,0.08)",
    padding: "0.875rem 1.25rem",
    marginBottom: "1.25rem",
  },
  monthNavBtn: {
    color: "#2563eb",
    textDecoration: "none",
    fontSize: "0.9rem",
    padding: "0.375rem 0.5rem",
    borderRadius: "4px",
  },
  monthHeading: {
    margin: 0,
    fontSize: "1.125rem",
    fontWeight: 600,
    color: "#111827",
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
    marginBottom: "1rem",
    WebkitOverflowScrolling: "touch" as unknown as undefined,
  },
  table: {
    width: "100%",
    minWidth: "600px",
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
  rangeNote: {
    fontSize: "0.8rem",
    color: "#9ca3af",
    margin: "0.5rem 0 0",
  },
} as const;
