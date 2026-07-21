package fs

// ACL is a canned S3 access-control level applied to a bucket or object. Only
// the canned subset is modeled — full ACL grammar (arbitrary grantees,
// AccessControlPolicy XML) is out of scope; the `?acl` subresource is
// echo-only.
type ACL string

const (
	// ACLPrivate grants no anonymous access (the default).
	ACLPrivate ACL = "private"
	// ACLPublicRead allows anonymous reads.
	ACLPublicRead ACL = "public-read"
	// ACLPublicReadWrite allows anonymous reads and writes.
	ACLPublicReadWrite ACL = "public-read-write"
)

// ParseACL normalizes a canned ACL header value, defaulting empty or
// unrecognized values to ACLPrivate.
func ParseACL(s string) ACL {
	switch ACL(s) {
	case ACLPublicRead:
		return ACLPublicRead
	case ACLPublicReadWrite:
		return ACLPublicReadWrite
	default:
		return ACLPrivate
	}
}

// AllowsAnonRead reports whether the level permits anonymous reads.
func (a ACL) AllowsAnonRead() bool {
	return a == ACLPublicRead || a == ACLPublicReadWrite
}

// AllowsAnonWrite reports whether the level permits anonymous writes.
func (a ACL) AllowsAnonWrite() bool {
	return a == ACLPublicReadWrite
}
