package adminhandler

import "github.com/go-faster/fs/adminapi"

// apiErr builds an *adminapi.ErrorStatusCode with the given HTTP status and
// message. Returning it from a handler makes ogen encode the structured Error
// body with that status.
func apiErr(status int, err error) *adminapi.ErrorStatusCode {
	return &adminapi.ErrorStatusCode{
		StatusCode: status,
		Response:   adminapi.Error{ErrorMessage: err.Error()},
	}
}
