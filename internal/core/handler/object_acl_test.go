package handler_test

import (
	"encoding/xml"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// aclPolicyDoc mirrors the response shape without depending on the handler's
// unexported wire structs, so the test pins the XML a client actually sees.
type aclPolicyDoc struct {
	XMLName xml.Name `xml:"AccessControlPolicy"`
	Owner   struct {
		ID          string `xml:"ID"`
		DisplayName string `xml:"DisplayName"`
	} `xml:"Owner"`
	Grants []struct {
		Grantee struct {
			Type string `xml:"type,attr"`
			ID   string `xml:"ID"`
			URI  string `xml:"URI"`
		} `xml:"Grantee"`
		Permission string `xml:"Permission"`
	} `xml:"AccessControlList>Grant"`
}

func getACL(t testing.TB, h http.Handler, target string) aclPolicyDoc {
	t.Helper()

	rec := do(t, h, http.MethodGet, target, "", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var doc aclPolicyDoc
	require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &doc))

	return doc
}

// PUT ?acl used to fall through to PutObject, storing the ACL document as the
// object's content and destroying it. It must only change the access level.
func TestPutObjectACLPreservesContent(t *testing.T) {
	h := newStorageHandler(t)

	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bkt", "", nil).Code)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bkt/k", "payload", nil).Code)

	rec := do(t, h, http.MethodPut, "/bkt/k?acl", "", map[string]string{"x-amz-acl": "public-read"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	got := do(t, h, http.MethodGet, "/bkt/k", "", nil)
	require.Equal(t, http.StatusOK, got.Code)
	require.Equal(t, "payload", got.Body.String(), "PUT ?acl must not overwrite the object")

	require.Equal(t, "public-read", getACLLevel(t, h))
}

// getACLLevel reads the stored level back through the canned-ACL grants.
func getACLLevel(t testing.TB, h http.Handler) string {
	t.Helper()

	doc := getACL(t, h, "/bkt/k?acl")

	var read, write bool

	for _, g := range doc.Grants {
		if g.Grantee.URI != "http://acs.amazonaws.com/groups/global/AllUsers" {
			continue
		}

		switch g.Permission {
		case "READ":
			read = true
		case "WRITE":
			write = true
		}
	}

	switch {
	case read && write:
		return "public-read-write"
	case read:
		return "public-read"
	default:
		return "private"
	}
}

func TestGetObjectACLDefaultsToOwnerFullControl(t *testing.T) {
	h := newStorageHandler(t)

	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bkt", "", nil).Code)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bkt/k", "x", nil).Code)

	doc := getACL(t, h, "/bkt/k?acl")

	require.Len(t, doc.Grants, 1, "a private object grants only its owner")
	require.Equal(t, "FULL_CONTROL", doc.Grants[0].Permission)
	require.Equal(t, "CanonicalUser", doc.Grants[0].Grantee.Type)
	require.NotEmpty(t, doc.Owner.ID)
	require.Equal(t, doc.Owner.ID, doc.Grants[0].Grantee.ID)
}

func TestPutObjectACLFromPolicyBody(t *testing.T) {
	h := newStorageHandler(t)

	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bkt", "", nil).Code)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bkt/k", "payload", nil).Code)

	body := `<AccessControlPolicy><Owner><ID>me</ID></Owner><AccessControlList>` +
		`<Grant><Grantee xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="Group">` +
		`<URI>http://acs.amazonaws.com/groups/global/AllUsers</URI></Grantee><Permission>READ</Permission></Grant>` +
		`</AccessControlList></AccessControlPolicy>`

	rec := do(t, h, http.MethodPut, "/bkt/k?acl", body, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	require.Equal(t, "public-read", getACLLevel(t, h))
	require.Equal(t, "payload", do(t, h, http.MethodGet, "/bkt/k", "", nil).Body.String())
}

func TestGetObjectACLPublicReadWriteGrants(t *testing.T) {
	h := newStorageHandler(t)

	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bkt", "", nil).Code)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bkt/k", "x",
		map[string]string{"x-amz-acl": "public-read-write"}).Code)

	doc := getACL(t, h, "/bkt/k?acl")
	require.Len(t, doc.Grants, 3, "owner FULL_CONTROL plus AllUsers READ and WRITE")
	require.Equal(t, "public-read-write", getACLLevel(t, h))
}

func TestObjectACLMissingObject(t *testing.T) {
	h := newStorageHandler(t)

	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bkt", "", nil).Code)

	require.Equal(t, http.StatusNotFound, do(t, h, http.MethodGet, "/bkt/nope?acl", "", nil).Code)
	require.Equal(t, http.StatusNotFound,
		do(t, h, http.MethodPut, "/bkt/nope?acl", "", map[string]string{"x-amz-acl": "public-read"}).Code)
}
