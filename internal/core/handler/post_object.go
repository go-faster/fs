package handler

import (
	"context"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // The POST policy signature is defined over HMAC-SHA1.
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/s3err"
)

// POST object is the browser upload: an HTML form posts multipart/form-data
// straight at the bucket, carrying a base64 policy document that says what the
// upload is allowed to be and a signature over it. The browser never holds a
// credential — the policy was signed elsewhere, by someone who has one — which
// is the whole point of the mechanism and why the policy, not the request, is
// what gets verified.

// maxPostFieldBytes caps how much form data is buffered before the file part.
// The fields are small by construction (a policy, a signature, a handful of
// names); a client sending more is not doing a browser upload.
const maxPostFieldBytes = 1 << 20

// postForm is a parsed POST-object form: the fields that preceded the file,
// and the file part itself, still unread.
type postForm struct {
	fields   map[string]string
	filename string
	file     *multipart.Part
}

// field returns a form field case-insensitively, the way S3 treats them.
func (f *postForm) field(name string) string {
	if v, ok := f.fields[strings.ToLower(name)]; ok {
		return v
	}

	return ""
}

// PostObject serves POST /{bucket} with a multipart/form-data body.
func (h *handler) PostObject(w http.ResponseWriter, r *http.Request, bucket string) {
	ctx := r.Context()

	form, err := parsePostForm(r)
	if err != nil {
		renderAPIError(ctx, w, r, s3err.MalformedPOSTRequest, err)
		return
	}

	defer func() { _ = form.file.Close() }()

	key := strings.ReplaceAll(form.field("key"), "${filename}", form.filename)
	if key == "" {
		renderAPIError(ctx, w, r, s3err.InvalidArgument, errors.New("key is required"))
		return
	}

	policy, err := h.authorizePost(r, form, bucket, key)
	if err != nil {
		if errors.Is(err, errMalformedPolicy) {
			renderAPIError(ctx, w, r, s3err.MalformedPOSTRequest, err)
			return
		}

		renderError(ctx, w, r, err)

		return
	}

	// content-length-range is enforced while reading rather than from a
	// declared length: the form does not carry one for the file part, and a
	// declared length is the client's claim, not a fact.
	body := io.Reader(form.file)
	if policy != nil && (policy.maxLength >= 0 || policy.minLength > 0) {
		body = &limitedPostBody{
			r:         form.file,
			remaining: policy.maxLength + 1,
			unbounded: policy.maxLength < 0,
			min:       policy.minLength,
		}
	}

	metadata := extractObjectMetadata(http.Header{})
	metadata.ContentType = form.field("Content-Type")
	metadata.CacheControl = form.field("Cache-Control")
	metadata.ContentDisposition = form.field("Content-Disposition")
	metadata.ContentEncoding = form.field("Content-Encoding")
	metadata.Expires = form.field("Expires")

	for name, value := range form.fields {
		if after, ok := strings.CutPrefix(name, "x-amz-meta-"); ok {
			if metadata.UserMetadata == nil {
				metadata.UserMetadata = make(map[string]string)
			}

			metadata.UserMetadata[after] = value
		}
	}

	// The form's tagging field carries the Tagging XML document, not the
	// query-style value the x-amz-tagging header uses.
	tags, err := parsePostTagging(form.field("tagging"))
	if err != nil {
		renderAPIError(ctx, w, r, s3err.InvalidArgument, err)
		return
	}

	resp, err := h.service.PutObject(ctx, &fs.PutObjectRequest{
		Reader:   body,
		Tags:     tags,
		Bucket:   bucket,
		Key:      key,
		Size:     -1,
		Metadata: metadata,
		ACL:      fs.ParseACL(form.field("acl")),
		Owner:    callerOwner(ctx),
	})
	if err != nil {
		switch {
		case errors.Is(err, errPostBodyTooLarge):
			renderAPIError(ctx, w, r, s3err.EntityTooLarge, err)
		case errors.Is(err, errPostBodyTooSmall):
			renderAPIError(ctx, w, r, s3err.EntityTooSmall, err)
		default:
			renderError(ctx, w, r, err)
		}

		return
	}

	writePostSuccess(ctx, w, r, form, bucket, key, resp.ETag, policy != nil)
}

