// Package replication ships a bucket's versions to buckets of other fs
// deployments: geo-replication (docs/design/geo-replication.md).
package replication

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
)

// param is the bucket subresource the target serves replication on.
const param = "x-fs-replica"

// Client speaks the replication protocol to one target bucket, signing each
// request with SigV4 as an S3 client would.
type Client struct {
	endpoint *url.URL
	bucket   string
	creds    aws.Credentials
	signer   *v4.Signer
	http     *http.Client
}

// NewClient returns a client of bucket at endpoint, an fs deployment's S3
// endpoint. hc nil means http.DefaultClient.
func NewClient(endpoint, bucket, accessKey, secretKey string, hc *http.Client) (*Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, errors.Wrap(err, "parse endpoint")
	}

	if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return nil, errors.Errorf("endpoint %q is not an http(s) URL", endpoint)
	}

	if hc == nil {
		hc = http.DefaultClient
	}

	return &Client{
		endpoint: u,
		bucket:   bucket,
		creds:    aws.Credentials{AccessKeyID: accessKey, SecretAccessKey: secretKey},
		signer:   v4.NewSigner(),
		http:     hc,
	}, nil
}

// Error is a target's refusal of a request, as its S3 error body says.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s (%d): %s", e.Code, e.Status, e.Message)
}

// Info asks the target about its bucket.
func (c *Client) Info(ctx context.Context) (fs.ReplicaInfo, error) {
	var info fs.ReplicaInfo

	err := c.do(ctx, http.MethodGet, "", nil, nil, &info)

	return info, err
}

// Claim has the target reference a version's blocks, and answers which it
// needs sent.
func (c *Client) Claim(ctx context.Context, versionID string, blocks []fs.ReplicaBlock) ([]fs.ReplicaBlockClaim, error) {
	body, err := json.Marshal(struct {
		VersionID string            `json:"version_id"`
		Blocks    []fs.ReplicaBlock `json:"blocks"`
	}{versionID, blocks})
	if err != nil {
		return nil, err
	}

	var claims []fs.ReplicaBlockClaim
	if err := c.do(ctx, http.MethodPost, "blocks", nil, body, &claims); err != nil {
		return nil, err
	}

	if len(claims) != len(blocks) {
		return nil, errors.Errorf("%d claims for %d blocks", len(claims), len(blocks))
	}

	return claims, nil
}

// PutBlock sends a block of a version, to be stored as its claim said.
func (c *Client) PutBlock(ctx context.Context, versionID string, b fs.ReplicaBlock, data []byte) error {
	q := url.Values{"version": {versionID}, "hash": {b.Hash}}
	if b.Scheme != "" {
		q.Set("scheme", b.Scheme)
	}

	return c.do(ctx, http.MethodPut, "block", q, data, nil)
}

// Apply sends versions, whose blocks the target holds, and returns its
// answer for each.
func (c *Client) Apply(ctx context.Context, versions []fs.ReplicaVersion) ([]fs.ReplicaResult, error) {
	body, err := json.Marshal(versions)
	if err != nil {
		return nil, err
	}

	var results []fs.ReplicaResult
	if err := c.do(ctx, http.MethodPost, "versions", nil, body, &results); err != nil {
		return nil, err
	}

	if len(results) != len(versions) {
		return nil, errors.Errorf("%d results for %d versions", len(results), len(versions))
	}

	return results, nil
}

// do sends the request op to the bucket, signed, and decodes a JSON answer
// into out, when not nil.
func (c *Client) do(ctx context.Context, method, op string, q url.Values, body []byte, out any) error {
	u := *c.endpoint
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + c.bucket

	// The subresource, bare when it has no value, as S3 clients write one.
	u.RawQuery = param
	if op != "" {
		u.RawQuery += "=" + op
	}

	if len(q) > 0 {
		u.RawQuery += "&" + q.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return errors.Wrap(err, "build request")
	}

	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	req.Header.Set("X-Amz-Content-Sha256", hash)

	if out == nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	} else {
		req.Header.Set("Content-Type", "application/json")
	}

	if err := c.signer.SignHTTP(ctx, c.creds, req, hash, "s3", "us-east-1", time.Now()); err != nil {
		return errors.Wrap(err, "sign request")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return errors.Wrapf(err, "%s %s", method, cmp.Or(op, "info"))
	}

	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return errors.Wrap(err, "read answer")
	}

	if resp.StatusCode/100 != 2 {
		e := &Error{Status: resp.StatusCode}

		var x struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		}

		if xml.Unmarshal(data, &x) == nil {
			e.Code, e.Message = x.Code, x.Message
		}

		return e
	}

	if out == nil {
		return nil
	}

	if err := json.Unmarshal(data, out); err != nil {
		return errors.Wrap(err, "decode answer")
	}

	return nil
}
