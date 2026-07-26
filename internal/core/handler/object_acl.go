package handler

import (
	"context"
	"encoding/xml"
	"net/http"
	"strings"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/s3err"
)

// The canonical grantee URIs S3 uses for its two predefined groups.
const (
	allUsersURI = "http://acs.amazonaws.com/groups/global/AllUsers"
)

// AccessControlPolicy is the XML document of the ?acl subresource, for both
// GET (rendered from the stored canned level) and PUT (parsed for a canned
// equivalent).
//
// Only the canned subset is modeled: the server stores one access level per
// object, not a grant table, so a GET renders the grants that level implies and
// a PUT is reduced back to the nearest canned level. Arbitrary grantees are not
// honored — see COMPATIBILITY.md.
type AccessControlPolicy struct {
	XMLName           xml.Name  `xml:"AccessControlPolicy"`
	Xmlns             string    `xml:"xmlns,attr,omitempty"`
	Owner             OwnerXML  `xml:"Owner"`
	AccessControlList GrantList `xml:"AccessControlList"`
}

// OwnerXML is the <Owner> element shared by ACL and listing responses.
type OwnerXML struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName,omitempty"`
}

// GrantList wraps the repeated <Grant> elements.
type GrantList struct {
	Grants []GrantXML `xml:"Grant"`
}

// GrantXML is a single grantee/permission pair.
type GrantXML struct {
	Grantee    GranteeXML `xml:"Grantee"`
	Permission string     `xml:"Permission"`
}

// GranteeXML identifies who a grant applies to. Type is an XSI attribute, which
// is how S3 distinguishes a CanonicalUser from a predefined Group.
type GranteeXML struct {
	XMLNS       string `xml:"xmlns:xsi,attr,omitempty"`
	Type        string `xml:"xsi:type,attr"`
	ID          string `xml:"ID,omitempty"`
	DisplayName string `xml:"DisplayName,omitempty"`
	URI         string `xml:"URI,omitempty"`
}

// aclPolicy renders the canned level as the grant list S3 reports for it: the
// owner always holds FULL_CONTROL, and the public levels add the AllUsers group
// with READ (public-read) or READ and WRITE (public-read-write).
func aclPolicy(owner OwnerXML, level fs.ACL) AccessControlPolicy {
	grants := []GrantXML{{
		Grantee: GranteeXML{
			XMLNS:       "http://www.w3.org/2001/XMLSchema-instance",
			Type:        "CanonicalUser",
			ID:          owner.ID,
			DisplayName: owner.DisplayName,
		},
		Permission: "FULL_CONTROL",
	}}

	group := func(perm string) GrantXML {
		return GrantXML{
			Grantee: GranteeXML{
				XMLNS: "http://www.w3.org/2001/XMLSchema-instance",
				Type:  "Group",
				URI:   allUsersURI,
			},
			Permission: perm,
		}
	}

	if level.AllowsAnonRead() {
		grants = append(grants, group("READ"))
	}

	if level.AllowsAnonWrite() {
		grants = append(grants, group("WRITE"))
	}

	return AccessControlPolicy{
		Xmlns:             "http://s3.amazonaws.com/doc/2006-03-01/",
		Owner:             owner,
		AccessControlList: GrantList{Grants: grants},
	}
}

// ownerXML renders an owner for the wire, falling back to the calling
// credential for objects written before owners were recorded so the response
// still carries a well-formed <Owner>.
func ownerXML(ctx context.Context, owner fs.Owner) OwnerXML {
	if owner.IsZero() {
		owner = callerOwner(ctx)
	}

	return OwnerXML{ID: owner.ID, DisplayName: owner.DisplayName}
}

// GetObjectACL handles GET on an object with ?acl. The owner reported is the
// principal that wrote the object, not the caller — clients compare it to
// decide ownership, so it must not change with who is asking.
func (h *handler) GetObjectACL(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")

	level, err := h.service.ObjectACL(ctx, bucket, key)
	if err != nil {
		renderError(ctx, w, r, err)
		return
	}

	owner, err := h.service.ObjectOwner(ctx, bucket, key)
	if err != nil {
		renderError(ctx, w, r, err)
		return
	}

	writeXML(ctx, w, r, aclPolicy(ownerXML(ctx, owner), level))
}

// PutObjectACL handles PUT on an object with ?acl. The level comes from the
// x-amz-acl canned header when present, otherwise from the AccessControlPolicy
// body reduced to its nearest canned equivalent.
func (h *handler) PutObjectACL(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")

	level, err := requestedACL(r)
	if err != nil {
		renderAPIError(ctx, w, r, s3err.MalformedXML, err)
		return
	}

	if err := h.service.SetObjectACL(ctx, bucket, key, level); err != nil {
		renderError(ctx, w, r, err)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// requestedACL derives the canned level a PUT ?acl asks for. A canned x-amz-acl
// header wins; otherwise the body is parsed and its AllUsers grants are reduced
// to the matching canned level (grants to specific users are ignored, since the
// server models only the public/private distinction).
func requestedACL(r *http.Request) (fs.ACL, error) {
	if canned := r.Header.Get("x-amz-acl"); canned != "" {
		return fs.ParseACL(canned), nil
	}

	var doc AccessControlPolicy
	if err := xml.NewDecoder(r.Body).Decode(&doc); err != nil {
		return fs.ACLPrivate, err
	}

	level := fs.ACLPrivate

	for _, g := range doc.AccessControlList.Grants {
		if g.Grantee.URI != allUsersURI {
			continue
		}

		switch g.Permission {
		case "WRITE", "FULL_CONTROL":
			return fs.ACLPublicReadWrite, nil
		case "READ":
			level = fs.ACLPublicRead
		}
	}

	return level, nil
}
