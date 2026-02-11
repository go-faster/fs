package handler

import "time"

// BucketInfo is the XML representation of a bucket.
type BucketInfo struct {
	Name         string    `xml:"Name"`
	CreationDate time.Time `xml:"CreationDate"`
}
