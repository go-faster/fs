import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import {
  Alert,
  Button,
  ClipboardButton,
  Dialog,
  Flex,
  Icon,
  Label,
  Table,
  Text,
  TextInput,
  Select,
  useToaster,
} from "@gravity-ui/uikit";
import type { LabelProps, TableColumnConfig } from "@gravity-ui/uikit";
import { Plus, TrashBin, Xmark } from "@gravity-ui/icons";
import {
  getGetPublicReadBucketsQueryKey,
  getListAccessKeysQueryKey,
  useCreateAccessKey,
  useDeleteAccessKey,
  useGetPublicReadBuckets,
  useListAccessKeys,
  useSetPublicReadBuckets,
} from "../api/admin";
import type { AccessKey, CreatedAccessKey, Grant, Permission } from "../api/model";
import { ApiError } from "../lib/fetcher";
import { errText, ErrorAlert, head, Loading, Mono, Panel, Rule } from "../components/ui";
import { fmtTime } from "../lib/format";
import { INFO_POLL } from "../lib/poll";

const PERMISSIONS: Permission[] = ["read", "write", "admin"];

const PERMISSION_OPTIONS = PERMISSIONS.map((p) => ({ value: p, content: p }));

// The stronger the grant, the louder the chip — a key that can administer
// buckets should not look like one that can only read them.
const PERMISSION_THEMES: Record<Permission, LabelProps["theme"]> = {
  read: "unknown",
  write: "info",
  admin: "warning",
};

const EMPTY_GRANT: Grant = { bucket: "*", permission: "read" };

function GrantChips({ grants }: { grants: Grant[] }) {
  if (grants.length === 0) {
    return (
      <Text variant="body-1" color="hint">
        none
      </Text>
    );
  }
  return (
    <Flex gap={1} wrap>
      {grants.map((g, i) => (
        <Label key={i} theme={PERMISSION_THEMES[g.permission]} size="xs">
          {g.bucket}:{g.permission}
        </Label>
      ))}
    </Flex>
  );
}

/** One half of a new credential: named, whole, and one click from the clipboard. */
function Secret({ label, value }: { label: string; value: string }) {
  return (
    <Flex direction="column" gap={1}>
      <span className="label-micro">{label}</span>
      <div className="secret">
        <span className="secret__value">{value}</span>
        <ClipboardButton text={value} size="s" />
      </div>
    </Flex>
  );
}

/**
 * The create dialog. It has two faces: the form, and — once the server has
 * minted the credential — the secret, which is the only time it is ever shown.
 * Keeping both in one dialog means the operator cannot navigate away from the
 * secret by accident; it takes a deliberate dismiss.
 */
function CreateDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const qc = useQueryClient();
  const toaster = useToaster();
  const [accessKey, setAccessKey] = useState("");
  const [grants, setGrants] = useState<Grant[]>([EMPTY_GRANT]);
  const [created, setCreated] = useState<CreatedAccessKey | null>(null);

  const create = useCreateAccessKey({
    mutation: {
      onSuccess: (c) => {
        setCreated(c);
        void qc.invalidateQueries({ queryKey: getListAccessKeysQueryKey() });
      },
      onError: (err) =>
        toaster.add({
          name: "create-access-key",
          theme: "danger",
          title: "Couldn't create the key",
          content: errText(err),
          autoHiding: false,
        }),
    },
  });

  const close = () => {
    onClose();
    // Reset only after the dialog is dismissed, so the form does not flash
    // back into view behind the closing animation.
    setAccessKey("");
    setGrants([EMPTY_GRANT]);
    setCreated(null);
  };

  const setGrant = (i: number, patch: Partial<Grant>) =>
    setGrants((prev) => prev.map((g, idx) => (idx === i ? { ...g, ...patch } : g)));

  const submit = () => {
    const cleaned = grants
      .map((g) => ({ ...g, bucket: g.bucket.trim() }))
      .filter((g) => g.bucket !== "");
    if (cleaned.length === 0) {
      toaster.add({
        name: "create-access-key",
        theme: "warning",
        title: "Add a grant",
        content: "A key needs at least one bucket pattern to be worth anything.",
      });
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
    <Dialog
      open={open}
      onClose={close}
      onEnterKeyDown={created ? close : submit}
      maxWidth="m"
      fullWidth
    >
      <Dialog.Header caption={created ? "Key created" : "Create access key"} />
      <Dialog.Body>
        {created ? (
          <Flex direction="column" gap={4}>
            <Alert
              theme="warning"
              view="outlined"
              title="Copy the secret now"
              message="It is shown this once. The server keeps only what it needs to verify a signature, so it cannot show it again."
            />
            <Secret label="access key" value={created.access_key} />
            <Secret label="secret key" value={created.secret_key} />
          </Flex>
        ) : (
          <Flex direction="column" gap={5}>
            <TextInput
              label="Access key ID"
              note="Generated when left blank."
              value={accessKey}
              onUpdate={setAccessKey}
              placeholder="AKIA…"
            />

            <Flex direction="column" gap={2}>
              <span className="label-micro">Grants</span>
              {grants.map((g, i) => (
                <div className="grant-row" key={i}>
                  <TextInput
                    value={g.bucket}
                    onUpdate={(bucket) => setGrant(i, { bucket })}
                    placeholder="bucket pattern, e.g. uploads-*"
                    controlProps={{ "aria-label": `Bucket pattern for grant ${i + 1}` }}
                  />
                  <Select
                    value={[g.permission]}
                    options={PERMISSION_OPTIONS}
                    onUpdate={([permission]) =>
                      setGrant(i, { permission: permission as Permission })
                    }
                    width="max"
                    aria-label={`Permission for grant ${i + 1}`}
                  />
                  <Button
                    view="flat"
                    onClick={() => setGrants((prev) => prev.filter((_, idx) => idx !== i))}
                    disabled={grants.length === 1}
                    aria-label={`Remove grant ${i + 1}`}
                  >
                    <Icon data={Xmark} />
                  </Button>
                </div>
              ))}
              <Flex>
                <Button view="outlined" onClick={() => setGrants((prev) => [...prev, EMPTY_GRANT])}>
                  <Icon data={Plus} />
                  Add grant
                </Button>
              </Flex>
            </Flex>

            <Text variant="body-1" color="secondary">
              A pattern is a bucket glob: <Mono>*</Mono> matches every bucket.
            </Text>
          </Flex>
        )}
      </Dialog.Body>
      {created ? (
        <Dialog.Footer textButtonApply="Done" onClickButtonApply={close} />
      ) : (
        <Dialog.Footer
          textButtonApply="Create key"
          textButtonCancel="Cancel"
          onClickButtonApply={submit}
          onClickButtonCancel={close}
          loading={create.isPending}
        />
      )}
    </Dialog>
  );
}

