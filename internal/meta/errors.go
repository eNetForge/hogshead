package meta

import "fmt"

// Code is a stable, serialisable error code. The S3 layer maps codes to
// S3 error responses. Codes are stable so that clients and the CLI can match
// on them.
type Code string

// Error codes returned by Apply and queries.
const (
	CodeNoSuchBucket           Code = "NoSuchBucket"
	CodeBucketAlreadyExists    Code = "BucketAlreadyExists"
	CodeBucketAlreadyOwned     Code = "BucketAlreadyOwnedByYou"
	CodeBucketNotEmpty         Code = "BucketNotEmpty"
	CodeNoSuchKey              Code = "NoSuchKey"
	CodeNoSuchVersion          Code = "NoSuchVersion"
	CodeNoSuchUpload           Code = "NoSuchUpload"
	CodeInvalidPart            Code = "InvalidPart"
	CodeInvalidPartOrder       Code = "InvalidPartOrder"
	CodeEntityTooSmall         Code = "EntityTooSmall"
	CodeNoSuchUser             Code = "NoSuchUser"
	CodeNoSuchTransfer         Code = "NoSuchTransfer"
	CodeUserExists             Code = "UserAlreadyExists"
	CodeNoSuchKeyID            Code = "NoSuchAccessKey"
	CodeInvalidArgument        Code = "InvalidArgument"
	CodePreconditionFailed     Code = "PreconditionFailed"
	CodeNoSuchLifecycle        Code = "NoSuchLifecycleConfiguration"
	CodeNoSuchCORS             Code = "NoSuchCORSConfiguration"
	CodeNoSuchBucketPolicy     Code = "NoSuchBucketPolicy"
	CodeInternal               Code = "InternalError"
	CodeInvalidBucketState     Code = "InvalidBucketState"
	CodeObjectLockConfigAbsent Code = "ObjectLockConfigurationNotFoundError"
)

// Error is an error carrying a Code.
type Error struct {
	Code Code
	Msg  string
}

func (e *Error) Error() string {
	if e.Msg == "" {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Msg
}

// Is lets errors.Is compare by code.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

// E builds an Error.
func E(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// Sentinel errors for errors.Is.
var (
	ErrNoSuchBucket   = &Error{Code: CodeNoSuchBucket}
	ErrNoSuchKey      = &Error{Code: CodeNoSuchKey}
	ErrNoSuchVersion  = &Error{Code: CodeNoSuchVersion}
	ErrNoSuchUpload   = &Error{Code: CodeNoSuchUpload}
	ErrNoSuchUser     = &Error{Code: CodeNoSuchUser}
	ErrNoSuchTransfer = &Error{Code: CodeNoSuchTransfer}
	ErrNoSuchKeyID    = &Error{Code: CodeNoSuchKeyID}
)
