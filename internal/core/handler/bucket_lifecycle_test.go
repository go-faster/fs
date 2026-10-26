package handler_test

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/engine"
	"github.com/go-faster/fs/internal/core/handler"
	"github.com/go-faster/fs/internal/core/service"
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
//
// The memory backend cannot list or remove versions, so the version rules are
// refused there too; the engine accepts them (TestBucketLifecycle_Versions).
func TestBucketLifecycle_RefusesUnenforced(t *testing.T) {
	for name, body := range map[string]string{
		"Transition": `<Rule><ID>t</ID><Status>Enabled</Status><Prefix></Prefix>
			<Transition><Days>1</Days><StorageClass>GLACIER</StorageClass></Transition></Rule>`,
		"NoncurrentVersionTransition": `<Rule><ID>t</ID><Status>Enabled</Status><Prefix></Prefix>
			<NoncurrentVersionTransition><NoncurrentDays>1</NoncurrentDays><StorageClass>GLACIER</StorageClass></NoncurrentVersionTransition></Rule>`,
		"NoncurrentVersionExpirationWithoutVersions": `<Rule><ID>n</ID><Status>Enabled</Status><Prefix></Prefix>
			<NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays></NoncurrentVersionExpiration></Rule>`,
		"ExpiredObjectDeleteMarkerWithoutVersions": `<Rule><ID>e</ID><Status>Enabled</Status><Prefix></Prefix>
			<Expiration><ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker></Expiration></Rule>`,
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

// lifecycleEngineHandler is newLifecycleHandler over the engine, which can list
// and remove versions and so enforce the rules on them.
func lifecycleEngineHandler(t testing.TB) http.Handler {
	t.Helper()

	storage, err := engine.Open(t.TempDir(), engine.Options{NoSync: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = storage.Close() })

	h := handler.New(service.New(storage))
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket", "", nil).Code)

	return h
}

// TestBucketLifecycle_Versions round-trips the rules on versions.
func TestBucketLifecycle_Versions(t *testing.T) {
	h := lifecycleEngineHandler(t)

	put := lifecycleRequest(t, h, http.MethodPut, `<LifecycleConfiguration>
		<Rule>
			<ID>history</ID>
			<Status>Enabled</Status>
			<Filter><Prefix>docs/</Prefix></Filter>
			<NoncurrentVersionExpiration>
				<NoncurrentDays>30</NoncurrentDays>
				<NewerNoncurrentVersions>5</NewerNoncurrentVersions>
			</NoncurrentVersionExpiration>
		</Rule>
		<Rule>
			<ID>markers</ID>
			<Status>Enabled</Status>
			<Prefix></Prefix>
			<Expiration><ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker></Expiration>
			<NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays></NoncurrentVersionExpiration>
		</Rule>
	</LifecycleConfiguration>`)
	require.Equal(t, http.StatusOK, put.Code, put.Body.String())

	get := lifecycleRequest(t, h, http.MethodGet, "")
	require.Equal(t, http.StatusOK, get.Code)

	var doc handler.LifecycleConfigurationXML
	require.NoError(t, xml.Unmarshal(get.Body.Bytes(), &doc))
	require.Len(t, doc.Rules, 2)

	nve := doc.Rules[0].NoncurrentVersionExpiration
	require.NotNil(t, nve)
	require.NotNil(t, nve.NoncurrentDays)
	require.Equal(t, 30, *nve.NoncurrentDays)
	require.NotNil(t, nve.NewerNoncurrentVersions)
	require.Equal(t, 5, *nve.NewerNoncurrentVersions)
	require.Nil(t, doc.Rules[0].Expiration)

	exp := doc.Rules[1].Expiration
	require.NotNil(t, exp)
	require.NotNil(t, exp.ExpiredObjectDeleteMarker)
	require.True(t, *exp.ExpiredObjectDeleteMarker)
	require.Zero(t, exp.Days)
	require.Empty(t, exp.Date)
	require.NotNil(t, doc.Rules[1].NoncurrentVersionExpiration)
	require.Nil(t, doc.Rules[1].NoncurrentVersionExpiration.NewerNoncurrentVersions)

	// The rendered document carries no element the client did not send.
	require.NotContains(t, get.Body.String(), "<NewerNoncurrentVersions>0")
}

// TestBucketLifecycle_VersionsValidation pins the refusals, checked against
// the AWS documentation and, where it names no error, Ceph RGW 20.2.
func TestBucketLifecycle_VersionsValidation(t *testing.T) {
	const filter = `<Filter><Prefix></Prefix></Filter>`

	for name, tc := range map[string]struct {
		body string
		code string
	}{
		"MarkerWithDays": {
			body: `<Rule><ID>x</ID><Status>Enabled</Status>` + filter + `
				<Expiration><Days>1</Days><ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker></Expiration></Rule>`,
			code: "MalformedXML",
		},
		"MarkerWithDate": {
			body: `<Rule><ID>x</ID><Status>Enabled</Status>` + filter + `
				<Expiration><Date>2030-01-01T00:00:00Z</Date><ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker></Expiration></Rule>`,
			code: "MalformedXML",
		},
		"NewerWithoutDays": {
			body: `<Rule><ID>x</ID><Status>Enabled</Status>` + filter + `
				<NoncurrentVersionExpiration><NewerNoncurrentVersions>3</NewerNoncurrentVersions></NoncurrentVersionExpiration></Rule>`,
			code: "MalformedXML",
		},
		"Empty": {
			body: `<Rule><ID>x</ID><Status>Enabled</Status>` + filter + `
				<NoncurrentVersionExpiration></NoncurrentVersionExpiration></Rule>`,
			code: "MalformedXML",
		},
		"ZeroDays": {
			body: `<Rule><ID>x</ID><Status>Enabled</Status>` + filter + `
				<NoncurrentVersionExpiration><NoncurrentDays>0</NoncurrentDays></NoncurrentVersionExpiration></Rule>`,
			code: "InvalidArgument",
		},
		"ZeroNewer": {
			body: `<Rule><ID>x</ID><Status>Enabled</Status>` + filter + `
				<NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays><NewerNoncurrentVersions>0</NewerNoncurrentVersions></NoncurrentVersionExpiration></Rule>`,
			code: "InvalidArgument",
		},
		"NewerOver100": {
			body: `<Rule><ID>x</ID><Status>Enabled</Status>` + filter + `
				<NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays><NewerNoncurrentVersions>101</NewerNoncurrentVersions></NoncurrentVersionExpiration></Rule>`,
			code: "InvalidArgument",
		},
		"NewerWithoutFilter": {
			body: `<Rule><ID>x</ID><Status>Enabled</Status><Prefix></Prefix>
				<NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays><NewerNoncurrentVersions>3</NewerNoncurrentVersions></NoncurrentVersionExpiration></Rule>`,
			code: "InvalidRequest",
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := lifecycleEngineHandler(t)

			rec := lifecycleRequest(t, h, http.MethodPut,
				"<LifecycleConfiguration>"+tc.body+"</LifecycleConfiguration>")
			require.Equal(t, tc.code, errorCode(t, rec.Body.String()), rec.Body.String())

			get := lifecycleRequest(t, h, http.MethodGet, "")
			require.Equal(t, http.StatusNotFound, get.Code)
		})
	}
}

// TestBucketLifecycle_Filters round-trips each filter shape: a single
// predicate comes back bare and several inside And, as S3 renders them.
func TestBucketLifecycle_Filters(t *testing.T) {
	const expire = `<Expiration><Days>1</Days></Expiration>`

	for name, tc := range map[string]struct {
		filter string
		check  func(t *testing.T, f *handler.LifecycleFilterXML)
	}{
		"Tag": {
			filter: `<Tag><Key>tmp</Key><Value>true</Value></Tag>`,
			check: func(t *testing.T, f *handler.LifecycleFilterXML) {
				requireTag(t, f.Tag, "tmp", "true")
				require.Nil(t, f.And)
				require.Nil(t, f.Prefix)
			},
		},
		"EmptyTagValue": {
			filter: `<Tag><Key>tmp</Key><Value></Value></Tag>`,
			check: func(t *testing.T, f *handler.LifecycleFilterXML) {
				requireTag(t, f.Tag, "tmp", "")
			},
		},
		"GreaterThan": {
			filter: `<ObjectSizeGreaterThan>0</ObjectSizeGreaterThan>`,
			check: func(t *testing.T, f *handler.LifecycleFilterXML) {
				require.NotNil(t, f.ObjectSizeGreaterThan)
				require.Equal(t, int64(0), *f.ObjectSizeGreaterThan)
				require.Nil(t, f.ObjectSizeLessThan)
			},
		},
		"LessThan": {
			filter: `<ObjectSizeLessThan>1024</ObjectSizeLessThan>`,
			check: func(t *testing.T, f *handler.LifecycleFilterXML) {
				require.NotNil(t, f.ObjectSizeLessThan)
				require.Equal(t, int64(1024), *f.ObjectSizeLessThan)
			},
		},
		"And": {
			filter: `<And><Prefix>logs/</Prefix><Tag><Key>a</Key><Value>1</Value></Tag><Tag><Key>b</Key><Value>2</Value></Tag>
				<ObjectSizeGreaterThan>10</ObjectSizeGreaterThan><ObjectSizeLessThan>100</ObjectSizeLessThan></And>`,
			check: func(t *testing.T, f *handler.LifecycleFilterXML) {
				require.NotNil(t, f.And)
				require.Nil(t, f.Tag)
				require.Equal(t, "logs/", *f.And.Prefix)
				require.Len(t, f.And.Tags, 2)
				requireTag(t, &f.And.Tags[0], "a", "1")
				requireTag(t, &f.And.Tags[1], "b", "2")
				require.Equal(t, int64(10), *f.And.ObjectSizeGreaterThan)
				require.Equal(t, int64(100), *f.And.ObjectSizeLessThan)
			},
		},
		"PrefixAndTag": {
			// A prefix and one tag are two predicates, so they stay in And.
			filter: `<And><Prefix>logs/</Prefix><Tag><Key>a</Key><Value>1</Value></Tag></And>`,
			check: func(t *testing.T, f *handler.LifecycleFilterXML) {
				require.NotNil(t, f.And)
				require.Equal(t, "logs/", *f.And.Prefix)
				require.Len(t, f.And.Tags, 1)
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newLifecycleHandler(t)

			put := lifecycleRequest(t, h, http.MethodPut, `<LifecycleConfiguration><Rule><ID>f</ID><Status>Enabled</Status>
				<Filter>`+tc.filter+`</Filter>`+expire+`</Rule></LifecycleConfiguration>`)
			require.Equal(t, http.StatusOK, put.Code, put.Body.String())

			get := lifecycleRequest(t, h, http.MethodGet, "")
			require.Equal(t, http.StatusOK, get.Code)

			var doc handler.LifecycleConfigurationXML
			require.NoError(t, xml.Unmarshal(get.Body.Bytes(), &doc))
			require.Len(t, doc.Rules, 1)
			require.NotNil(t, doc.Rules[0].Filter)
			tc.check(t, doc.Rules[0].Filter)
		})
	}
}

// TestBucketLifecycle_FiltersValidation covers the design's validation table
// (docs/design/lifecycle-filters.md), on the engine so that the version
// actions are accepted where a filter allows them.
func TestBucketLifecycle_FiltersValidation(t *testing.T) {
	const (
		expire  = `<Expiration><Days>1</Days></Expiration>`
		marker  = `<Expiration><ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker></Expiration>`
		abort   = `<AbortIncompleteMultipartUpload><DaysAfterInitiation>1</DaysAfterInitiation></AbortIncompleteMultipartUpload>`
		tag     = `<Tag><Key>k</Key><Value>v</Value></Tag>`
		maxSize = "1099511627776000"
		over    = "1099511627776001"
	)

	for name, tc := range map[string]struct {
		filter, action string
		code           string // Empty: accepted.
	}{
		"BarePrefixAndTag":  {filter: `<Prefix>a/</Prefix>` + tag, action: expire, code: "MalformedXML"},
		"BareBothSizes":     {filter: `<ObjectSizeGreaterThan>1</ObjectSizeGreaterThan><ObjectSizeLessThan>9</ObjectSizeLessThan>`, action: expire, code: "MalformedXML"},
		"AndOfPrefix":       {filter: `<And><Prefix>a/</Prefix></And>`, action: expire, code: "MalformedXML"},
		"AndOfTag":          {filter: `<And>` + tag + `</And>`, action: expire, code: "MalformedXML"},
		"AndOfSize":         {filter: `<And><ObjectSizeGreaterThan>5</ObjectSizeGreaterThan></And>`, action: expire, code: "MalformedXML"},
		"EmptyAnd":          {filter: `<And></And>`, action: expire, code: "MalformedXML"},
		"TagWithoutValue":   {filter: `<Tag><Key>k</Key></Tag>`, action: expire, code: "MalformedXML"},
		"GreaterEqualLess":  {filter: `<And><ObjectSizeGreaterThan>100</ObjectSizeGreaterThan><ObjectSizeLessThan>100</ObjectSizeLessThan></And>`, action: expire, code: "InvalidRequest"},
		"GreaterAboveLess":  {filter: `<And><ObjectSizeGreaterThan>200</ObjectSizeGreaterThan><ObjectSizeLessThan>100</ObjectSizeLessThan></And>`, action: expire, code: "InvalidRequest"},
		"NegativeGreater":   {filter: `<ObjectSizeGreaterThan>-1</ObjectSizeGreaterThan>`, action: expire, code: "InvalidRequest"},
		"GreaterOverMax":    {filter: `<ObjectSizeGreaterThan>` + over + `</ObjectSizeGreaterThan>`, action: expire, code: "InvalidRequest"},
		"ZeroLess":          {filter: `<ObjectSizeLessThan>0</ObjectSizeLessThan>`, action: expire, code: "InvalidRequest"},
		"LessOverMax":       {filter: `<ObjectSizeLessThan>` + over + `</ObjectSizeLessThan>`, action: expire, code: "InvalidRequest"},
		"TagWithMarker":     {filter: tag, action: marker, code: "InvalidRequest"},
		"AndTagWithMarker":  {filter: `<And><Prefix>a/</Prefix>` + tag + `</And>`, action: marker, code: "InvalidRequest"},
		"TagWithAbort":      {filter: tag, action: abort, code: "InvalidRequest"},
		"SizeWithAbort":     {filter: `<ObjectSizeGreaterThan>1</ObjectSizeGreaterThan>`, action: abort, code: "InvalidRequest"},
		"SizeWithMarker":    {filter: `<ObjectSizeLessThan>1</ObjectSizeLessThan>`, action: marker, code: "InvalidRequest"},
		"EmptyTagKey":       {filter: `<Tag><Key></Key><Value>v</Value></Tag>`, action: expire, code: "InvalidRequest"},
		"LongTagKey":        {filter: `<Tag><Key>` + strings.Repeat("k", 129) + `</Key><Value>v</Value></Tag>`, action: expire, code: "InvalidRequest"},
		"LongTagValue":      {filter: `<Tag><Key>k</Key><Value>` + strings.Repeat("v", 257) + `</Value></Tag>`, action: expire, code: "InvalidRequest"},
		"DuplicateTagKeys":  {filter: `<And>` + tag + `<Tag><Key>k</Key><Value>w</Value></Tag></And>`, action: expire, code: "InvalidRequest"},
		"ZeroGreater":       {filter: `<ObjectSizeGreaterThan>0</ObjectSizeGreaterThan>`, action: expire},
		"MaxBounds":         {filter: `<And><ObjectSizeGreaterThan>1</ObjectSizeGreaterThan><ObjectSizeLessThan>` + maxSize + `</ObjectSizeLessThan></And>`, action: expire},
		"EmptyTagValue":     {filter: `<Tag><Key>k</Key><Value></Value></Tag>`, action: expire},
		"PrefixWithAbort":   {filter: `<Prefix>a/</Prefix>`, action: abort},
		"ElevenTags":        {filter: `<And>` + elevenTags() + `</And>`, action: expire},
		"TagWithNoncurrent": {filter: tag, action: `<NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays></NoncurrentVersionExpiration>`},
	} {
		t.Run(name, func(t *testing.T) {
			h := lifecycleEngineHandler(t)

			rec := lifecycleRequest(t, h, http.MethodPut, `<LifecycleConfiguration><Rule><ID>x</ID><Status>Enabled</Status>
				<Filter>`+tc.filter+`</Filter>`+tc.action+`</Rule></LifecycleConfiguration>`)

			get := lifecycleRequest(t, h, http.MethodGet, "")

			if tc.code == "" {
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				require.Equal(t, http.StatusOK, get.Code)

				return
			}

			require.Equal(t, tc.code, errorCode(t, rec.Body.String()), rec.Body.String())
			require.Equal(t, http.StatusNotFound, get.Code)
		})
	}
}

func requireTag(t *testing.T, tag *handler.LifecycleTagXML, key, value string) {
	t.Helper()

	require.NotNil(t, tag)
	require.Equal(t, key, tag.Key)
	require.NotNil(t, tag.Value)
	require.Equal(t, value, *tag.Value)
}

// elevenTags is one more tag than an object may carry, which a rule may
// still require.
func elevenTags() string {
	var b strings.Builder

	for i := range 11 {
		fmt.Fprintf(&b, "<Tag><Key>k%d</Key><Value>v</Value></Tag>", i)
	}

	return b.String()
}

// TestBucketLifecycle_MarkerFalse: AWS accepts a rule whose only action is
// ExpiredObjectDeleteMarker false, and returns it as it was sent.
func TestBucketLifecycle_MarkerFalse(t *testing.T) {
	h := lifecycleEngineHandler(t)

	put := lifecycleRequest(t, h, http.MethodPut, `<LifecycleConfiguration><Rule><ID>x</ID><Status>Enabled</Status>
		<Filter><Prefix></Prefix></Filter>
		<Expiration><ExpiredObjectDeleteMarker>false</ExpiredObjectDeleteMarker></Expiration></Rule></LifecycleConfiguration>`)
	require.Equal(t, http.StatusOK, put.Code, put.Body.String())

	get := lifecycleRequest(t, h, http.MethodGet, "")
	require.Equal(t, http.StatusOK, get.Code)

	var doc handler.LifecycleConfigurationXML
	require.NoError(t, xml.Unmarshal(get.Body.Bytes(), &doc))
	require.Len(t, doc.Rules, 1)
	require.NotNil(t, doc.Rules[0].Expiration)
	require.NotNil(t, doc.Rules[0].Expiration.ExpiredObjectDeleteMarker)
	require.False(t, *doc.Rules[0].Expiration.ExpiredObjectDeleteMarker)
}
