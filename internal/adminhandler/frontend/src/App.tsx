import { useCallback, useEffect, useMemo, useState, type FormEvent } from "react";
import { Navigate, Route, Routes, useLocation, useNavigate } from "react-router-dom";
import { useIsFetching, useQueryClient } from "@tanstack/react-query";
import { AsideHeader, type AsideHeaderItem } from "@gravity-ui/navigation";
import {
  Button,
  Card,
  Flex,
  Icon,
  PasswordInput,
  Text,
  Tooltip,
  type IconData,
  type Theme,
} from "@gravity-ui/uikit";
import {
  ArrowRightFromSquare,
  ArrowsRotateRight,
  Bucket,
  ChartMixed,
  Display,
  Key,
  Moon,
  Server,
  Sun,
} from "@gravity-ui/icons";
import { clearToken, getToken, setToken, subscribe } from "./lib/auth";
import { useGetInfo } from "./api/admin";
import { INFO_POLL } from "./lib/poll";
import { useAppTheme } from "./lib/theme";
import { GoFasterMark } from "./components/GoFasterMark";
import { Vitals } from "./components/Vitals";
import Overview from "./pages/Overview";
import AccessKeys from "./pages/AccessKeys";
import Cluster from "./pages/Cluster";
import Buckets from "./pages/Buckets";

// useToken re-renders when the stored admin token changes (sign-in/sign-out, or
// a 401 clearing a stale token).
function useToken(): string {
  const [token, setLocal] = useState(getToken());
  useEffect(() => subscribe(() => setLocal(getToken())), []);
  return token;
}

// Four destinations, so the list stays flat: AsideHeader's menu groups put
// their children behind a popup rather than inlining them, which would hide
// three of these four behind an extra click.
const NAV: { id: string; title: string; icon: IconData }[] = [
  { id: "/", title: "Overview", icon: ChartMixed },
  { id: "/access-keys", title: "Access keys", icon: Key },
  { id: "/cluster", title: "Cluster", icon: Server },
  { id: "/buckets", title: "Buckets", icon: Bucket },
];

const THEME_CYCLE: { next: Theme; icon: IconData; hint: string }[] = [
  { next: "light", icon: Display, hint: "Theme: system" },
  { next: "dark", icon: Sun, hint: "Theme: light" },
  { next: "system", icon: Moon, hint: "Theme: dark" },
];

function ThemeButton() {
  const { theme, setTheme } = useAppTheme();
  const current = THEME_CYCLE[theme === "light" ? 1 : theme === "dark" ? 2 : 0];
  return (
    <Tooltip content={`${current.hint} — click to switch`}>
      <Button view="flat" onClick={() => setTheme(current.next)} aria-label="Switch theme">
        <Icon data={current.icon} />
      </Button>
    </Tooltip>
  );
}

/**
 * The token gate. The admin API is bearer-protected; the operator pastes the
 * token once and it stays in this browser's localStorage, so the console
 * survives a reload without the server holding a session.
 */
function Gate() {
  const [value, setValue] = useState("");

  const submit = (e: FormEvent) => {
    e.preventDefault();
    setToken(value.trim());
  };

  return (
    <Flex className="gate" justifyContent="center" alignItems="center">
      <Card view="outlined" className="gate__card">
        <form onSubmit={submit}>
          <Flex direction="column" gap={5}>
            <Flex alignItems="center" gap={3}>
              <GoFasterMark width={28} height={28} />
              <Text variant="header-1">fs admin</Text>
            </Flex>

            <Text variant="body-1" color="secondary">
              Enter the admin API token. It is kept in this browser and sent as a bearer token to
              the admin API — nothing else stores it.
            </Text>

            <PasswordInput
              autoFocus
              size="l"
              value={value}
              onUpdate={setValue}
              placeholder="FS_ADMIN_TOKEN"
              controlProps={{ "aria-label": "Admin token" }}
            />

            <Button type="submit" view="action" size="l" disabled={!value.trim()} width="max">
              Continue
            </Button>
          </Flex>
        </form>
      </Card>
    </Flex>
  );
}

function TopBar({ section }: { section: string }) {
  const qc = useQueryClient();
  const fetching = useIsFetching();

  return (
    <Flex className="topbar" alignItems="center" gap={3}>
      <span className="label-micro">{section}</span>
      <Flex grow />
      <ThemeButton />
      <Button
        view="outlined"
        loading={fetching > 0}
        onClick={() => qc.invalidateQueries()}
        aria-label="Refresh all data"
      >
        <Icon data={ArrowsRotateRight} />
        Refresh
      </Button>
    </Flex>
  );
}

function Console() {
  const navigate = useNavigate();
  const { pathname } = useLocation();
  const [compact, setCompact] = useState(false);
  const info = useGetInfo({ query: { refetchInterval: INFO_POLL } });

  const menuItems = useMemo<AsideHeaderItem[]>(
    () =>
      NAV.map((item) => ({
        id: item.id,
        title: item.title,
        icon: item.icon,
        current: pathname === item.id,
        onItemClick: () => navigate(item.id),
      })),
    [pathname, navigate],
  );

  const section = NAV.find((item) => item.id === pathname)?.title ?? "Overview";

  const renderContent = useCallback(
    () => (
      <Flex direction="column" className="content">
        <TopBar section={section} />
        <Vitals />
        <div className="page">
          <Routes>
            <Route path="/" element={<Overview />} />
            <Route path="/access-keys" element={<AccessKeys />} />
            <Route path="/cluster" element={<Cluster />} />
            <Route path="/buckets" element={<Buckets />} />
            <Route path="*" element={<Navigate to="/" replace />} />
          </Routes>
        </div>
      </Flex>
    ),
    [section],
  );

  const renderFooter = useCallback(
    () => (
      <Flex direction="column" gap={2} spacing={{ px: 3, pb: 3 }} className="aside-footer">
        {!compact && info.data && (
          <Flex direction="column" spacing={{ px: 1 }}>
            <Text variant="caption-2" color="hint" ellipsis>
              {info.data.version || "dev"}
            </Text>
            <Text variant="caption-2" color="hint" ellipsis>
              {info.data.os}/{info.data.arch}
            </Text>
          </Flex>
        )}
        <Button
          view="flat"
          width={compact ? undefined : "max"}
          onClick={() => clearToken()}
          aria-label="Sign out"
        >
          <Icon data={ArrowRightFromSquare} />
          {compact ? null : "Sign out"}
        </Button>
      </Flex>
    ),
    [compact, info.data],
  );

  return (
    <AsideHeader
      logo={{
        text: "fs",
        icon: GoFasterMark,
        iconSize: 24,
        onClick: () => navigate("/"),
      }}
      compact={compact}
      onChangeCompact={setCompact}
      headerDecoration
      menuItems={menuItems}
      renderContent={renderContent}
      renderFooter={renderFooter}
    />
  );
}

export default function App() {
  const token = useToken();
  return token ? <Console /> : <Gate />;
}
