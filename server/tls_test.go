package server_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/server"
	"github.com/go-faster/fs/storagemem"
)

// writeSelfSignedCert generates an ECDSA self-signed certificate for
// 127.0.0.1/localhost valid for an hour, writes cert.pem and key.pem into dir,
// and returns their paths.
func writeSelfSignedCert(t *testing.T, dir, org string) (certPath, keyPath string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{Organization: []string{org}},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)

	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")

	certPEM, err := os.Create(certPath)
	require.NoError(t, err)

	require.NoError(t, pem.Encode(certPEM, &pem.Block{Type: "CERTIFICATE", Bytes: der}))
	require.NoError(t, certPEM.Close())

	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)

	keyPEM, err := os.Create(keyPath)
	require.NoError(t, err)

	require.NoError(t, pem.Encode(keyPEM, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	require.NoError(t, keyPEM.Close())

	return certPath, keyPath
}

func TestServer_TLS(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeSelfSignedCert(t, dir, "go-faster-test")

	srv, err := server.New(server.Config{
		Storage: storagemem.New(),
		Addr:    "127.0.0.1:0",
		TLS:     &server.TLSConfig{CertFile: certPath, KeyFile: keyPath},
	})
	require.NoError(t, err)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()

	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // self-signed test cert.
	}}

	url := "https://" + ln.Addr().String() + "/health"

	require.Eventually(t, func() bool {
		resp, err := client.Get(url) //nolint:noctx // test polling of a local URL.
		if err != nil {
			return false
		}

		_ = resp.Body.Close()

		return resp.StatusCode == http.StatusOK
	}, 3*time.Second, 20*time.Millisecond, "TLS server did not become ready")

	// Hot reload the certificate (new keypair) without dropping the listener.
	writeSelfSignedCert(t, dir, "go-faster-reloaded")
	require.NoError(t, srv.ReloadCertificate())

	resp, err := client.Get(url) //nolint:noctx // test fetch of a local URL.
	require.NoError(t, err)

	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	cancel()
	require.NoError(t, <-done)
}
