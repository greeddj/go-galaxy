package s3

import (
	"errors"
	"time"
)

var (
	errS3BucketIsEmpty          = errors.New("s3 bucket is empty")
	errS3HttpClientIsNil        = errors.New("s3 http client is nil")
	errS3LockAlreadyIsExists    = errors.New("s3 lock is already exists")
	errS3LockTTLIsInvalid       = errors.New("s3 lock TTL is invalid")
	errS3LockHeaderIsMissing    = errors.New("s3 lock header is missing")
	errS3LockTimestampIsMissing = errors.New("s3 lock timestamp is missing")
	errS3NotFound               = errors.New("s3 object not found")
	errS3BucketNotFound         = errors.New("s3 bucket not found")
	errS3BucketEmpty            = errors.New("s3 bucket is empty")
	errS3BucketHeadFailed       = errors.New("s3 bucket head is failed")
	errS3CreateBucketFailed     = errors.New("s3 create bucket failed")
	errS3BucketRequestFailed    = errors.New("s3 bucket request failed")
	errS3PreconditionFailed     = errors.New("s3 precondition failed")
	errS3HTTPClientNil          = errors.New("s3 http client is nil")
	errS3InvalidEndpoint        = errors.New("s3 invalid endpoint")
	errS3GetFailed              = errors.New("s3 get object failed")
	errS3HeadFailed             = errors.New("s3 head object failed")
	errS3PutFailed              = errors.New("s3 put object failed")
	errS3DeleteFailed           = errors.New("s3 delete object failed")
	errS3ClientNil              = errors.New("s3 client is nil")
	errArtifactSHA256Mismatch   = errors.New("s3 artifact sha256 mismatch")
)

const (
	emptySHA256     = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	statePrefix     = "state"
	artifactsPrefix = "artifacts"
	locksPrefix     = "locks"
	storeObject     = "store.json.gz"
	projectsObject  = "projects.json"
	lockObject      = "cache.lock"
	lockTTL         = 10 * time.Minute
	peekBytes       = 2
	headerLength    = 2
)
