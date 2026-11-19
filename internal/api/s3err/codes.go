package s3err

import (
	"net/http"
	"sort"
)

// S3 error codes. Messages and statuses follow the AWS S3 error responses
// reference; the trailing group are hogshead specific codes.
const (
	AccessDenied                            Code = "AccessDenied"
	AccountProblem                          Code = "AccountProblem"
	AuthorizationHeaderMalformed            Code = "AuthorizationHeaderMalformed"
	AuthorizationQueryParametersError       Code = "AuthorizationQueryParametersError"
	BadDigest                               Code = "BadDigest"
	BucketAlreadyExists                     Code = "BucketAlreadyExists"
	BucketAlreadyOwnedByYou                 Code = "BucketAlreadyOwnedByYou"
	BucketNotEmpty                          Code = "BucketNotEmpty"
	ConditionalRequestConflict              Code = "ConditionalRequestConflict"
	CredentialsNotSupported                 Code = "CredentialsNotSupported"
	EntityTooSmall                          Code = "EntityTooSmall"
	EntityTooLarge                          Code = "EntityTooLarge"
	ExpiredToken                            Code = "ExpiredToken"
	IllegalVersioningConfigurationException Code = "IllegalVersioningConfigurationException"
	IncompleteBody                          Code = "IncompleteBody"
	IncorrectNumberOfFilesInPostRequest     Code = "IncorrectNumberOfFilesInPostRequest"
	InlineDataTooLarge                      Code = "InlineDataTooLarge"
	InternalError                           Code = "InternalError"
	InvalidAccessKeyID                      Code = "InvalidAccessKeyId"
	InvalidArgument                         Code = "InvalidArgument"
	InvalidBucketName                       Code = "InvalidBucketName"
	InvalidBucketState                      Code = "InvalidBucketState"
	InvalidDigest                           Code = "InvalidDigest"
	InvalidEncryptionAlgorithmError         Code = "InvalidEncryptionAlgorithmError"
	InvalidLocationConstraint               Code = "InvalidLocationConstraint"
	InvalidObjectState                      Code = "InvalidObjectState"
	InvalidPart                             Code = "InvalidPart"
	InvalidPartOrder                        Code = "InvalidPartOrder"
	InvalidPolicyDocument                   Code = "InvalidPolicyDocument"
	InvalidRange                            Code = "InvalidRange"
	InvalidRequest                          Code = "InvalidRequest"
	InvalidSecurity                         Code = "InvalidSecurity"
	InvalidStorageClass                     Code = "InvalidStorageClass"
	InvalidTargetBucketForLogging           Code = "InvalidTargetBucketForLogging"
	InvalidToken                            Code = "InvalidToken"
	InvalidURI                              Code = "InvalidURI"
	KeyTooLongError                         Code = "KeyTooLongError"
	MalformedACLError                       Code = "MalformedACLError"
	MalformedPOSTRequest                    Code = "MalformedPOSTRequest"
	MalformedPolicy                         Code = "MalformedPolicy"
	MalformedXML                            Code = "MalformedXML"
	MaxMessageLengthExceeded                Code = "MaxMessageLengthExceeded"
	MetadataTooLarge                        Code = "MetadataTooLarge"
	MethodNotAllowed                        Code = "MethodNotAllowed"
	MissingContentLength                    Code = "MissingContentLength"
	MissingRequestBodyError                 Code = "MissingRequestBodyError"
	MissingSecurityHeader                   Code = "MissingSecurityHeader"
	NoSuchAccessKey                         Code = "NoSuchAccessKey"
	NoSuchBucket                            Code = "NoSuchBucket"
	NoSuchBucketPolicy                      Code = "NoSuchBucketPolicy"
	NoSuchCORSConfiguration                 Code = "NoSuchCORSConfiguration"
	NoSuchKey                               Code = "NoSuchKey"
	NoSuchLifecycleConfiguration            Code = "NoSuchLifecycleConfiguration"
	NoSuchNode                              Code = "NoSuchNode"
	NoSuchObjectLockConfiguration           Code = "NoSuchObjectLockConfiguration"
	NoSuchTagSet                            Code = "NoSuchTagSet"
	InvalidTag                              Code = "InvalidTag"
	InvalidPartNumber                       Code = "InvalidPartNumber"
	NoSuchUpload                            Code = "NoSuchUpload"
	NoSuchUser                              Code = "NoSuchUser"
	NoSuchVersion                           Code = "NoSuchVersion"
	NotImplemented                          Code = "NotImplemented"
	NotModified                             Code = "NotModified"
	ObjectLockConfigurationNotFoundError    Code = "ObjectLockConfigurationNotFoundError"
	OperationAborted                        Code = "OperationAborted"
	PermanentRedirect                       Code = "PermanentRedirect"
	PreconditionFailed                      Code = "PreconditionFailed"
	ReadOnly                                Code = "ReadOnly"
	Redirect                                Code = "Redirect"
	RequestTimeout                          Code = "RequestTimeout"
	RequestTimeTooSkewed                    Code = "RequestTimeTooSkewed"
	RequestedRangeNotSatisfiable            Code = "RequestedRangeNotSatisfiable"
	ServiceUnavailable                      Code = "ServiceUnavailable"
	SignatureDoesNotMatch                   Code = "SignatureDoesNotMatch"
	SlowDown                                Code = "SlowDown"
	TemporaryRedirect                       Code = "TemporaryRedirect"
	TooManyBuckets                          Code = "TooManyBuckets"
	UnexpectedContent                       Code = "UnexpectedContent"
	UnresolvableGrantByEmailAddress         Code = "UnresolvableGrantByEmailAddress"
	UserAlreadyExists                       Code = "UserAlreadyExists"
	UserKeyMustBeSpecified                  Code = "UserKeyMustBeSpecified"
	XAmzContentSHA256Mismatch               Code = "XAmzContentSHA256Mismatch"
)

