import { useEffect, useState, type FormEvent } from "react";
import { NavLink, Navigate, Route, Routes } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import { getToken, setToken, clearToken, subscribe } from "./lib/auth";
import { useGetInfo } from "./api/admin";
import MatrixRain from "./components/MatrixRain";
import Overview from "./pages/Overview";
import AccessKeys from "./pages/AccessKeys";

// useToken re-renders when the stored admin token changes (login/logout, or a
// 401 clearing a stale token).
function useToken(): string {
  const [token, setLocal] = useState(getToken());
  useEffect(() => subscribe(() => setLocal(getToken())), []);
  return token;
}

function Brand() {
  return (
    <div className="brand">
      <span className="logo">fs</span>
      <span className="tag">admin</span>
    </div>
  );
}

function Gate() {
  const [value, setValue] = useState("");

  const submit = (e: FormEvent) => {
    e.preventDefault();
    setToken(value.trim());
  };

  return (
    <div className="gate">
      <form className="card" onSubmit={submit}>
        <Brand />
        <p className="lead">
          Enter the admin API token to manage access keys. It is stored in this
          browser only and sent as a bearer token to the admin API.
        </p>
        <div className="field">
          <label htmlFor="token">Admin token</label>
          <input
            id="token"
            type="password"
            autoFocus
            value={value}
            onChange={(e) => setValue(e.target.value)}
            placeholder="FS_ADMIN_TOKEN"
          />
        </div>
        <button className="primary" type="submit" disabled={!value.trim()}>
          Continue
        </button>
      </form>
    </div>
  );
}

function Topbar() {
  const qc = useQueryClient();
  const info = useGetInfo();

  const meta = info.data
    ? `${info.data.version} · ${info.data.go_version}`
    : "go-faster/fs";

  return (
    <header className="topbar">
      <h1>fs</h1>
      <span className="sub">Admin Panel</span>
      <span className="spacer" />
      <span className="meta">{meta}</span>
      <button
        onClick={() => void qc.invalidateQueries()}
        disabled={info.isFetching}
      >
        {info.isFetching ? "Refreshing…" : "Refresh"}
      </button>
    </header>
  );
}

function Dot() {
  return <span className="ic" aria-hidden="true" />;
}

function Layout() {
  const navClass = ({ isActive }: { isActive: boolean }) =>
    isActive ? "nav-item active" : "nav-item";

  return (
    <div className="shell">
      <aside className="sidebar">
        <Brand />
        <div className="nav-group">Instance</div>
        <NavLink className={navClass} to="/" end>
          <Dot />
          Overview
        </NavLink>
        <NavLink className={navClass} to="/access-keys">
          <Dot />
          Access keys
        </NavLink>
        <div className="foot">
          <button onClick={() => clearToken()}>Sign out</button>
        </div>
      </aside>

      <div className="content">
        <Topbar />
        <div className="page">
          <div className="matrix-hero">
            <MatrixRain />
            <div className="matrix-hero__content">
              <span className="matrix-hero__title">fs admin</span>
              <span className="matrix-hero__subtitle">
                go-faster/fs · S3 access control
              </span>
            </div>
          </div>

          <Routes>
            <Route path="/" element={<Overview />} />
            <Route path="/access-keys" element={<AccessKeys />} />
            <Route path="*" element={<Navigate to="/" replace />} />
          </Routes>
        </div>
      </div>
    </div>
  );
}

export default function App() {
  const token = useToken();
  return token ? <Layout /> : <Gate />;
}
