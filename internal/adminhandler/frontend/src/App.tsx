import { useEffect, useState, type FormEvent } from "react";
import { NavLink, Navigate, Route, Routes } from "react-router-dom";
import { getToken, setToken, clearToken, subscribe } from "./lib/auth";
import Overview from "./pages/Overview";
import AccessKeys from "./pages/AccessKeys";

// useToken re-renders when the stored admin token changes (login/logout, or a
// 401 clearing a stale token).
function useToken(): string {
  const [token, setLocal] = useState(getToken());
  useEffect(() => subscribe(() => setLocal(getToken())), []);
  return token;
}

function Gate() {
  const [value, setValue] = useState("");

  const submit = (e: FormEvent) => {
    e.preventDefault();
    setToken(value.trim());
  };

  return (
    <div className="gate">
      <form className="panel" onSubmit={submit}>
        <div className="brand">
          fs admin
          <small>go-faster/fs</small>
        </div>
        <p className="muted">
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

function Layout() {
  return (
    <div className="app">
      <aside className="sidebar">
        <div className="brand">
          fs admin
          <small>go-faster/fs</small>
        </div>
        <nav className="nav">
          <NavLink to="/" end>
            Overview
          </NavLink>
          <NavLink to="/access-keys">Access keys</NavLink>
        </nav>
        <div className="sidebar-foot">
          <button onClick={() => clearToken()}>Sign out</button>
        </div>
      </aside>
      <main className="main">
        <Routes>
          <Route path="/" element={<Overview />} />
          <Route path="/access-keys" element={<AccessKeys />} />
          <Route path="*" element={<Navigate to="/" replace />} />
        </Routes>
      </main>
    </div>
  );
}

export default function App() {
  const token = useToken();
  return token ? <Layout /> : <Gate />;
}