// registry maps every code to its template. Entries are never handed out
// directly; New always copies.
var registry = map[Code]Error{
	AccessDenied:                            {Code: AccessDenied, HTTPStatus: http.StatusForbidden, Message: "Access Denied"},
	AccountProblem:                          {Code: AccountProblem, HTTPStatus: http.StatusForbidden, Message: "There is a problem with your AWS account that prevents the action from completing successfully. Contact AWS Support for further assistance."},
	AuthorizationHeaderMalformed:            {Code: AuthorizationHeaderMalformed, HTTPStatus: http.StatusBadRequest, Message: "The authorization header you provided is invalid."},
	AuthorizationQueryParametersError:       {Code: AuthorizationQueryParametersError, HTTPStatus: http.StatusBadRequest, Message: "The authorization query parameters that you provided are not valid."},
	BadDigest:                               {Code: BadDigest, HTTPStatus: http.StatusBadRequest, Message: "The Content-MD5 or checksum value that you specified did not match what the server received."},
	BucketAlreadyExists:                     {Code: BucketAlreadyExists, HTTPStatus: http.StatusConflict, Message: "The requested bucket name is not available. The bucket namespace is shared by all users of the system. Please select a different name and try again."},
	BucketAlreadyOwnedByYou:                 {Code: BucketAlreadyOwnedByYou, HTTPStatus: http.StatusConflict, Message: "The bucket you tried to create already exists, and you own it."},
	BucketNotEmpty:                          {Code: BucketNotEmpty, HTTPStatus: http.StatusConflict, Message: "The bucket you tried to delete is not empty."},
	ConditionalRequestConflict:              {Code: ConditionalRequestConflict, HTTPStatus: http.StatusConflict, Message: "The conditional request cannot succeed due to a conflicting operation against this resource."},
	CredentialsNotSupported:                 {Code: CredentialsNotSupported, HTTPStatus: http.StatusBadRequest, Message: "This request does not support credentials."},
	EntityTooSmall:                          {Code: EntityTooSmall, HTTPStatus: http.StatusBadRequest, Message: "Your proposed upload is smaller than the minimum allowed object size."},
	EntityTooLarge:                          {Code: EntityTooLarge, HTTPStatus: http.StatusBadRequest, Message: "Your proposed upload exceeds the maximum allowed object size."},
	ExpiredToken:                            {Code: ExpiredToken, HTTPStatus: http.StatusBadRequest, Message: "The provided token has expired."},
	IllegalVersioningConfigurationException: {Code: IllegalVersioningConfigurationException, HTTPStatus: http.StatusBadRequest, Message: "The versioning configuration specified in the request is invalid."},
	IncompleteBody:                          {Code: IncompleteBody, HTTPStatus: http.StatusBadRequest, Message: "You did not provide the number of bytes specified by the Content-Length HTTP header."},
	IncorrectNumberOfFilesInPostRequest:     {Code: IncorrectNumberOfFilesInPostRequest, HTTPStatus: http.StatusBadRequest, Message: "POST requires exactly one file upload per request."},
	InlineDataTooLarge:                      {Code: InlineDataTooLarge, HTTPStatus: http.StatusBadRequest, Message: "The inline data exceeds the maximum allowed size."},
	InternalError:                           {Code: InternalError, HTTPStatus: http.StatusInternalServerError, Message: "We encountered an internal error. Please try again."},
	InvalidAccessKeyID:                      {Code: InvalidAccessKeyID, HTTPStatus: http.StatusForbidden, Message: "The AWS access key ID that you provided does not exist in our records."},
	InvalidArgument:                         {Code: InvalidArgument, HTTPStatus: http.StatusBadRequest, Message: "Invalid Argument"},
	InvalidBucketName:                       {Code: InvalidBucketName, HTTPStatus: http.StatusBadRequest, Message: "The specified bucket is not valid."},
	InvalidBucketState:                      {Code: InvalidBucketState, HTTPStatus: http.StatusConflict, Message: "The request is not valid for the current state of the bucket."},
	InvalidDigest:                           {Code: InvalidDigest, HTTPStatus: http.StatusBadRequest, Message: "The Content-MD5 or checksum value that you specified is not valid."},
	InvalidEncryptionAlgorithmError:         {Code: InvalidEncryptionAlgorithmError, HTTPStatus: http.StatusBadRequest, Message: "The encryption request that you specified is not valid. The valid value is AES256."},
	InvalidLocationConstraint:               {Code: InvalidLocationConstraint, HTTPStatus: http.StatusBadRequest, Message: "The specified location constraint is not valid."},
	InvalidObjectState:                      {Code: InvalidObjectState, HTTPStatus: http.StatusForbidden, Message: "The action is not valid for the current state of the object."},
	InvalidPart:                             {Code: InvalidPart, HTTPStatus: http.StatusBadRequest, Message: "One or more of the specified parts could not be found. The part might not have been uploaded, or the specified entity tag might not have matched the part's entity tag."},
	InvalidPartOrder:                        {Code: InvalidPartOrder, HTTPStatus: http.StatusBadRequest, Message: "The list of parts was not in ascending order. The parts list must be specified in order by part number."},
	InvalidPolicyDocument:                   {Code: InvalidPolicyDocument, HTTPStatus: http.StatusBadRequest, Message: "The content of the form does not meet the conditions specified in the policy document."},
	InvalidRange:                            {Code: InvalidRange, HTTPStatus: http.StatusRequestedRangeNotSatisfiable, Message: "The requested range cannot be satisfied."},
	InvalidRequest:                          {Code: InvalidRequest, HTTPStatus: http.StatusBadRequest, Message: "Invalid Request"},
	InvalidSecurity:                         {Code: InvalidSecurity, HTTPStatus: http.StatusForbidden, Message: "The provided security credentials are not valid."},
	InvalidStorageClass:                     {Code: InvalidStorageClass, HTTPStatus: http.StatusBadRequest, Message: "The storage class that you specified is not valid."},
	InvalidTargetBucketForLogging:           {Code: InvalidTargetBucketForLogging, HTTPStatus: http.StatusBadRequest, Message: "The target bucket for logging does not exist, is not owned by you, or does not have the appropriate grants for the log-delivery group."},
	InvalidToken:                            {Code: InvalidToken, HTTPStatus: http.StatusBadRequest, Message: "The provided token is malformed or otherwise invalid."},
	InvalidURI:                              {Code: InvalidURI, HTTPStatus: http.StatusBadRequest, Message: "Couldn't parse the specified URI."},
	KeyTooLongError:                         {Code: KeyTooLongError, HTTPStatus: http.StatusBadRequest, Message: "Your key is too long."},
	MalformedACLError:                       {Code: MalformedACLError, HTTPStatus: http.StatusBadRequest, Message: "The XML that you provided was not well formed or did not validate against our published schema."},
	MalformedPOSTRequest:                    {Code: MalformedPOSTRequest, HTTPStatus: http.StatusBadRequest, Message: "The body of your POST request is not well-formed multipart/form-data."},
	MalformedPolicy:                         {Code: MalformedPolicy, HTTPStatus: http.StatusBadRequest, Message: "The policy document is not valid JSON or does not conform to the policy schema."},
	MalformedXML:                            {Code: MalformedXML, HTTPStatus: http.StatusBadRequest, Message: "The XML that you provided was not well formed or did not validate against our published schema."},
	MaxMessageLengthExceeded:                {Code: MaxMessageLengthExceeded, HTTPStatus: http.StatusBadRequest, Message: "Your request was too big."},
	MetadataTooLarge:                        {Code: MetadataTooLarge, HTTPStatus: http.StatusBadRequest, Message: "Your metadata headers exceed the maximum allowed metadata size."},
	MethodNotAllowed:                        {Code: MethodNotAllowed, HTTPStatus: http.StatusMethodNotAllowed, Message: "The specified method is not allowed against this resource."},
	MissingContentLength:                    {Code: MissingContentLength, HTTPStatus: http.StatusLengthRequired, Message: "You must provide the Content-Length HTTP header."},
	MissingRequestBodyError:                 {Code: MissingRequestBodyError, HTTPStatus: http.StatusBadRequest, Message: "Request body is empty."},
	MissingSecurityHeader:                   {Code: MissingSecurityHeader, HTTPStatus: http.StatusBadRequest, Message: "Your request is missing a required header."},
	NoSuchAccessKey:                         {Code: NoSuchAccessKey, HTTPStatus: http.StatusNotFound, Message: "The specified access key does not exist."},
	NoSuchBucket:                            {Code: NoSuchBucket, HTTPStatus: http.StatusNotFound, Message: "The specified bucket does not exist."},
	NoSuchBucketPolicy:                      {Code: NoSuchBucketPolicy, HTTPStatus: http.StatusNotFound, Message: "The specified bucket does not have a bucket policy."},
	NoSuchCORSConfiguration:                 {Code: NoSuchCORSConfiguration, HTTPStatus: http.StatusNotFound, Message: "The CORS configuration does not exist"},
	NoSuchKey:                               {Code: NoSuchKey, HTTPStatus: http.StatusNotFound, Message: "The specified key does not exist."},
	NoSuchLifecycleConfiguration:            {Code: NoSuchLifecycleConfiguration, HTTPStatus: http.StatusNotFound, Message: "The lifecycle configuration does not exist."},
	NoSuchNode:                              {Code: NoSuchNode, HTTPStatus: http.StatusNotFound, Message: "The specified node does not exist."},
	NoSuchObjectLockConfiguration:           {Code: NoSuchObjectLockConfiguration, HTTPStatus: http.StatusNotFound, Message: "The specified object does not have an Object Lock configuration."},
	NoSuchTagSet:                            {Code: NoSuchTagSet, HTTPStatus: http.StatusNotFound, Message: "The TagSet does not exist"},
	InvalidTag:                              {Code: InvalidTag, HTTPStatus: http.StatusBadRequest, Message: "The tag provided was not a valid tag"},
	InvalidPartNumber:                       {Code: InvalidPartNumber, HTTPStatus: http.StatusBadRequest, Message: "The requested partnumber is not satisfiable"},
	NoSuchUpload:                            {Code: NoSuchUpload, HTTPStatus: http.StatusNotFound, Message: "The specified multipart upload does not exist. The upload ID might be invalid, or the multipart upload might have been aborted or completed."},
	NoSuchUser:                              {Code: NoSuchUser, HTTPStatus: http.StatusNotFound, Message: "The specified user does not exist."},
	NoSuchVersion:                           {Code: NoSuchVersion, HTTPStatus: http.StatusNotFound, Message: "The version ID specified in the request does not match an existing version."},
	NotImplemented:                          {Code: NotImplemented, HTTPStatus: http.StatusNotImplemented, Message: "A header that you provided implies functionality that is not implemented."},
	NotModified:                             {Code: NotModified, HTTPStatus: http.StatusNotModified, Message: "Not Modified"},
	ObjectLockConfigurationNotFoundError:    {Code: ObjectLockConfigurationNotFoundError, HTTPStatus: http.StatusNotFound, Message: "Object Lock configuration does not exist for this bucket"},
	OperationAborted:                        {Code: OperationAborted, HTTPStatus: http.StatusConflict, Message: "A conflicting conditional action is currently in progress against this resource. Try again."},
	PermanentRedirect:                       {Code: PermanentRedirect, HTTPStatus: http.StatusMovedPermanently, Message: "The bucket that you are attempting to access must be addressed using the specified endpoint. Send all future requests to this endpoint."},
	PreconditionFailed:                      {Code: PreconditionFailed, HTTPStatus: http.StatusPreconditionFailed, Message: "At least one of the preconditions that you specified did not hold."},
	ReadOnly:                                {Code: ReadOnly, HTTPStatus: http.StatusServiceUnavailable, Message: "Server is read-only"},
	Redirect:                                {Code: Redirect, HTTPStatus: http.StatusTemporaryRedirect, Message: "Temporary redirect."},
	RequestTimeout:                          {Code: RequestTimeout, HTTPStatus: http.StatusBadRequest, Message: "Your socket connection to the server was not read from or written to within the timeout period."},
	RequestTimeTooSkewed:                    {Code: RequestTimeTooSkewed, HTTPStatus: http.StatusForbidden, Message: "The difference between the request time and the server's time is too large."},
	RequestedRangeNotSatisfiable:            {Code: RequestedRangeNotSatisfiable, HTTPStatus: http.StatusRequestedRangeNotSatisfiable, Message: "The requested range is not satisfiable."},
	ServiceUnavailable:                      {Code: ServiceUnavailable, HTTPStatus: http.StatusServiceUnavailable, Message: "Service is unable to handle request."},
	SignatureDoesNotMatch:                   {Code: SignatureDoesNotMatch, HTTPStatus: http.StatusForbidden, Message: "The request signature that the server calculated does not match the signature that you provided. Check your AWS secret access key and signing method. For more information, see REST Authentication and SOAP Authentication for details."},
	SlowDown:                                {Code: SlowDown, HTTPStatus: http.StatusServiceUnavailable, Message: "Please reduce your request rate."},
	TemporaryRedirect:                       {Code: TemporaryRedirect, HTTPStatus: http.StatusTemporaryRedirect, Message: "You are being redirected to the bucket while DNS updates."},
	TooManyBuckets:                          {Code: TooManyBuckets, HTTPStatus: http.StatusBadRequest, Message: "You have attempted to create more buckets than allowed."},
	UnexpectedContent:                       {Code: UnexpectedContent, HTTPStatus: http.StatusBadRequest, Message: "This request does not support content."},
	UnresolvableGrantByEmailAddress:         {Code: UnresolvableGrantByEmailAddress, HTTPStatus: http.StatusBadRequest, Message: "The email address that you provided does not match any account on record."},
	UserAlreadyExists:                       {Code: UserAlreadyExists, HTTPStatus: http.StatusConflict, Message: "The specified user already exists."},
	UserKeyMustBeSpecified:                  {Code: UserKeyMustBeSpecified, HTTPStatus: http.StatusBadRequest, Message: "The bucket POST must contain the specified field name. If it is specified, check the order of the fields."},
	XAmzContentSHA256Mismatch:               {Code: XAmzContentSHA256Mismatch, HTTPStatus: http.StatusBadRequest, Message: "The provided 'x-amz-content-sha256' header does not match what was computed."},
}

// Codes returns every registered code in sorted order.
func Codes() []Code {
	out := make([]Code, 0, len(registry))
	for c := range registry {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// IsRegistered reports whether code has a registry entry.
func IsRegistered(code Code) bool {
	_, ok := registry[code]
	return ok
}
