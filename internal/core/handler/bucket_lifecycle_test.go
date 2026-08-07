package handler_test

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/core/handler"
)

// newLifecycleHandler returns a handler over a memory backend with one bucket.
func newLifecycleHandler(t testing.TB) http.Handler {
	t.Helper()

	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket", "", nil).Code)

	return h
}

func lifecycleRequest(t testing.TB, h http.Handler, method, body string) *httptest.ResponseRecorder {
	t.Helper()

	return do(t, h, method, "/bucket?lifecycle", body, nil)
}

func TestBucketLifecycle_RoundTrip(t *testing.T) {
	h := newLifecycleHandler(t)

	rec := lifecycleRequest(t, h, http.MethodGet, "")
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, "NoSuchLifecycleConfiguration", errorCode(t, rec.Body.String()))

	put := lifecycleRequest(t, h, http.MethodPut, `<LifecycleConfiguration>
		<Rule>
			<ID>expire-logs</ID>
			<Status>Enabled</Status>
			<Filter><Prefix>logs/</Prefix></Filter>
			<Expiration><Days>30</Days></Expiration>
		</Rule>
		<Rule>
			<ID>abandoned</ID>
			<Status>Enabled</Status>
			<Prefix></Prefix>
			<AbortIncompleteMultipartUpload><DaysAfterInitiation>7</DaysAfterInitiation></AbortIncompleteMultipartUpload>
		</Rule>
	</LifecycleConfiguration>`)
	require.Equal(t, http.StatusOK, put.Code, put.Body.String())

	get := lifecycleRequest(t, h, http.MethodGet, "")
	require.Equal(t, http.StatusOK, get.Code)

	var doc handler.LifecycleConfigurationXML
	require.NoError(t, xml.Unmarshal(get.Body.Bytes(), &doc))
	require.Len(t, doc.Rules, 2)

	require.Equal(t, "expire-logs", doc.Rules[0].ID)
	require.Equal(t, fs.LifecycleEnabled, doc.Rules[0].Status)
	require.NotNil(t, doc.Rules[0].Filter)
	require.NotNil(t, doc.Rules[0].Filter.Prefix)
	require.Equal(t, "logs/", *doc.Rules[0].Filter.Prefix)
	require.NotNil(t, doc.Rules[0].Expiration)
	require.Equal(t, 30, doc.Rules[0].Expiration.Days)

	require.NotNil(t, doc.Rules[1].AbortIncompleteMultipartUpload)
	require.Equal(t, 7, doc.Rules[1].AbortIncompleteMultipartUpload.DaysAfterInitiation)

	del := lifecycleRequest(t, h, http.MethodDelete, "")
	require.Equal(t, http.StatusNoContent, del.Code)

	gone := lifecycleRequest(t, h, http.MethodGet, "")
	require.Equal(t, http.StatusNotFound, gone.Code)
}

// TestBucketLifecycle_LegacyPrefix covers the pre-Filter spelling SDKs still
// emit: it has to reach the rule, not be dropped into a rule that matches the
// whole bucket and expires everything.
func TestBucketLifecycle_LegacyPrefix(t *testing.T) {
	h := newLifecycleHandler(t)

	put := lifecycleRequest(t, h, http.MethodPut, `<LifecycleConfiguration>
		<Rule>
			<ID>legacy</ID>
			<Status>Enabled</Status>
			<Prefix>tmp/</Prefix>
			<Expiration><Days>1</Days></Expiration>
		</Rule>
	</LifecycleConfiguration>`)
	require.Equal(t, http.StatusOK, put.Code, put.Body.String())

	get := lifecycleRequest(t, h, http.MethodGet, "")

	var doc handler.LifecycleConfigurationXML
	require.NoError(t, xml.Unmarshal(get.Body.Bytes(), &doc))
	require.Len(t, doc.Rules, 1)
	require.NotNil(t, doc.Rules[0].Filter.Prefix)
	require.Equal(t, "tmp/", *doc.Rules[0].Filter.Prefix)
}

