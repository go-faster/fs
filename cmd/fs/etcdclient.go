package main

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"strings"
	"time"

	"github.com/go-faster/errors"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// etcdDialTimeout bounds the initial connection to the control plane. A node
// that cannot reach etcd should say so rather than hang at startup.
const etcdDialTimeout = 5 * time.Second

// etcdClientConfig builds the etcd client configuration for this node: the
// endpoints, and the transport and credentials the deployment configured.
//
// Both callers go through it — the data node's runtime and the CLI/headless
// admin — because an operator that secures one and not the other has secured
// nothing.
func (c *Config) etcdClientConfig() (clientv3.Config, error) {
	cfg := clientv3.Config{
		Endpoints:   c.Cluster.Etcd.Endpoints,
		DialTimeout: etcdDialTimeout,
		Username:    c.EtcdUsername(),
		Password:    c.EtcdPassword(),
	}

	tlsCfg, err := etcdTLS(c.Cluster.Etcd)
	if err != nil {
		return clientv3.Config{}, err
	}

	cfg.TLS = tlsCfg

	return cfg, nil
}

// etcdTLS is the transport configuration, or nil for a plaintext connection.
//
// The etcd client decides the transport from this value alone: with a nil TLS
// config it speaks cleartext no matter what the endpoint URL says. So an
// "https://" endpoint turns TLS on by itself, verifying against the system
// roots, rather than letting a config that looks secure connect in the clear.
func etcdTLS(cfg EtcdConfig) (*tls.Config, error) {
	if !cfg.TLS.enabled() && !hasTLSEndpoint(cfg.Endpoints) {
		return nil, nil //nolint:nilnil // No TLS configured is a valid answer.
	}

	out := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         cfg.TLS.ServerName,
		InsecureSkipVerify: cfg.TLS.InsecureSkipVerify, //nolint:gosec // Opt-in, and documented as development-only.
	}

	if cfg.TLS.CAFile != "" {
		pem, err := os.ReadFile(cfg.TLS.CAFile)
		if err != nil {
			return nil, errors.Wrap(err, "read cluster.etcd.tls.ca_file")
		}

		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.Errorf("cluster.etcd.tls.ca_file %q contains no certificates", cfg.TLS.CAFile)
		}

		out.RootCAs = pool
	}

	if cfg.TLS.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		if err != nil {
			return nil, errors.Wrap(err, "load cluster.etcd.tls client certificate")
		}

		out.Certificates = []tls.Certificate{cert}
	}

	return out, nil
}

// hasTLSEndpoint reports whether any endpoint is an https URL.
func hasTLSEndpoint(endpoints []string) bool {
	for _, endpoint := range endpoints {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(endpoint)), "https://") {
			return true
		}
	}

	return false
}

// validateEtcdSecurity rejects the half-configured cases at startup rather
// than at the first connection, where they surface as a handshake error with
// no hint of which half is missing.
func (c *Config) validateEtcdSecurity() error {
	tlsCfg := c.Cluster.Etcd.TLS

	// A client certificate is a pair. One without the other is silently
	// ignored by crypto/tls, so the node would come up presenting nothing and
	// be refused by an etcd requiring mutual TLS.
	if (tlsCfg.CertFile == "") != (tlsCfg.KeyFile == "") {
		return errors.New("cluster.etcd.tls.cert_file and key_file must be set together")
	}

	if tlsCfg.InsecureSkipVerify && tlsCfg.CAFile != "" {
		return errors.New("cluster.etcd.tls: insecure_skip_verify makes ca_file meaningless; set one or the other")
	}

	// etcd authenticates on both, so one alone is a credential that cannot
	// work — most likely a password supplied without its user.
	if (c.EtcdUsername() == "") != (c.EtcdPassword() == "") {
		return errors.New("cluster.etcd.auth: username and password must be set together " +
			"(or FS_ETCD_USERNAME / FS_ETCD_PASSWORD)")
	}

	return nil
}
