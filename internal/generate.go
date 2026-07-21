// Package internal contains go:generate annotations.
package internal

//go:generate go tool ogen --target adminapi --package adminapi --clean ../_oas/admin.yml