// TestBucketLifecycle_RefusesUnenforced is the honesty guard: an element the
// sweep does not act on must be refused by name, never stored and ignored. A
// stored-but-inert rule tells a client its data expires when nothing will ever
// delete it.
func TestBucketLifecycle_RefusesUnenforced(t *testing.T) {
	for name, body := range map[string]string{
		"Transition": `<Rule><ID>t</ID><Status>Enabled</Status><Prefix></Prefix>
			<Transition><Days>1</Days><StorageClass>GLACIER</StorageClass></Transition></Rule>`,
		"NoncurrentVersionExpiration": `<Rule><ID>n</ID><Status>Enabled</Status><Prefix></Prefix>
			<NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays></NoncurrentVersionExpiration></Rule>`,
		"ExpiredObjectDeleteMarker": `<Rule><ID>e</ID><Status>Enabled</Status><Prefix></Prefix>
			<Expiration><ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker></Expiration></Rule>`,
		"FilterTag": `<Rule><ID>f</ID><Status>Enabled</Status>
			<Filter><Tag><Key>k</Key><Value>v</Value></Tag></Filter>
			<Expiration><Days>1</Days></Expiration></Rule>`,
		"FilterSize": `<Rule><ID>s</ID><Status>Enabled</Status>
			<Filter><ObjectSizeGreaterThan>1</ObjectSizeGreaterThan></Filter>
			<Expiration><Days>1</Days></Expiration></Rule>`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newLifecycleHandler(t)

			rec := lifecycleRequest(t, h, http.MethodPut,
				"<LifecycleConfiguration>"+body+"</LifecycleConfiguration>")
			require.Equal(t, http.StatusNotImplemented, rec.Code, rec.Body.String())
			require.Equal(t, "NotImplemented", errorCode(t, rec.Body.String()))

			// The whole document is refused, so nothing is left behind.
			get := lifecycleRequest(t, h, http.MethodGet, "")
			require.Equal(t, http.StatusNotFound, get.Code)
		})
	}
}

func TestBucketLifecycle_Validation(t *testing.T) {
	longID := strings.Repeat("x", 256)

	for name, tc := range map[string]struct {
		body string
		code string
	}{
		"InvalidStatus": {
			body: `<Rule><Status>Sometimes</Status><Prefix></Prefix><Expiration><Days>1</Days></Expiration></Rule>`,
			code: "MalformedXML",
		},
		"NoRules": {body: "", code: "MalformedXML"},
		"NoAction": {
			body: `<Rule><ID>x</ID><Status>Enabled</Status><Prefix>a/</Prefix></Rule>`,
			code: "MalformedXML",
		},
		"DaysAndDate": {
			body: `<Rule><ID>x</ID><Status>Enabled</Status><Prefix></Prefix>
				<Expiration><Days>1</Days><Date>2030-01-01T00:00:00Z</Date></Expiration></Rule>`,
			code: "MalformedXML",
		},
		"InvalidDate": {
			body: `<Rule><ID>x</ID><Status>Enabled</Status><Prefix></Prefix>
				<Expiration><Date>20300101</Date></Expiration></Rule>`,
			code: "MalformedXML",
		},
		"IDTooLong": {
			body: `<Rule><ID>` + longID + `</ID><Status>Enabled</Status><Prefix></Prefix>
				<Expiration><Days>1</Days></Expiration></Rule>`,
			code: "InvalidArgument",
		},
		"DuplicateID": {
			body: `<Rule><ID>same</ID><Status>Enabled</Status><Prefix>a/</Prefix>
					<Expiration><Days>1</Days></Expiration></Rule>
				<Rule><ID>same</ID><Status>Enabled</Status><Prefix>b/</Prefix>
					<Expiration><Days>2</Days></Expiration></Rule>`,
			code: "InvalidArgument",
		},
		"ZeroAbortDays": {
			body: `<Rule><ID>x</ID><Status>Enabled</Status><Prefix></Prefix>
				<AbortIncompleteMultipartUpload><DaysAfterInitiation>0</DaysAfterInitiation></AbortIncompleteMultipartUpload></Rule>`,
			code: "InvalidArgument",
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newLifecycleHandler(t)

			rec := lifecycleRequest(t, h, http.MethodPut,
				"<LifecycleConfiguration>"+tc.body+"</LifecycleConfiguration>")
			require.Equal(t, tc.code, errorCode(t, rec.Body.String()), rec.Body.String())
		})
	}
}

// TestBucketLifecycle_DateRoundTrip checks the Date form survives storage and
// comes back in the form S3 renders it.
func TestBucketLifecycle_DateRoundTrip(t *testing.T) {
	h := newLifecycleHandler(t)

	put := lifecycleRequest(t, h, http.MethodPut, `<LifecycleConfiguration>
		<Rule>
			<ID>by-date</ID>
			<Status>Enabled</Status>
			<Prefix>archive/</Prefix>
			<Expiration><Date>2030-01-01T00:00:00Z</Date></Expiration>
		</Rule>
	</LifecycleConfiguration>`)
	require.Equal(t, http.StatusOK, put.Code, put.Body.String())

	get := lifecycleRequest(t, h, http.MethodGet, "")

	var doc handler.LifecycleConfigurationXML
	require.NoError(t, xml.Unmarshal(get.Body.Bytes(), &doc))
	require.Len(t, doc.Rules, 1)
	require.Equal(t, "2030-01-01T00:00:00Z", doc.Rules[0].Expiration.Date)
	require.Zero(t, doc.Rules[0].Expiration.Days)
}