/** Confirms an irreversible delete by naming exactly what is about to go. */
function DeleteDialog({
  accessKey,
  onClose,
}: {
  accessKey: string | null;
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const toaster = useToaster();

  const del = useDeleteAccessKey({
    mutation: {
      onSuccess: () => {
        toaster.add({
          name: "delete-access-key",
          theme: "success",
          title: "Key deleted",
          content: `${accessKey} will no longer sign requests.`,
        });
        void qc.invalidateQueries({ queryKey: getListAccessKeysQueryKey() });
        onClose();
      },
      onError: (err) =>
        toaster.add({
          name: "delete-access-key",
          theme: "danger",
          title: "Couldn't delete the key",
          content: errText(err),
          autoHiding: false,
        }),
    },
  });

  return (
    <Dialog open={accessKey !== null} onClose={onClose} maxWidth="s" fullWidth>
      <Dialog.Header caption="Delete access key" />
      <Dialog.Body>
        <Text variant="body-1">
          <Mono>{accessKey}</Mono> stops signing requests immediately, on every node. Anything
          still using it will start getting 403s. This cannot be undone.
        </Text>
      </Dialog.Body>
      <Dialog.Footer
        textButtonApply="Delete key"
        textButtonCancel="Cancel"
        onClickButtonApply={() => accessKey && del.mutate({ accessKey })}
        onClickButtonCancel={onClose}
        loading={del.isPending}
        // `preset="danger"` only recolors the footer's error text in uikit 7 —
        // the apply button stays `view="action"`. The confirm for something
        // irreversible has to look different from the one that creates a key.
        propsButtonApply={{ view: "outlined-danger" }}
      />
    </Dialog>
  );
}

/**
 * The cluster-wide public-read bucket list. It renders only with cluster-wide
 * credentials (auth.source: etcd); the endpoint returns 501 otherwise and the
 * panel hides itself. Each add and remove replaces the whole list, matching the
 * API.
 */
function PublicReadPanel() {
  const qc = useQueryClient();
  const toaster = useToaster();
  const q = useGetPublicReadBuckets({ query: { retry: false } });
  const [draft, setDraft] = useState("");

  const save = useSetPublicReadBuckets({
    mutation: {
      onSuccess: () => {
        void qc.invalidateQueries({ queryKey: getGetPublicReadBucketsQueryKey() });
      },
      onError: (err) =>
        toaster.add({
          name: "public-read-buckets",
          theme: "danger",
          title: "Couldn't save the list",
          content: errText(err),
          autoHiding: false,
        }),
    },
  });

  // Not applicable without cluster-wide credentials: hide entirely on 501. The
  // fetcher throws ApiError at runtime (the generated error type is nominal).
  const err = q.error as unknown as ApiError | null;
  if (err && err.status === 501) return null;

  const buckets = q.data?.buckets ?? [];
  const replace = (next: string[]) => save.mutate({ data: { buckets: next } });

  const add = () => {
    const name = draft.trim();
    if (name === "") return;
    if (buckets.includes(name)) {
      toaster.add({
        name: "public-read-buckets",
        theme: "warning",
        title: "Already public",
        content: `${name} is already readable without credentials.`,
      });
      return;
    }
    setDraft("");
    replace([...buckets, name]);
  };

  return (
    <Flex direction="column" gap={3}>
      <Rule>Public read</Rule>
      <Panel title="Unsigned access" sub="cluster-wide, hot-reloaded">
        <Flex direction="column" gap={4}>
          <Text variant="body-1" color="secondary">
            Buckets anyone can read without credentials — unsigned GET, HEAD and list. Changes
            reach every node within seconds; no restart.
          </Text>

          {err && <ErrorAlert error={err} what="the public-read list" />}

          <Flex gap={2} wrap alignItems="center">
            {buckets.length === 0 ? (
              <Text variant="body-1" color="hint">
                No bucket is public. Every read is signed.
              </Text>
            ) : (
              buckets.map((b) => (
                <Label
                  key={b}
                  theme="warning"
                  type="close"
                  onCloseClick={() => replace(buckets.filter((x) => x !== b))}
                  disabled={save.isPending}
                >
                  {b}
                </Label>
              ))
            )}
          </Flex>

          <Flex gap={2}>
            <TextInput
              className="field-narrow"
              value={draft}
              onUpdate={setDraft}
              onKeyDown={(e) => {
                if (e.key === "Enter") add();
              }}
              placeholder="bucket name"
              controlProps={{ "aria-label": "Bucket to make public-read" }}
            />
            <Button
              view="outlined"
              onClick={add}
              loading={save.isPending}
              disabled={draft.trim() === ""}
            >
              Make public
            </Button>
          </Flex>
        </Flex>
      </Panel>
    </Flex>
  );
}

export default function AccessKeys() {
  const list = useListAccessKeys({ query: { refetchInterval: INFO_POLL } });
  const [creating, setCreating] = useState(false);
  const [deleting, setDeleting] = useState<string | null>(null);

  const columns: TableColumnConfig<AccessKey>[] = [
    {
      id: "access_key",
      name: head("access key"),
      primary: true,
      template: (k) => <Mono>{k.access_key}</Mono>,
    },
    { id: "grants", name: head("grants"), template: (k) => <GrantChips grants={k.grants} /> },
    {
      id: "source",
      name: head("source"),
      // Config keys are owned by the file on disk; the admin API will not touch
      // them, so the source column is also what says whether a row is editable.
      template: (k) => (
        <Label theme={k.source === "managed" ? "info" : "unknown"} size="xs">
          {k.source}
        </Label>
      ),
    },
    {
      id: "created_at",
      name: head("created"),
      template: (k) => (
        <Text variant="code-inline-1" color={k.created_at ? undefined : "hint"}>
          {k.created_at ? fmtTime(k.created_at) : "—"}
        </Text>
      ),
    },
    {
      id: "actions",
      name: head(""),
      align: "end",
      template: (k) =>
        k.source === "managed" ? (
          <Button
            view="flat-danger"
            size="s"
            onClick={() => setDeleting(k.access_key)}
            aria-label={`Delete ${k.access_key}`}
          >
            <Icon data={TrashBin} />
          </Button>
        ) : (
          <Text variant="caption-2" color="hint" title="Defined in the config file">
            read-only
          </Text>
        ),
    },
  ];

  const count = list.data?.keys.length ?? 0;

  return (
    <Flex direction="column" gap={5}>
      <Flex direction="column" gap={3}>
        <Rule>Credentials</Rule>
        <Panel
          title="Access keys"
          sub={list.data ? `${count} ${count === 1 ? "credential" : "credentials"}` : undefined}
          actions={
            <Button view="action" onClick={() => setCreating(true)}>
              <Icon data={Plus} />
              Create key
            </Button>
          }
          scroll={count > 0}
        >
          {list.isLoading && <Loading />}
          {list.error && <ErrorAlert error={list.error} what="access keys" />}
          {list.data && count === 0 && (
            <Text variant="body-1" color="secondary">
              No access keys. Create one to let a client sign requests to this instance.
            </Text>
          )}
          {count > 0 && (
            <Table
              data={list.data?.keys ?? []}
              columns={columns}
              getRowId="access_key"
              width="max"
            />
          )}
        </Panel>
      </Flex>

      <PublicReadPanel />

      <CreateDialog open={creating} onClose={() => setCreating(false)} />
      <DeleteDialog accessKey={deleting} onClose={() => setDeleting(null)} />
    </Flex>
  );
}