// writePostSuccess answers a completed upload the way the form asked to be
// answered: a redirect, a chosen status code, or the default 204.
func writePostSuccess(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
	form *postForm, bucket, key, etag string, policed bool,
) {
	// A redirect is honored only for a form that carried a policy. The policy
	// has to name every field it allows, so the signer approved this exact
	// destination; without one, anyone could point the endpoint anywhere and
	// use it as an open redirect.
	if redirect := form.field("success_action_redirect"); redirect != "" && policed {
		if u, err := url.Parse(redirect); err == nil {
			// Built by hand rather than through url.Values, which sorts: S3
			// emits bucket, key, etag in that order and clients compare the
			// whole URL.
			u.RawQuery = "bucket=" + url.QueryEscape(bucket) +
				"&key=" + url.QueryEscape(key) +
				"&etag=" + url.QueryEscape(`"`+etag+`"`)

			w.Header().Set("ETag", quoteETag(etag))
			// The destination came from a policy-covered field, so a signer
			// with a credential named it; see the comment above.
			http.Redirect(w, r, u.String(), http.StatusSeeOther) //nolint:gosec // G710: policy-approved destination.

			return
		}
	}

	w.Header().Set("ETag", quoteETag(etag))

	// Only 200 and 201 are honored; anything else falls back to 204, which is
	// what S3 does rather than letting a form choose an arbitrary status.
	switch form.field("success_action_status") {
	case "201":
		writeXMLStatus(ctx, w, r, http.StatusCreated, PostResponseXML{
			Location: "/" + bucket + "/" + key,
			Bucket:   bucket,
			Key:      key,
			ETag:     quoteETag(etag),
		})
	case "200":
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// PostResponseXML is the body of a 201 response.
type PostResponseXML struct {
	XMLName  xml.Name `xml:"PostResponse"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

// parsePostForm reads the form fields up to the file part.
//
// S3 requires the file to be last, and this relies on it: everything before it
// is buffered as a field, and the file itself is left unread so it can stream
// into storage.
func parsePostForm(r *http.Request) (*postForm, error) {
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		return nil, errors.New("expected a multipart/form-data body")
	}

	reader := multipart.NewReader(r.Body, params["boundary"])
	form := &postForm{fields: make(map[string]string)}

	var buffered int

	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("form has no file part")
		}

		if err != nil {
			return nil, errors.Wrap(err, "read form part")
		}

		if strings.EqualFold(part.FormName(), "file") {
			form.file = part
			form.filename = part.FileName()

			return form, nil
		}

		value, err := io.ReadAll(io.LimitReader(part, int64(maxPostFieldBytes-buffered)))
		_ = part.Close()

		if err != nil {
			return nil, errors.Wrap(err, "read form field")
		}

		buffered += len(value)
		if buffered >= maxPostFieldBytes {
			return nil, errors.New("form fields are too large")
		}

		form.fields[strings.ToLower(part.FormName())] = string(value)
	}
}

// postPolicy is a decoded policy document: when the upload expires, what the
// fields must look like, and how big the body may be.
type postPolicy struct {
	expiration time.Time
	conditions []postCondition
	minLength  int64
	maxLength  int64
}

// postCondition is one condition of a policy.
type postCondition struct {
	operator string // "eq" or "starts-with"
	field    string // lowercased field name, without the "$"
	value    string
}

// authorizePost verifies that the upload is allowed: either the policy is
// signed by a credential the server knows and the form satisfies it, or the
// bucket permits anonymous writes and no policy was supplied.
func (h *handler) authorizePost(
	r *http.Request, form *postForm, bucket, key string,
) (*postPolicy, error) {
	raw := form.field("policy")
	if raw == "" {
		// No policy: this is the anonymous form upload, which only a bucket
		// that allows anonymous writes accepts. A server running without
		// credentials has no such notion — every request is anonymous — so it
		// accepts the upload the way it accepts any other write.
		if h.postSecret != nil && !anonymousWriteAllowed(r.Context()) {
			return nil, errors.Wrap(fs.ErrAccessDenied, "bucket does not allow anonymous uploads")
		}

		return nil, nil
	}

	policy, err := parsePostPolicy(raw)
	if err != nil {
		return nil, err
	}

	if err := h.verifyPostSignature(form, raw); err != nil {
		return nil, err
	}

	if !policy.expiration.IsZero() && time.Now().After(policy.expiration) {
		return nil, errors.Wrap(fs.ErrAccessDenied, "policy has expired")
	}

	if err := policy.check(form, bucket, key); err != nil {
		return nil, err
	}

	return policy, nil
}

// verifyPostSignature checks the signature over the base64 policy.
//
// Both forms are accepted: the modern one (x-amz-algorithm / x-amz-credential /
// x-amz-signature, HMAC-SHA256 through the SigV4 signing key) and the original
// browser-upload one (AWSAccessKeyId + HMAC-SHA1 over the policy). The second
// is not SigV2 request signing — it signs only the policy document, which
// itself constrains what the upload may be — and it is what browser upload
// forms in the wild still generate.
func (h *handler) verifyPostSignature(form *postForm, policy string) error {
	if h.postSecret == nil {
		// The server is running without credentials, so there is nothing to
		// verify against; the anonymous path already gated the request.
		return nil
	}

	if signature := form.field("x-amz-signature"); signature != "" {
		credential := form.field("x-amz-credential")

		accessKey, _, _ := strings.Cut(credential, "/")

		secret, ok := h.postSecret(accessKey)
		if !ok {
			return errors.Wrap(fs.ErrAccessDenied, "unknown access key")
		}

		want := sigV4PostSignature(secret, credential, form.field("x-amz-date"), policy)
		if !hmac.Equal([]byte(signature), []byte(want)) {
			return errors.Wrap(fs.ErrAccessDenied, "signature mismatch")
		}

		return nil
	}

	accessKey := form.field("AWSAccessKeyId")
	if accessKey == "" || form.field("signature") == "" {
		return errors.Wrap(errMalformedPolicy, "policy carries no signature")
	}

	secret, ok := h.postSecret(accessKey)
	if !ok {
		return errors.Wrap(fs.ErrAccessDenied, "unknown access key")
	}

	mac := hmac.New(sha1.New, []byte(secret)) //nolint:gosec // Defined over HMAC-SHA1.
	_, _ = mac.Write([]byte(policy))

	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(form.field("signature")), []byte(want)) {
		return errors.Wrap(fs.ErrAccessDenied, "signature mismatch")
	}

	return nil
}

// sigV4PostSignature computes the SigV4 policy signature for a credential
// scope of the form <key>/<date>/<region>/<service>/aws4_request.
func sigV4PostSignature(secret, credential, amzDate, policy string) string {
	parts := strings.Split(credential, "/")
	if len(parts) < 5 { //nolint:mnd // A credential scope has five components.
		return ""
	}

	date, region, service := parts[1], parts[2], parts[3]
	if amzDate != "" && len(amzDate) >= len(date) {
		date = amzDate[:len(date)]
	}

	key := []byte("AWS4" + secret)
	for _, part := range []string{date, region, service, "aws4_request"} {
		mac := hmac.New(sha256.New, key)
		_, _ = mac.Write([]byte(part))
		key = mac.Sum(nil)
	}

	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(policy))

	return hex.EncodeToString(mac.Sum(nil))
}

// parsePostPolicy decodes the base64 JSON policy document.
func parsePostPolicy(raw string) (*postPolicy, error) {
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, errors.Wrap(fs.ErrAccessDenied, "policy is not base64")
	}

	// Decoded field by field rather than into a struct: encoding/json matches
	// member names case-insensitively, which would quietly accept a policy
	// spelling "CONDITIONS". The names in a policy document are case-sensitive,
	// and a policy whose conditions the server did not recognize is one whose
	// conditions it would not enforce.
	var members map[string]json.RawMessage
	if err := json.Unmarshal(decoded, &members); err != nil {
		return nil, errors.Wrap(errMalformedPolicy, "policy is not valid JSON")
	}

	var doc struct {
		Expiration string
		Conditions []json.RawMessage
	}

	if v, ok := members["expiration"]; ok {
		if err := json.Unmarshal(v, &doc.Expiration); err != nil {
			return nil, errors.Wrap(errMalformedPolicy, "policy expiration is not a string")
		}
	}

	if v, ok := members["conditions"]; ok {
		if err := json.Unmarshal(v, &doc.Conditions); err != nil {
			return nil, errors.Wrap(errMalformedPolicy, "policy conditions are not a list")
		}

		if doc.Conditions == nil {
			doc.Conditions = []json.RawMessage{}
		}
	}

	// A policy without an expiration or without conditions is not a policy
	// that denies the upload — it is not a policy at all, and the difference
	// matters: the caller has a bug to fix, not a permission to request.
	if doc.Expiration == "" {
		return nil, errors.Wrap(errMalformedPolicy, "policy has no expiration")
	}

	if doc.Conditions == nil {
		return nil, errors.Wrap(errMalformedPolicy, "policy has no conditions")
	}

	policy := &postPolicy{minLength: 0, maxLength: -1}

	expiration, err := time.Parse(time.RFC3339, doc.Expiration)
	if err != nil {
		return nil, errors.Wrap(errMalformedPolicy, "policy expiration is not a timestamp")
	}

	policy.expiration = expiration

	for _, raw := range doc.Conditions {
		if err := policy.addCondition(raw); err != nil {
			return nil, err
		}
	}

	return policy, nil
}

// addCondition parses one policy condition, in either of the two shapes a
// policy document uses: an object ({"acl": "private"}) or an array
// (["starts-with", "$key", "foo"]).
func (p *postPolicy) addCondition(raw json.RawMessage) error {
	var object map[string]string
	if err := json.Unmarshal(raw, &object); err == nil {
		// An empty object says nothing, which is not the same as saying
		// "anything goes": a policy that carries one is malformed.
		if len(object) == 0 {
			return errors.Wrap(errMalformedPolicy, "empty policy condition")
		}

		for name, value := range object {
			p.conditions = append(p.conditions, postCondition{
				operator: "eq",
				field:    strings.ToLower(name),
				value:    value,
			})
		}

		return nil
	}

	var array []any
	if err := json.Unmarshal(raw, &array); err != nil || len(array) != 3 { //nolint:mnd // Every array condition has three elements.
		return errors.Wrap(errMalformedPolicy, "malformed policy condition")
	}

	operator, _ := array[0].(string)

	if strings.EqualFold(operator, "content-length-range") {
		p.minLength = int64(numberOf(array[1]))
		p.maxLength = int64(numberOf(array[2]))

		return nil
	}

	field, _ := array[1].(string)
	value, _ := array[2].(string)

	p.conditions = append(p.conditions, postCondition{
		operator: strings.ToLower(operator),
		field:    strings.ToLower(strings.TrimPrefix(field, "$")),
		value:    value,
	})

	return nil
}

// numberOf reads a JSON number that may have decoded as a float.
func numberOf(v any) float64 {
	f, _ := v.(float64)
	return f
}

// check reports whether the form satisfies every condition the policy states.
//
// A field the form supplies but the policy does not mention is a failure, not
// an omission: the policy is an exhaustive statement of what the upload may
// contain, and anything unmentioned was never authorized.
func (p *postPolicy) check(form *postForm, bucket, key string) error {
	stated := make(map[string]string, len(form.fields)+2)
	for name, value := range form.fields {
		stated[name] = value
	}

	// The bucket comes from the URL, and the key is the substituted one —
	// ${filename} has already been resolved, and that is the name the object
	// will actually have, so that is what the policy must be judged against.
	stated["bucket"] = bucket
	stated["key"] = key

	mentioned := make(map[string]bool, len(p.conditions))

	for _, cond := range p.conditions {
		mentioned[cond.field] = true

		value := stated[cond.field]

		switch cond.operator {
		case "eq":
			if value != cond.value {
				return errors.Wrapf(fs.ErrAccessDenied, "condition on %q not met", cond.field)
			}
		case "starts-with":
			if !strings.HasPrefix(value, cond.value) {
				return errors.Wrapf(fs.ErrAccessDenied, "condition on %q not met", cond.field)
			}
		default:
			return errors.Wrapf(fs.ErrAccessDenied, "unknown condition %q", cond.operator)
		}
	}

	for name := range form.fields {
		if ignoredPostField(name) || mentioned[name] {
			continue
		}

		return errors.Wrapf(fs.ErrAccessDenied, "field %q is not covered by the policy", name)
	}

	return nil
}

// ignoredPostFields are the form fields that carry the request's own machinery
// rather than anything about the object, and so need no policy condition.
var ignoredPostFields = map[string]bool{
	"policy":           true,
	"signature":        true,
	"awsaccesskeyid":   true,
	"x-amz-signature":  true,
	"x-amz-credential": true,
	"x-amz-algorithm":  true,
	"x-amz-date":       true,
	"file":             true,
}

// ignoredPostFieldPrefix marks the fields S3 reserves for the caller's own
// use: an "x-ignore-" field is carried through the form and, as the name says,
// ignored — so it needs no policy condition.
const ignoredPostFieldPrefix = "x-ignore-"

func ignoredPostField(name string) bool {
	return ignoredPostFields[name] || strings.HasPrefix(name, ignoredPostFieldPrefix)
}

// errMalformedPolicy marks a policy document the server could not make sense
// of, as opposed to one that parsed and refused the upload.
var errMalformedPolicy = errors.New("malformed policy document")

// The two ways a body can violate the policy's content-length-range.
var (
	errPostBodyTooLarge = errors.New("upload exceeds the policy's content-length-range")
	errPostBodyTooSmall = errors.New("upload is below the policy's content-length-range")
)

// limitedPostBody enforces the policy's size bounds as the body streams: the
// ceiling by failing the read that would cross it, the floor by failing at EOF.
//
// Both fail the *read*, so the backend abandons the write the way it would for
// a truncated body and nothing is stored. Checking afterwards would mean
// storing the object first and then deleting it — which destroys whatever was
// already at that key, an upload that broke the rules taking a good object
// with it.
type limitedPostBody struct {
	r         io.Reader
	remaining int64
	unbounded bool
	min       int64
	read      int64
}

func (l *limitedPostBody) Read(p []byte) (int, error) {
	if !l.unbounded && l.remaining <= 0 {
		return 0, errPostBodyTooLarge
	}

	if !l.unbounded && int64(len(p)) > l.remaining {
		p = p[:l.remaining]
	}

	n, err := l.r.Read(p)
	l.remaining -= int64(n)
	l.read += int64(n)

	if !l.unbounded && l.remaining <= 0 && err == nil {
		return n, errPostBodyTooLarge
	}

	if errors.Is(err, io.EOF) && l.read < l.min {
		return n, errPostBodyTooSmall
	}

	return n, err
}

// parsePostTagging decodes the Tagging document a form may carry.
func parsePostTagging(document string) ([]fs.Tag, error) {
	if document == "" {
		return nil, nil
	}

	var doc Tagging

	// The document is a form field, so it is bounded by maxPostFieldBytes, and
	// encoding/xml resolves no external entities; there is nothing here for a
	// hostile document to expand into.
	if err := xml.Unmarshal([]byte(document), &doc); err != nil { //nolint:gosec // G709: bounded input, no entity resolution.
		return nil, errors.Wrap(err, "parse tagging document")
	}

	tags := make([]fs.Tag, 0, len(doc.TagSet.Tags))
	for _, tag := range doc.TagSet.Tags {
		tags = append(tags, fs.Tag(tag))
	}

	return tags, nil
}
