//go:build unix

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// The WAN soak (docs/design/geo-replication.md, phase 7) runs the target
// deployment on another machine: one runs `chaos -serve-target 0.0.0.0`,
// the other `chaos -target <host> -target-ca target.crt`. The source still
// reaches the target through the fault proxy, so the faults come on top of a
// real link, and over TLS, which the proxy passes through to the target.
var (
	// serveTarget is the host the target's S3 ports listen on, in the mode
	// that runs only the target.
	serveTarget string
	// targetHost is where a remote target runs, and targetCA its certificate.
	targetHost, targetCA string
)

// targetCert is the remote target's certificate, under its -dir.
const targetCert = "target.crt"

// remoteTarget is the target deployment that -serve-target runs on host:
// its nodes always up, as far as the harness knows, which cannot kill them.
func remoteTarget(host string) (*cluster, error) {
	if targetCA == "" {
		return nil, errors.New("-target needs -target-ca, the target's " + targetCert)
	}

	pool, err := certPool(targetCA)
	if err != nil {
		return nil, err
	}

	dst := &cluster{secret: secret + "-replica", widths: []int{3}}

	for i, zone := range []string{"a", "b", "c"} {
		n := &node{id: fmt.Sprintf("t%d", i+1), zone: zone, s3: 18700 + i, host: host, member: true, up: true}

		if n.client, err = secureClient(n.s3Addr(), pool); err != nil {
			return nil, err
		}

		dst.nodes = append(dst.nodes, n)
	}

	return dst, nil
}

// certPool is the certificates in file.
func certPool(file string) (*x509.CertPool, error) {
	ca, err := os.ReadFile(file) //nolint:gosec // The file the operator named.
	if err != nil {
		return nil, err
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("no certificate in %s", file)
	}

	return pool, nil
}

// secureClient is an S3 client of a target node at addr over TLS, trusting
// pool. The certificate names 127.0.0.1, where the runner reaches it through
// the proxy, whatever addr is.
func secureClient(addr string, pool *x509.CertPool) (*minio.Client, error) {
	return minio.New(addr, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: true,
		Transport: &http.Transport{
			TLSClientConfig:       &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12},
			ResponseHeaderTimeout: 30 * time.Second,
			MaxIdleConnsPerHost:   16,
		},
	})
}

// serveTargetOnly runs the target deployment for a source on another
// machine, until SIGINT or SIGTERM.
func serveTargetOnly(bin, dir, host string) error {
	abs, err := filepath.Abs(bin)
	if err != nil {
		return err
	}

	if dir == "" {
		return errors.New("-serve-target needs -dir")
	}

	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}

	files, err := selfSigned(dir)
	if err != nil {
		return err
	}

	dst, err := newTarget(abs, dir)
	if err != nil {
		return err
	}

	dst.bind, dst.tls = host, files

	pool, err := certPool(files.cert)
	if err != nil {
		return err
	}

	for _, n := range dst.nodes {
		if n.client, err = secureClient(n.s3Addr(), pool); err != nil {
			return err
		}
	}

	defer func() {
		for _, n := range dst.nodes {
			dst.kill(n)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := startTarget(ctx, dst); err != nil {
		return err
	}

	fmt.Printf("target ready on %s:18700-18702; certificate %s\n", host, files.cert)

	<-ctx.Done()

	return nil
}

// selfSigned writes a certificate for 127.0.0.1 and localhost, its own
// authority, and its key into dir.
func selfSigned(dir string) (*tlsFiles, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "fs chaos target"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(7 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		DNSNames:              []string{"localhost"},
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}

	files := &tlsFiles{cert: filepath.Join(dir, targetCert), key: filepath.Join(dir, "target.key")}

	if err := os.WriteFile(files.cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		return nil, err
	}

	if err := os.WriteFile(files.key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return nil, err
	}

	return files, nil
}
