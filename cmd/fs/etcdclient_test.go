package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clusterCfg is a valid cluster-mode config with the given etcd settings.
func clusterCfg(t *testing.T, etcd EtcdConfig) Config {
	t.Helper()

	cfg := DefaultConfig()
	cfg.Storage.Type = StorageTypeCluster
	cfg.Cluster = ClusterConfig{
		NodeID:        "node-0",
		AdvertiseAddr: "node-0.fs:7080",
		Secret:        "0123456789abcdef0123456789abcdef",
		Etcd:          etcd,
	}

	if len(cfg.Cluster.Etcd.Endpoints) == 0 {
		cfg.Cluster.Etcd.Endpoints = []string{"http://127.0.0.1:2379"}
	}

	return cfg
}

// writeCert writes a self-signed certificate and its key, returning both paths.
func writeCert(t *testing.T) (certPath, keyPath string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "fs-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)

	dir := t.TempDir()
	certPath = filepath.Join(dir, "tls.crt")
	keyPath = filepath.Join(dir, "tls.key")

	require.NoError(t, os.WriteFile(certPath,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))

	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(keyPath,
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))

	return certPath, keyPath
}

// TestEtcdTLSPlaintextByDefault: an http endpoint with nothing configured
// stays plaintext, which is what every existing deployment does.
func TestEtcdTLSPlaintextByDefault(t *testing.T) {
	cfg := clusterCfg(t, EtcdConfig{Endpoints: []string{"http://127.0.0.1:2379"}})

	client, err := cfg.etcdClientConfig()
	require.NoError(t, err)
	assert.Nil(t, client.TLS, "an http endpoint with no TLS material must stay plaintext")
}

// TestEtcdTLSFromEndpointScheme is the trap this exists to close: the etcd
// client reads the transport off this config and ignores the URL, so an https
// endpoint with a nil TLS config connects in cleartext to a TLS port.
func TestEtcdTLSFromEndpointScheme(t *testing.T) {
	cfg := clusterCfg(t, EtcdConfig{Endpoints: []string{"https://etcd.example:2379"}})

	client, err := cfg.etcdClientConfig()
	require.NoError(t, err)
	require.NotNil(t, client.TLS, "an https endpoint must enable TLS on its own")
	assert.Nil(t, client.TLS.RootCAs, "with no ca_file it should verify against the system roots")
	assert.False(t, client.TLS.InsecureSkipVerify)
}

func TestEtcdTLSLoadsMaterial(t *testing.T) {
	certPath, keyPath := writeCert(t)

	cfg := clusterCfg(t, EtcdConfig{
		Endpoints: []string{"http://127.0.0.1:2379"},
		TLS: EtcdTLSConfig{
			CAFile:     certPath,
			CertFile:   certPath,
			KeyFile:    keyPath,
			ServerName: "etcd.internal",
		},
	})

	client, err := cfg.etcdClientConfig()
	require.NoError(t, err)
	require.NotNil(t, client.TLS)

	assert.NotNil(t, client.TLS.RootCAs, "ca_file should be loaded into the root pool")
	assert.Len(t, client.TLS.Certificates, 1, "the client certificate should be loaded")
	assert.Equal(t, "etcd.internal", client.TLS.ServerName)
	assert.GreaterOrEqual(t, client.TLS.MinVersion, uint16(0x0303), "TLS 1.2 minimum")
}

// TestEtcdTLSReportsBadMaterial: a wrong path or a file that is not a
// certificate has to fail at startup, not at the first reconnect.
func TestEtcdTLSReportsBadMaterial(t *testing.T) {
	t.Run("missing ca file", func(t *testing.T) {
		cfg := clusterCfg(t, EtcdConfig{TLS: EtcdTLSConfig{CAFile: "/nonexistent/ca.pem"}})

		_, err := cfg.etcdClientConfig()
		require.ErrorContains(t, err, "ca_file")
	})

	t.Run("ca file with no certificates", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "empty.pem")
		require.NoError(t, os.WriteFile(path, []byte("not a certificate"), 0o600))

		cfg := clusterCfg(t, EtcdConfig{TLS: EtcdTLSConfig{CAFile: path}})

		_, err := cfg.etcdClientConfig()
		require.ErrorContains(t, err, "no certificates")
	})
}

func TestEtcdAuthFromConfigAndEnv(t *testing.T) {
	cfg := clusterCfg(t, EtcdConfig{
		Auth: EtcdAuthConfig{Username: "fs-node", Password: "from-file"},
	})

	client, err := cfg.etcdClientConfig()
	require.NoError(t, err)
	assert.Equal(t, "fs-node", client.Username)
	assert.Equal(t, "from-file", client.Password)

	// The env override is how a deployment keeps the password out of the
	// config file it ships.
	t.Setenv("FS_ETCD_USERNAME", "env-user")
	t.Setenv("FS_ETCD_PASSWORD", "env-secret")

	client, err = cfg.etcdClientConfig()
	require.NoError(t, err)
	assert.Equal(t, "env-user", client.Username)
	assert.Equal(t, "env-secret", client.Password)
}

// TestValidateEtcdSecurity covers the half-configured cases, which otherwise
// surface as a handshake failure with no hint of which half is missing.
func TestValidateEtcdSecurity(t *testing.T) {
	certPath, keyPath := writeCert(t)

	for name, tc := range map[string]struct {
		etcd    EtcdConfig
		env     map[string]string
		wantErr string
	}{
		"no security is valid": {},
		"complete client certificate": {
			etcd: EtcdConfig{TLS: EtcdTLSConfig{CertFile: certPath, KeyFile: keyPath}},
		},
		"certificate without key": {
			etcd:    EtcdConfig{TLS: EtcdTLSConfig{CertFile: certPath}},
			wantErr: "cert_file and key_file must be set together",
		},
		"key without certificate": {
			etcd:    EtcdConfig{TLS: EtcdTLSConfig{KeyFile: keyPath}},
			wantErr: "cert_file and key_file must be set together",
		},
		"skip verify with a ca is contradictory": {
			etcd:    EtcdConfig{TLS: EtcdTLSConfig{CAFile: certPath, InsecureSkipVerify: true}},
			wantErr: "insecure_skip_verify makes ca_file meaningless",
		},
		"complete credentials": {
			etcd: EtcdConfig{Auth: EtcdAuthConfig{Username: "fs-node", Password: "hunter2"}},
		},
		"password without username": {
			etcd:    EtcdConfig{Auth: EtcdAuthConfig{Password: "hunter2"}},
			wantErr: "username and password must be set together",
		},
		"username without password": {
			etcd:    EtcdConfig{Auth: EtcdAuthConfig{Username: "fs-node"}},
			wantErr: "username and password must be set together",
		},
		"env supplies the missing half": {
			etcd: EtcdConfig{Auth: EtcdAuthConfig{Username: "fs-node"}},
			env:  map[string]string{"FS_ETCD_PASSWORD": "hunter2"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			cfg := clusterCfg(t, tc.etcd)

			err := cfg.Validate()
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)

				return
			}

			require.NoError(t, err)
		})
	}
}
