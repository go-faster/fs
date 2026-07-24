// Package fs contains the go:generate directives for the repository's
// generated code.
package fs

//go:generate go tool ogen --target adminapi --package adminapi --clean _oas/admin.yml
