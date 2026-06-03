/**
 * AppLayout — shared authenticated shell.
 *
 * Provides the top navigation bar (with logout), a skip-to-content link
 * for keyboard/screen-reader users, and a main content area.
 *
 * Responsive: collapses the nav label on narrow viewports via CSS media query.
 */

import { Form, NavLink } from "react-router";

interface AppLayoutProps {
  displayName: string;
  children: React.ReactNode;
}

export function AppLayout({ displayName, children }: AppLayoutProps) {
  return (
    <div style={layout.page}>
      {/* Skip navigation — visible on focus only (a11y: keyboard shortcut) */}
      <a href="#main-content" style={layout.skipLink}>
        メインコンテンツへスキップ
      </a>

      <header style={layout.header} role="banner">
        <div style={layout.headerInner}>
          {/* Site logo / home link */}
          <NavLink
            to="/"
            style={layout.logo}
            aria-label="HR SaaS ホームへ"
          >
            HR SaaS
          </NavLink>

          {/* Primary navigation */}
          <nav aria-label="主要ナビゲーション" style={layout.nav}>
            <ul style={layout.navList} role="list">
              <li>
                <NavLink
                  to="/"
                  end
                  style={({ isActive }) =>
                    isActive ? { ...layout.navLink, ...layout.navLinkActive } : layout.navLink
                  }
                >
                  ダッシュボード
                </NavLink>
              </li>
              <li>
                <NavLink
                  to="/attendance"
                  style={({ isActive }) =>
                    isActive ? { ...layout.navLink, ...layout.navLinkActive } : layout.navLink
                  }
                >
                  勤怠
                </NavLink>
              </li>
              <li>
                <NavLink
                  to="/selfservice/change-requests"
                  style={({ isActive }) =>
                    isActive ? { ...layout.navLink, ...layout.navLinkActive } : layout.navLink
                  }
                >
                  変更申請
                </NavLink>
              </li>
            </ul>
          </nav>

          {/* User info + logout */}
          <div style={layout.userArea}>
            <span style={layout.userName} aria-hidden="true">
              {displayName}
            </span>
            <Form method="post" action="/logout">
              <button
                type="submit"
                style={layout.logoutBtn}
                aria-label={`${displayName} をログアウト`}
              >
                ログアウト
              </button>
            </Form>
          </div>
        </div>
      </header>

      <main id="main-content" style={layout.main} tabIndex={-1}>
        {children}
      </main>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Styles
// ---------------------------------------------------------------------------

export const layout = {
  page: {
    minHeight: "100vh",
    backgroundColor: "#f5f5f5",
    fontFamily: "system-ui, -apple-system, sans-serif",
  },
  skipLink: {
    position: "absolute" as const,
    left: "-9999px",
    top: "auto",
    width: "1px",
    height: "1px",
    overflow: "hidden",
    zIndex: 9999,
    // Shown on focus
    outline: "none",
  } as React.CSSProperties,
  header: {
    backgroundColor: "#1e40af",
    color: "#ffffff",
    padding: "0 1rem",
  },
  headerInner: {
    maxWidth: "960px",
    margin: "0 auto",
    minHeight: "56px",
    display: "flex",
    alignItems: "center",
    gap: "1rem",
    flexWrap: "wrap" as const,
  },
  logo: {
    fontSize: "1.125rem",
    fontWeight: 700,
    letterSpacing: "0.05em",
    color: "#ffffff",
    textDecoration: "none",
    flexShrink: 0,
  },
  nav: {
    flex: 1,
  },
  navList: {
    display: "flex",
    flexWrap: "wrap" as const,
    gap: "0.25rem",
    listStyle: "none",
    margin: 0,
    padding: 0,
  },
  navLink: {
    color: "rgba(255,255,255,0.85)",
    textDecoration: "none",
    padding: "0.375rem 0.75rem",
    borderRadius: "6px",
    fontSize: "0.9rem",
    display: "block",
  },
  navLinkActive: {
    backgroundColor: "rgba(255,255,255,0.2)",
    color: "#ffffff",
    fontWeight: 600,
  },
  userArea: {
    display: "flex",
    alignItems: "center",
    gap: "0.75rem",
    flexShrink: 0,
    flexWrap: "wrap" as const,
  },
  userName: {
    fontSize: "0.875rem",
    color: "rgba(255,255,255,0.8)",
  },
  logoutBtn: {
    background: "transparent",
    border: "1px solid rgba(255,255,255,0.5)",
    borderRadius: "6px",
    color: "#ffffff",
    padding: "0.375rem 0.875rem",
    fontSize: "0.875rem",
    cursor: "pointer",
    whiteSpace: "nowrap" as const,
  },
  main: {
    maxWidth: "960px",
    margin: "0 auto",
    padding: "2rem 1rem",
    outline: "none",
  },
} as const;
