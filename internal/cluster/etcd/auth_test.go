package etcd_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster/etcd"
)

func TestAuthKeyCRUD(t *testing.T) {
	t.Parallel()

	client := startEtcd(t)
	ctx := context.Background()
	cfg := etcd.Config{Prefix: "/fs"}

	rec := etcd.AuthRecord{
		AccessKey:    "AKIAEXAMPLE",
		SecretSealed: "sealed-blob",
		Grants:       []etcd.AuthGrant{{Pattern: "*", Permission: "admin"}},
		CreatedAt:    time.Now().UTC().Truncate(time.Second),
	}

	created, err := etcd.CreateAuthKey(ctx, client, cfg, rec)
	require.NoError(t, err)
	assert.True(t, created)

	// Creating the same access key again is rejected without error.
	created, err = etcd.CreateAuthKey(ctx, client, cfg, rec)
	require.NoError(t, err)
	assert.False(t, created, "duplicate access key must not be created")

	list, _, err := etcd.ListAuthKeys(ctx, client, cfg)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, rec, list[0])

	deleted, err := etcd.DeleteAuthKey(ctx, client, cfg, rec.AccessKey)
	require.NoError(t, err)
	assert.True(t, deleted)

	// Deleting a missing key reports not-found without error.
	deleted, err = etcd.DeleteAuthKey(ctx, client, cfg, rec.AccessKey)
	require.NoError(t, err)
	assert.False(t, deleted)

	list, _, err = etcd.ListAuthKeys(ctx, client, cfg)
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestAuthSourcePropagates(t *testing.T) {
	t.Parallel()

	client := startEtcd(t)
	ctx := context.Background()
	cfg := etcd.Config{Prefix: "/fs"}

	// A record present before the source starts must appear in the initial load.
	first := etcd.AuthRecord{AccessKey: "AKIAFIRST", SecretSealed: "s1"}
	created, err := etcd.CreateAuthKey(ctx, client, cfg, first)
	require.NoError(t, err)
	require.True(t, created)

	changes := make(chan []etcd.AuthRecord, 16)
	src, err := etcd.NewAuthSource(ctx, client, cfg, func(snap etcd.AuthSnapshot) {
		// Copy: the slice is owned by the source after the call returns.
		cp := append([]etcd.AuthRecord(nil), snap.Records...)
		changes <- cp
	})
	require.NoError(t, err)

	defer func() { _ = src.Close() }()

	// Initial snapshot.
	got := waitForKeys(t, changes, "AKIAFIRST")
	assert.Equal(t, "s1", got["AKIAFIRST"].SecretSealed)

	// Adding a key propagates through the watch.
	second := etcd.AuthRecord{AccessKey: "AKIASECOND", SecretSealed: "s2"}
	created, err = etcd.CreateAuthKey(ctx, client, cfg, second)
	require.NoError(t, err)
	require.True(t, created)

	waitForKeys(t, changes, "AKIAFIRST", "AKIASECOND")

	// Deleting a key propagates too.
	deleted, err := etcd.DeleteAuthKey(ctx, client, cfg, "AKIAFIRST")
	require.NoError(t, err)
	require.True(t, deleted)

	got = waitForKeys(t, changes, "AKIASECOND")
	assert.Equal(t, "s2", got["AKIASECOND"].SecretSealed)

	assert.Equal(t, []etcd.AuthRecord{second}, src.Snapshot().Records)
}

func TestPublicReadPropagates(t *testing.T) {
	t.Parallel()

	client := startEtcd(t)
	ctx := context.Background()
	cfg := etcd.Config{Prefix: "/fs"}

	// No list stored yet: LoadPublicRead reports absence, distinct from empty.
	_, present, err := etcd.LoadPublicRead(ctx, client, cfg)
	require.NoError(t, err)
	assert.False(t, present)

	// Seed once; a second seed with a different list is a no-op (absence CAS).
	seeded, err := etcd.SeedPublicRead(ctx, client, cfg, []string{"assets"})
	require.NoError(t, err)
	assert.True(t, seeded)

	seeded, err = etcd.SeedPublicRead(ctx, client, cfg, []string{"other"})
	require.NoError(t, err)
	assert.False(t, seeded)

	changes := make(chan []string, 16)
	src, err := etcd.NewAuthSource(ctx, client, cfg, func(snap etcd.AuthSnapshot) {
		changes <- append([]string(nil), snap.PublicRead...)
	})
	require.NoError(t, err)

	defer func() { _ = src.Close() }()

	assert.Equal(t, []string{"assets"}, waitForPublicRead(t, changes, "assets"))

	// Updating the list propagates through the watch.
	require.NoError(t, etcd.SetPublicRead(ctx, client, cfg, []string{"assets", "downloads"}))
	assert.Equal(t, []string{"assets", "downloads"}, waitForPublicRead(t, changes, "assets", "downloads"))

	// The snapshot exposes both credentials and public-read together.
	assert.Equal(t, []string{"assets", "downloads"}, src.Snapshot().PublicRead)
}

// waitForPublicRead drains change events until one matches want exactly.
func waitForPublicRead(t *testing.T, changes <-chan []string, want ...string) []string {
	t.Helper()

	deadline := time.After(15 * time.Second)

	for {
		select {
		case got := <-changes:
			if assert.ObjectsAreEqual(want, got) {
				return got
			}
		case <-deadline:
			t.Fatalf("timed out waiting for public-read %v", want)
		}
	}
}

// waitForKeys drains change events until one matches exactly the wanted access
// keys, returning that snapshot keyed by access key.
func waitForKeys(t *testing.T, changes <-chan []etcd.AuthRecord, want ...string) map[string]etcd.AuthRecord {
	t.Helper()

	deadline := time.After(15 * time.Second)

	for {
		select {
		case records := <-changes:
			byKey := make(map[string]etcd.AuthRecord, len(records))
			for _, rec := range records {
				byKey[rec.AccessKey] = rec
			}

			if len(byKey) != len(want) {
				continue
			}

			ok := true

			for _, k := range want {
				if _, has := byKey[k]; !has {
					ok = false
					break
				}
			}

			if ok {
				return byKey
			}
		case <-deadline:
			t.Fatalf("timed out waiting for keys %v", want)
		}
	}
}
