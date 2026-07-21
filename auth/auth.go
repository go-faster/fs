// Package auth provides credential storage and a small grant-based
// authorization model for the S3 server. It is deliberately a table, not a
// policy engine: each access key maps to a secret and a set of
// (bucket-pattern → permission) grants, and buckets may be flagged public-read.
//
// The zero value is not useful; construct a Store with NewStore and swap its
// snapshot atomically via Set for hot reload.
package auth

import (
	"path"
	"strings"
	"sync/atomic"

	"github.com/go-faster/errors"
)

// Permission is an access level. Higher levels include lower ones:
// Admin ⊇ Write ⊇ Read.
type Permission int

const (
	// Read allows GET/HEAD/list operations.
	Read Permission = iota
	// Write allows Read plus PUT/POST/DELETE (including bucket create/delete).
	Write
	// Admin allows everything Write does, and is the level future admin-only
	// operations will require.
	Admin
)

// Action is the access level a request requires, derived from its method.
type Action int

const (
	// ActionRead is required by GET/HEAD.
	ActionRead Action = iota
	// ActionWrite is required by PUT/POST/DELETE.
	ActionWrite
)

// Grant authorizes an access key for buckets matching Pattern (a glob using
// path.Match semantics; "*" matches all buckets) up to Permission.
type Grant struct {
	Pattern    string
	Permission Permission
}

// Key is a credential: an access/secret pair and its grants.
type Key struct {
	AccessKey string
	SecretKey string
	Grants    []Grant
}

// Config is the declarative auth configuration, suitable for loading from a
// file or building programmatically.
type Config struct {
	// Keys are the credentials the server accepts.
	Keys []Key
	// PublicReadBuckets may be read anonymously (unsigned GET/HEAD/list).
	PublicReadBuckets []string
}

// Validate checks the configuration for empty/duplicate access keys and empty
// secrets.
func (c Config) Validate() error {
	seen := make(map[string]struct{}, len(c.Keys))

	for _, k := range c.Keys {
		if k.AccessKey == "" {
			return errors.New("auth: empty access key")
		}

		if k.SecretKey == "" {
			return errors.Errorf("auth: key %q has empty secret", k.AccessKey)
		}

		if _, ok := seen[k.AccessKey]; ok {
			return errors.Errorf("auth: duplicate access key %q", k.AccessKey)
		}

		seen[k.AccessKey] = struct{}{}
	}

	return nil
}

// snapshot is an immutable resolved view of a Config.
type snapshot struct {
	keys       map[string]Key
	publicRead map[string]struct{}
}

func newSnapshot(c Config) *snapshot {
	s := &snapshot{
		keys:       make(map[string]Key, len(c.Keys)),
		publicRead: make(map[string]struct{}, len(c.PublicReadBuckets)),
	}

	for _, k := range c.Keys {
		s.keys[k.AccessKey] = k
	}

	for _, b := range c.PublicReadBuckets {
		s.publicRead[b] = struct{}{}
	}

	return s
}

// Store holds the active auth configuration behind an atomic pointer, so Set
// hot-reloads it without locking readers.
type Store struct {
	snap atomic.Pointer[snapshot]
}

// NewStore builds a Store from cfg after validating it.
func NewStore(cfg Config) (*Store, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	s := &Store{}
	s.snap.Store(newSnapshot(cfg))

	return s, nil
}

// Set atomically replaces the active configuration (hot reload).
func (s *Store) Set(cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}

	s.snap.Store(newSnapshot(cfg))

	return nil
}

// Secret returns the secret key for an access key, matching sigv4.SecretKeyFunc.
func (s *Store) Secret(accessKey string) (string, bool) {
	k, ok := s.snap.Load().keys[accessKey]
	if !ok {
		return "", false
	}

	return k.SecretKey, true
}

// Allow reports whether the access key may perform action on bucket. A bucket
// of "" (e.g. ListBuckets) is allowed for any known key. Unknown keys are
// denied.
func (s *Store) Allow(accessKey, bucket string, action Action) bool {
	k, ok := s.snap.Load().keys[accessKey]
	if !ok {
		return false
	}

	if bucket == "" {
		// Service-level operations (ListBuckets) need only a valid identity.
		return true
	}

	need := Read
	if action == ActionWrite {
		need = Write
	}

	for _, g := range k.Grants {
		if g.Permission >= need && patternMatches(g.Pattern, bucket) {
			return true
		}
	}

	return false
}

// PublicRead reports whether bucket permits anonymous reads.
func (s *Store) PublicRead(bucket string) bool {
	_, ok := s.snap.Load().publicRead[bucket]

	return ok
}

// patternMatches reports whether a bucket matches a grant pattern. "*" matches
// everything; otherwise path.Match glob semantics apply, with a literal
// fallback so an invalid pattern still matches its exact self.
func patternMatches(pattern, bucket string) bool {
	if pattern == "*" || pattern == bucket {
		return true
	}

	if !strings.ContainsAny(pattern, "*?[") {
		return false
	}

	ok, err := path.Match(pattern, bucket)

	return err == nil && ok
}
