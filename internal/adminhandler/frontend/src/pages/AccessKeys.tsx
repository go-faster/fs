import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import {
  getListAccessKeysQueryKey,
  useCreateAccessKey,
  useDeleteAccessKey,
  useListAccessKeys,
} from "../api/admin";
import type { CreatedAccessKey, Grant, Permission } from "../api/model";
import { useToast } from "../components/toast";

const PERMISSIONS: Permission[] = ["read", "write", "admin"];

function GrantList({ grants }: { grants: Grant[] }) {
  if (grants.length === 0) return <span className="empty">none</span>;
  return (
    <>
      {grants.map((g, i) => (
        <span className="chip" key={i}>
          {g.bucket}:{g.permission}
        </span>
      ))}
    </>
  );
}

// CreatePanel is the create form. Grants are entered as bucket-pattern + level
// rows; the access key and secret are generated server-side when left blank.
function CreatePanel({ onCreated }: { onCreated: (c: CreatedAccessKey) => void }) {
  const qc = useQueryClient();
  const toast = useToast();
  const [accessKey, setAccessKey] = useState("");
  const [grants, setGrants] = useState<Grant[]>([{ bucket: "*", permission: "read" }]);

  const create = useCreateAccessKey({
    mutation: {
      onSuccess: (created) => {
        toast.notify(`Created ${created.access_key}`, "success");
        void qc.invalidateQueries({ queryKey: getListAccessKeysQueryKey() });
        onCreated(created);
        setAccessKey("");
        setGrants([{ bucket: "*", permission: "read" }]);
      },
      onError: (err) => toast.notify(err.error_message, "error"),
    },
  });

  const setGrant = (i: number, patch: Partial<Grant>) =>
    setGrants((prev) => prev.map((g, idx) => (idx === i ? { ...g, ...patch } : g)));

  const addGrant = () =>
    setGrants((prev) => [...prev, { bucket: "*", permission: "read" }]);

  const removeGrant = (i: number) =>
    setGrants((prev) => prev.filter((_, idx) => idx !== i));

  const submit = () => {
    const cleaned = grants
      .map((g) => ({ ...g, bucket: g.bucket.trim() }))
      .filter((g) => g.bucket !== "");
    if (cleaned.length === 0) {
      toast.notify("Add at least one grant with a bucket pattern", "error");
      return;
    }
    create.mutate({
      data: {
        ...(accessKey.trim() ? { access_key: accessKey.trim() } : {}),
        grants: cleaned,
      },
    });
  };

  return (
    <div className="card">
      <h2>Create access key</h2>
      <div className="field field-narrow">
        <label htmlFor="ak">Access key ID — optional</label>
        <input
          id="ak"
          value={accessKey}
          onChange={(e) => setAccessKey(e.target.value)}
          placeholder="generated when blank"
        />
      </div>

      <label>Grants</label>
      {grants.map((g, i) => (
        <div className="grant-row" key={i}>
          <input
            value={g.bucket}
            onChange={(e) => setGrant(i, { bucket: e.target.value })}
            placeholder="bucket pattern (e.g. uploads-*)"
          />
          <select
            value={g.permission}
            onChange={(e) => setGrant(i, { permission: e.target.value as Permission })}
          >
            {PERMISSIONS.map((p) => (
              <option key={p} value={p}>
                {p}
              </option>
            ))}
          </select>
          <button
            type="button"
            onClick={() => removeGrant(i)}
            disabled={grants.length === 1}
          >
            Remove
          </button>
        </div>
      ))}

      <div className="actions" style={{ marginTop: "14px" }}>
        <button type="button" onClick={addGrant}>
          Add grant
        </button>
        <span className="spacer" />
        <button className="primary" onClick={submit} disabled={create.isPending}>
          {create.isPending ? "Creating…" : "Create key"}
        </button>
      </div>
    </div>
  );
}

// SecretCard shows a newly created secret exactly once.
function SecretCard({ created, onDismiss }: { created: CreatedAccessKey; onDismiss: () => void }) {
  const toast = useToast();
  const copy = (text: string) => {
    void navigator.clipboard?.writeText(text).then(
      () => toast.notify("Copied", "success"),
      () => toast.notify("Copy failed", "error"),
    );
  };

  return (
    <div className="card">
      <h2>New credential</h2>
      <div className="secret-box">
        <div className="warn">
          Copy the secret now — it is shown once and cannot be retrieved later.
        </div>
        <dl className="kv">
          <dt>Access key</dt>
          <dd>
            {created.access_key}{" "}
            <button type="button" onClick={() => copy(created.access_key)}>
              Copy
            </button>
          </dd>
          <dt>Secret key</dt>
          <dd>
            {created.secret_key}{" "}
            <button type="button" onClick={() => copy(created.secret_key)}>
              Copy
            </button>
          </dd>
        </dl>
      </div>
      <div className="actions" style={{ marginTop: "14px" }}>
        <span className="spacer" />
        <button onClick={onDismiss}>Done</button>
      </div>
    </div>
  );
}

export default function AccessKeys() {
  const qc = useQueryClient();
  const toast = useToast();
  const list = useListAccessKeys();
  const [created, setCreated] = useState<CreatedAccessKey | null>(null);

  const del = useDeleteAccessKey({
    mutation: {
      onSuccess: () => {
        toast.notify("Access key deleted", "success");
        void qc.invalidateQueries({ queryKey: getListAccessKeysQueryKey() });
      },
      onError: (err) => toast.notify(err.error_message, "error"),
    },
  });

  const onDelete = (accessKey: string) => {
    if (!window.confirm(`Delete access key ${accessKey}? This cannot be undone.`)) {
      return;
    }
    del.mutate({ accessKey });
  };

  const count = list.data?.keys.length ?? 0;

  return (
    <>
      <div className="section-title">Access keys</div>

      {created && (
        <div style={{ marginBottom: "14px" }}>
          <SecretCard created={created} onDismiss={() => setCreated(null)} />
        </div>
      )}

      <div style={{ marginBottom: "14px" }}>
        <CreatePanel onCreated={setCreated} />
      </div>

      <div className="card">
        <h2>
          Configured credentials
          <span className="sub">
            {count} credential{count === 1 ? "" : "s"}
          </span>
        </h2>

        {list.isLoading && <div className="empty">Loading…</div>}
        {list.error && <div className="err-box">{list.error.error_message}</div>}
        {list.data && list.data.keys.length === 0 && (
          <div className="empty">No access keys.</div>
        )}
        {list.data && list.data.keys.length > 0 && (
          <div className="scroll">
            <table className="left">
              <thead>
                <tr>
                  <th>Access key</th>
                  <th>Grants</th>
                  <th>Source</th>
                  <th>Created</th>
                  <th></th>
                </tr>
              </thead>
              <tbody>
                {list.data.keys.map((k) => (
                  <tr key={k.access_key}>
                    <td>{k.access_key}</td>
                    <td>
                      <GrantList grants={k.grants} />
                    </td>
                    <td>
                      {k.source === "managed" ? (
                        <span className="chip on">managed</span>
                      ) : (
                        <span className="chip">config</span>
                      )}
                    </td>
                    <td>
                      {k.created_at
                        ? new Date(k.created_at).toLocaleString()
                        : "—"}
                    </td>
                    <td>
                      {k.source === "managed" ? (
                        <button
                          className="danger"
                          onClick={() => onDelete(k.access_key)}
                          disabled={del.isPending}
                        >
                          Delete
                        </button>
                      ) : (
                        <span className="empty" title="Defined in config">
                          read-only
                        </span>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </>
  );
}
