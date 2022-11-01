// Copyright (c) 2015-2021 MinIO, Inc.
//
// This file is part of MinIO Object Storage stack
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package cmd

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/klauspost/compress/gzhttp"
	miniogo "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/encrypt"
	"github.com/minio/minio-go/v7/pkg/tags"
	sse "github.com/minio/minio/internal/bucket/encryption"
	"github.com/minio/minio/internal/bucket/lifecycle"
	objectlock "github.com/minio/minio/internal/bucket/object/lock"
	"github.com/minio/minio/internal/bucket/replication"
	"github.com/minio/minio/internal/config/dns"
	"github.com/minio/minio/internal/config/storageclass"
	"github.com/minio/minio/internal/crypto"
	"github.com/minio/minio/internal/etag"
	"github.com/minio/minio/internal/event"
	"github.com/minio/minio/internal/fips"
	"github.com/minio/minio/internal/handlers"
	"github.com/minio/minio/internal/hash"
	xhttp "github.com/minio/minio/internal/http"
	xioutil "github.com/minio/minio/internal/ioutil"
	"github.com/minio/minio/internal/kms"
	"github.com/minio/minio/internal/logger"
	"github.com/minio/minio/internal/s3select"
	"github.com/minio/pkg/bucket/policy"
	iampolicy "github.com/minio/pkg/iam/policy"
	xnet "github.com/minio/pkg/net"
	"github.com/minio/sio"
)

// supportedHeadGetReqParams - supported request parameters for GET and HEAD presigned request.
var supportedHeadGetReqParams = map[string]string{
	"response-expires":             xhttp.Expires,
	"response-content-type":        xhttp.ContentType,
	"response-cache-control":       xhttp.CacheControl,
	"response-content-encoding":    xhttp.ContentEncoding,
	"response-content-language":    xhttp.ContentLanguage,
	"response-content-disposition": xhttp.ContentDisposition,
}

const (
	compressionAlgorithmV1 = "golang/snappy/LZ77"
	compressionAlgorithmV2 = "klauspost/compress/s2"

	// When an upload exceeds encryptBufferThreshold ...
	encryptBufferThreshold = 1 << 20
	// add an input buffer of this size.
	encryptBufferSize = 1 << 20
)

// setHeadGetRespHeaders - set any requested parameters as response headers.
func setHeadGetRespHeaders(w http.ResponseWriter, reqParams url.Values) {
	for k, v := range reqParams {
		if header, ok := supportedHeadGetReqParams[strings.ToLower(k)]; ok {
			w.Header()[header] = v
		}
	}
}

// SelectObjectContentHandler - GET Object?select
// ----------
// This implementation of the GET operation retrieves object content based
// on an SQL expression. In the request, along with the sql expression, you must
// also specify a data serialization format (JSON, CSV) of the object.
func (api objectAPIHandlers) SelectObjectContentHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "SelectObject")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	// Fetch object stat info.
	objectAPI := api.ObjectAPI()
	if objectAPI == nil {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrServerNotInitialized), r.URL)
		return
	}

	if crypto.S3.IsRequested(r.Header) || crypto.S3KMS.IsRequested(r.Header) { // If SSE-S3 or SSE-KMS present -> AWS fails with undefined error
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrBadRequest), r.URL)
		return
	}

	if _, ok := crypto.IsRequested(r.Header); ok && !objectAPI.IsEncryptionSupported() {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrBadRequest), r.URL)
		return
	}

	vars := mux.Vars(r)
	bucket := vars["bucket"]
	object, err := unescapePath(vars["object"])
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	// get gateway encryption options
	opts, err := getOpts(ctx, r, bucket, object)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	getObjectInfo := objectAPI.GetObjectInfo
	if api.CacheAPI() != nil {
		getObjectInfo = api.CacheAPI().GetObjectInfo
	}

	// Check for auth type to return S3 compatible error.
	// type to return the correct error (NoSuchKey vs AccessDenied)
	if s3Error := checkRequestAuthType(ctx, r, policy.GetObjectAction, bucket, object); s3Error != ErrNone {
		if getRequestAuthType(r) == authTypeAnonymous {
			// As per "Permission" section in
			// https://docs.aws.amazon.com/AmazonS3/latest/API/RESTObjectGET.html
			// If the object you request does not exist,
			// the error Amazon S3 returns depends on
			// whether you also have the s3:ListBucket
			// permission.
			// * If you have the s3:ListBucket permission
			//   on the bucket, Amazon S3 will return an
			//   HTTP status code 404 ("no such key")
			//   error.
			// * if you don’t have the s3:ListBucket
			//   permission, Amazon S3 will return an HTTP
			//   status code 403 ("access denied") error.`
			if globalPolicySys.IsAllowed(policy.Args{
				Action:          policy.ListBucketAction,
				BucketName:      bucket,
				ConditionValues: getConditionValues(r, "", "", nil),
				IsOwner:         false,
			}) {
				_, err = getObjectInfo(ctx, bucket, object, opts)
				if toAPIError(ctx, err).Code == "NoSuchKey" {
					s3Error = ErrNoSuchKey
				}
			}
		}
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Error), r.URL)
		return
	}

	// Get request range.
	rangeHeader := r.Header.Get(xhttp.Range)
	if rangeHeader != "" {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrUnsupportedRangeHeader), r.URL)
		return
	}

	if r.ContentLength <= 0 {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrEmptyRequestBody), r.URL)
		return
	}

	getObjectNInfo := objectAPI.GetObjectNInfo
	if api.CacheAPI() != nil {
		getObjectNInfo = api.CacheAPI().GetObjectNInfo
	} else {
		// Take read lock on object, here so subsequent lower-level
		// calls do not need to.
		lock := objectAPI.NewNSLock(bucket, object)
		lkctx, err := lock.GetRLock(ctx, globalOperationTimeout)
		if err != nil {
			writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
			return
		}
		ctx = lkctx.Context()
		defer lock.RUnlock(lkctx.Cancel)
	}

	objInfo, err := getObjectInfo(ctx, bucket, object, opts)
	if err != nil {
		if globalBucketVersioningSys.Enabled(bucket) {
			// Versioning enabled quite possibly object is deleted might be delete-marker
			// if present set the headers, no idea why AWS S3 sets these headers.
			if objInfo.VersionID != "" && objInfo.DeleteMarker {
				w.Header()[xhttp.AmzVersionID] = []string{objInfo.VersionID}
				w.Header()[xhttp.AmzDeleteMarker] = []string{strconv.FormatBool(objInfo.DeleteMarker)}
			}
		}
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	// filter object lock metadata if permission does not permit
	getRetPerms := checkRequestAuthType(ctx, r, policy.GetObjectRetentionAction, bucket, object)
	legalHoldPerms := checkRequestAuthType(ctx, r, policy.GetObjectLegalHoldAction, bucket, object)

	// filter object lock metadata if permission does not permit
	objInfo.UserDefined = objectlock.FilterObjectLockMetadata(objInfo.UserDefined, getRetPerms != ErrNone, legalHoldPerms != ErrNone)

	if objectAPI.IsEncryptionSupported() {
		if _, err = DecryptObjectInfo(&objInfo, r); err != nil {
			writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
			return
		}
	}

	actualSize, err := objInfo.GetActualSize()
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	objectRSC := s3select.NewObjectReadSeekCloser(
		func(offset int64) (io.ReadCloser, error) {
			rs := &HTTPRangeSpec{
				IsSuffixLength: false,
				Start:          offset,
				End:            -1,
			}
			return getObjectNInfo(ctx, bucket, object, rs, r.Header, noLock, opts)
		},
		actualSize,
	)

	s3Select, err := s3select.NewS3Select(r.Body)
	if err != nil {
		if serr, ok := err.(s3select.SelectError); ok {
			encodedErrorResponse := encodeResponse(APIErrorResponse{
				Code:       serr.ErrorCode(),
				Message:    serr.ErrorMessage(),
				BucketName: bucket,
				Key:        object,
				Resource:   r.URL.Path,
				RequestID:  w.Header().Get(xhttp.AmzRequestID),
				HostID:     globalDeploymentID,
			})
			writeResponse(w, serr.HTTPStatusCode(), encodedErrorResponse, mimeXML)
		} else {
			writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		}
		return
	}
	defer s3Select.Close()

	if err = s3Select.Open(objectRSC); err != nil {
		if serr, ok := err.(s3select.SelectError); ok {
			encodedErrorResponse := encodeResponse(APIErrorResponse{
				Code:       serr.ErrorCode(),
				Message:    serr.ErrorMessage(),
				BucketName: bucket,
				Key:        object,
				Resource:   r.URL.Path,
				RequestID:  w.Header().Get(xhttp.AmzRequestID),
				HostID:     globalDeploymentID,
			})
			writeResponse(w, serr.HTTPStatusCode(), encodedErrorResponse, mimeXML)
		} else {
			writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		}
		return
	}

	// Set encryption response headers
	if objectAPI.IsEncryptionSupported() {
		switch kind, _ := crypto.IsEncrypted(objInfo.UserDefined); kind {
		case crypto.S3:
			w.Header().Set(xhttp.AmzServerSideEncryption, xhttp.AmzEncryptionAES)
		case crypto.S3KMS:
			w.Header().Set(xhttp.AmzServerSideEncryption, xhttp.AmzEncryptionKMS)
			w.Header().Set(xhttp.AmzServerSideEncryptionKmsID, objInfo.KMSKeyID())
			if kmsCtx, ok := objInfo.UserDefined[crypto.MetaContext]; ok {
				w.Header().Set(xhttp.AmzServerSideEncryptionKmsContext, kmsCtx)
			}
		case crypto.SSEC:
			// Validate the SSE-C Key set in the header.
			if _, err = crypto.SSEC.UnsealObjectKey(r.Header, objInfo.UserDefined, bucket, object); err != nil {
				writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
				return
			}
			w.Header().Set(xhttp.AmzServerSideEncryptionCustomerAlgorithm, r.Header.Get(xhttp.AmzServerSideEncryptionCustomerAlgorithm))
			w.Header().Set(xhttp.AmzServerSideEncryptionCustomerKeyMD5, r.Header.Get(xhttp.AmzServerSideEncryptionCustomerKeyMD5))
		}
	}

	s3Select.Evaluate(w)

	// Notify object accessed via a GET request.
	sendEvent(eventArgs{
		EventName:    event.ObjectAccessedGet,
		BucketName:   bucket,
		Object:       objInfo,
		ReqParams:    extractReqParams(r),
		RespElements: extractRespElements(w),
		UserAgent:    r.UserAgent(),
		Host:         handlers.GetSourceIP(r),
	})
}

func (api objectAPIHandlers) getObjectHandler(ctx context.Context, objectAPI ObjectLayer, bucket, object string, w http.ResponseWriter, r *http.Request) {
	if crypto.S3.IsRequested(r.Header) || crypto.S3KMS.IsRequested(r.Header) { // If SSE-S3 or SSE-KMS present -> AWS fails with undefined error
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrBadRequest), r.URL)
		return
	}
	if _, ok := crypto.IsRequested(r.Header); !objectAPI.IsEncryptionSupported() && ok {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrBadRequest), r.URL)
		return
	}

	// get gateway encryption options
	opts, err := getOpts(ctx, r, bucket, object)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	// Check for auth type to return S3 compatible error.
	// type to return the correct error (NoSuchKey vs AccessDenied)
	if s3Error := checkRequestAuthType(ctx, r, policy.GetObjectAction, bucket, object); s3Error != ErrNone {
		if getRequestAuthType(r) == authTypeAnonymous {
			// As per "Permission" section in
			// https://docs.aws.amazon.com/AmazonS3/latest/API/RESTObjectGET.html
			// If the object you request does not exist,
			// the error Amazon S3 returns depends on
			// whether you also have the s3:ListBucket
			// permission.
			// * If you have the s3:ListBucket permission
			//   on the bucket, Amazon S3 will return an
			//   HTTP status code 404 ("no such key")
			//   error.
			// * if you don’t have the s3:ListBucket
			//   permission, Amazon S3 will return an HTTP
			//   status code 403 ("access denied") error.`
			if globalPolicySys.IsAllowed(policy.Args{
				Action:          policy.ListBucketAction,
				BucketName:      bucket,
				ConditionValues: getConditionValues(r, "", "", nil),
				IsOwner:         false,
			}) {
				getObjectInfo := objectAPI.GetObjectInfo
				if api.CacheAPI() != nil {
					getObjectInfo = api.CacheAPI().GetObjectInfo
				}

				_, err = getObjectInfo(ctx, bucket, object, opts)
				if toAPIError(ctx, err).Code == "NoSuchKey" {
					s3Error = ErrNoSuchKey
				}
			}
		}
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Error), r.URL)
		return
	}

	getObjectNInfo := objectAPI.GetObjectNInfo
	if api.CacheAPI() != nil {
		getObjectNInfo = api.CacheAPI().GetObjectNInfo
	}

	// Get request range.
	var rs *HTTPRangeSpec
	var rangeErr error
	rangeHeader := r.Header.Get(xhttp.Range)
	if rangeHeader != "" {
		// Both 'Range' and 'partNumber' cannot be specified at the same time
		if opts.PartNumber > 0 {
			writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrInvalidRangePartNumber), r.URL)
			return
		}

		rs, rangeErr = parseRequestRangeSpec(rangeHeader)
		// Handle only errInvalidRange. Ignore other
		// parse error and treat it as regular Get
		// request like Amazon S3.
		if rangeErr == errInvalidRange {
			writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrInvalidRange), r.URL)
			return
		}
		if rangeErr != nil {
			logger.LogIf(ctx, rangeErr, logger.Application)
		}
	}

	// Validate pre-conditions if any.
	opts.CheckPrecondFn = func(oi ObjectInfo) bool {
		if objectAPI.IsEncryptionSupported() {
			if _, err := DecryptObjectInfo(&oi, r); err != nil {
				writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
				return true
			}
		}

		return checkPreconditions(ctx, w, r, oi, opts)
	}

	gr, err := getObjectNInfo(ctx, bucket, object, rs, r.Header, readLock, opts)
	if err != nil {
		var (
			reader *GetObjectReader
			proxy  proxyResult
			perr   error
		)
		proxytgts := getproxyTargets(ctx, bucket, object, opts)
		if !proxytgts.Empty() {
			// proxy to replication target if active-active replication is in place.
			reader, proxy, perr = proxyGetToReplicationTarget(ctx, bucket, object, rs, r.Header, opts, proxytgts)
			if perr != nil && !isErrObjectNotFound(ErrorRespToObjectError(perr, bucket, object)) &&
				!isErrVersionNotFound(ErrorRespToObjectError(perr, bucket, object)) {
				logger.LogIf(ctx, fmt.Errorf("Replication proxy failed for %s/%s(%s) - %w", bucket, object, opts.VersionID, perr))
			}
			if reader != nil && proxy.Proxy && perr == nil {
				gr = reader
			}
		}
		if reader == nil || !proxy.Proxy {
			if isErrPreconditionFailed(err) {
				return
			}
			if proxy.Err != nil {
				writeErrorResponse(ctx, w, toAPIError(ctx, proxy.Err), r.URL)
				return
			}
			if globalBucketVersioningSys.Enabled(bucket) && gr != nil {
				if !gr.ObjInfo.VersionPurgeStatus.Empty() {
					// Shows the replication status of a permanent delete of a version
					w.Header()[xhttp.MinIODeleteReplicationStatus] = []string{string(gr.ObjInfo.VersionPurgeStatus)}
				}
				if !gr.ObjInfo.ReplicationStatus.Empty() && gr.ObjInfo.DeleteMarker {
					w.Header()[xhttp.MinIODeleteMarkerReplicationStatus] = []string{string(gr.ObjInfo.ReplicationStatus)}
				}

				// Versioning enabled quite possibly object is deleted might be delete-marker
				// if present set the headers, no idea why AWS S3 sets these headers.
				if gr.ObjInfo.VersionID != "" && gr.ObjInfo.DeleteMarker {
					w.Header()[xhttp.AmzVersionID] = []string{gr.ObjInfo.VersionID}
					w.Header()[xhttp.AmzDeleteMarker] = []string{strconv.FormatBool(gr.ObjInfo.DeleteMarker)}
				}
			}
			writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
			return
		}
	}
	defer gr.Close()

	objInfo := gr.ObjInfo

	// Automatically remove the object/version is an expiry lifecycle rule can be applied
	if lc, err := globalLifecycleSys.Get(bucket); err == nil {
		rcfg, _ := globalBucketObjectLockSys.Get(bucket)
		action := evalActionFromLifecycle(ctx, *lc, rcfg, objInfo, false)
		var success bool
		switch action {
		case lifecycle.DeleteVersionAction, lifecycle.DeleteAction:
			success = applyExpiryRule(objInfo, false, action == lifecycle.DeleteVersionAction)
		case lifecycle.DeleteRestoredAction, lifecycle.DeleteRestoredVersionAction:
			// Restored object delete would be still allowed to proceed as success
			// since transition behavior is slightly different.
			applyExpiryRule(objInfo, true, action == lifecycle.DeleteRestoredVersionAction)
		}
		if success {
			writeErrorResponseHeadersOnly(w, errorCodes.ToAPIErr(ErrNoSuchKey))
			return
		}
	}

	// filter object lock metadata if permission does not permit
	getRetPerms := checkRequestAuthType(ctx, r, policy.GetObjectRetentionAction, bucket, object)
	legalHoldPerms := checkRequestAuthType(ctx, r, policy.GetObjectLegalHoldAction, bucket, object)

	// filter object lock metadata if permission does not permit
	objInfo.UserDefined = objectlock.FilterObjectLockMetadata(objInfo.UserDefined, getRetPerms != ErrNone, legalHoldPerms != ErrNone)

	// Set encryption response headers
	if objectAPI.IsEncryptionSupported() {
		switch kind, _ := crypto.IsEncrypted(objInfo.UserDefined); kind {
		case crypto.S3:
			w.Header().Set(xhttp.AmzServerSideEncryption, xhttp.AmzEncryptionAES)
		case crypto.S3KMS:
			w.Header().Set(xhttp.AmzServerSideEncryption, xhttp.AmzEncryptionKMS)
			w.Header().Set(xhttp.AmzServerSideEncryptionKmsID, objInfo.KMSKeyID())
			if kmsCtx, ok := objInfo.UserDefined[crypto.MetaContext]; ok {
				w.Header().Set(xhttp.AmzServerSideEncryptionKmsContext, kmsCtx)
			}
		case crypto.SSEC:
			w.Header().Set(xhttp.AmzServerSideEncryptionCustomerAlgorithm, r.Header.Get(xhttp.AmzServerSideEncryptionCustomerAlgorithm))
			w.Header().Set(xhttp.AmzServerSideEncryptionCustomerKeyMD5, r.Header.Get(xhttp.AmzServerSideEncryptionCustomerKeyMD5))
		}
	}

	if err = setObjectHeaders(w, objInfo, rs, opts); err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	// Set Parts Count Header
	if opts.PartNumber > 0 && len(objInfo.Parts) > 0 {
		setPartsCountHeaders(w, objInfo)
	}

	setHeadGetRespHeaders(w, r.Form)

	statusCodeWritten := false
	httpWriter := xioutil.WriteOnClose(w)
	if rs != nil || opts.PartNumber > 0 {
		statusCodeWritten = true
		w.WriteHeader(http.StatusPartialContent)
	}

	// Write object content to response body
	if _, err = xioutil.Copy(httpWriter, gr); err != nil {
		if !httpWriter.HasWritten() && !statusCodeWritten {
			// write error response only if no data or headers has been written to client yet
			writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
			return
		}
		if !xnet.IsNetworkOrHostDown(err, true) { // do not need to log disconnected clients
			logger.LogIf(ctx, fmt.Errorf("Unable to write all the data to client %w", err))
		}
		return
	}

	if err = httpWriter.Close(); err != nil {
		if !httpWriter.HasWritten() && !statusCodeWritten { // write error response only if no data or headers has been written to client yet
			writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
			return
		}
		if !xnet.IsNetworkOrHostDown(err, true) { // do not need to log disconnected clients
			logger.LogIf(ctx, fmt.Errorf("Unable to write all the data to client %w", err))
		}
		return
	}

	// Notify object accessed via a GET request.
	sendEvent(eventArgs{
		EventName:    event.ObjectAccessedGet,
		BucketName:   bucket,
		Object:       objInfo,
		ReqParams:    extractReqParams(r),
		RespElements: extractRespElements(w),
		UserAgent:    r.UserAgent(),
		Host:         handlers.GetSourceIP(r),
	})
}

// GetObjectHandler - GET Object
// ----------
// This implementation of the GET operation retrieves object. To use GET,
// you must have READ access to the object.
func (api objectAPIHandlers) GetObjectHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "GetObject")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	objectAPI := api.ObjectAPI()
	if objectAPI == nil {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrServerNotInitialized), r.URL)
		return
	}

	vars := mux.Vars(r)
	bucket := vars["bucket"]
	object, err := unescapePath(vars["object"])
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}
	if !globalAPIConfig.shouldGzipObjects() {
		w.Header().Set(gzhttp.HeaderNoCompression, "true")
	}

	if r.Header.Get(xMinIOExtract) == "true" && strings.Contains(object, archivePattern) {
		api.getObjectInArchiveFileHandler(ctx, objectAPI, bucket, object, w, r)
	} else {
		api.getObjectHandler(ctx, objectAPI, bucket, object, w, r)
	}
}

func (api objectAPIHandlers) headObjectHandler(ctx context.Context, objectAPI ObjectLayer, bucket, object string, w http.ResponseWriter, r *http.Request) {
	if crypto.S3.IsRequested(r.Header) || crypto.S3KMS.IsRequested(r.Header) { // If SSE-S3 or SSE-KMS present -> AWS fails with undefined error
		writeErrorResponseHeadersOnly(w, errorCodes.ToAPIErr(ErrBadRequest))
		return
	}
	if _, ok := crypto.IsRequested(r.Header); !objectAPI.IsEncryptionSupported() && ok {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrBadRequest), r.URL)
		return
	}

	getObjectInfo := objectAPI.GetObjectInfo
	if api.CacheAPI() != nil {
		getObjectInfo = api.CacheAPI().GetObjectInfo
	}

	opts, err := getOpts(ctx, r, bucket, object)
	if err != nil {
		writeErrorResponseHeadersOnly(w, toAPIError(ctx, err))
		return
	}

	if s3Error := checkRequestAuthType(ctx, r, policy.GetObjectAction, bucket, object); s3Error != ErrNone {
		if getRequestAuthType(r) == authTypeAnonymous {
			// As per "Permission" section in
			// https://docs.aws.amazon.com/AmazonS3/latest/API/RESTObjectHEAD.html
			// If the object you request does not exist,
			// the error Amazon S3 returns depends on
			// whether you also have the s3:ListBucket
			// permission.
			// * If you have the s3:ListBucket permission
			//   on the bucket, Amazon S3 will return an
			//   HTTP status code 404 ("no such key")
			//   error.
			// * if you don’t have the s3:ListBucket
			//   permission, Amazon S3 will return an HTTP
			//   status code 403 ("access denied") error.`
			if globalPolicySys.IsAllowed(policy.Args{
				Action:          policy.ListBucketAction,
				BucketName:      bucket,
				ConditionValues: getConditionValues(r, "", "", nil),
				IsOwner:         false,
			}) {
				_, err = getObjectInfo(ctx, bucket, object, opts)
				if toAPIError(ctx, err).Code == "NoSuchKey" {
					s3Error = ErrNoSuchKey
				}
			}
		}
		writeErrorResponseHeadersOnly(w, errorCodes.ToAPIErr(s3Error))
		return
	}
	// Get request range.
	var rs *HTTPRangeSpec
	rangeHeader := r.Header.Get(xhttp.Range)

	objInfo, err := getObjectInfo(ctx, bucket, object, opts)
	if err != nil {
		var (
			proxy proxyResult
			oi    ObjectInfo
		)
		// proxy HEAD to replication target if active-active replication configured on bucket
		proxytgts := getproxyTargets(ctx, bucket, object, opts)
		if !proxytgts.Empty() {
			if rangeHeader != "" {
				rs, _ = parseRequestRangeSpec(rangeHeader)
			}
			oi, proxy = proxyHeadToReplicationTarget(ctx, bucket, object, rs, opts, proxytgts)
			if proxy.Proxy {
				objInfo = oi
			}
		}
		if !proxy.Proxy {
			if globalBucketVersioningSys.Enabled(bucket) {
				switch {
				case !objInfo.VersionPurgeStatus.Empty():
					w.Header()[xhttp.MinIODeleteReplicationStatus] = []string{string(objInfo.VersionPurgeStatus)}
				case !objInfo.ReplicationStatus.Empty() && objInfo.DeleteMarker:
					w.Header()[xhttp.MinIODeleteMarkerReplicationStatus] = []string{string(objInfo.ReplicationStatus)}
				}
				// Versioning enabled quite possibly object is deleted might be delete-marker
				// if present set the headers, no idea why AWS S3 sets these headers.
				if objInfo.VersionID != "" && objInfo.DeleteMarker {
					w.Header()[xhttp.AmzVersionID] = []string{objInfo.VersionID}
					w.Header()[xhttp.AmzDeleteMarker] = []string{strconv.FormatBool(objInfo.DeleteMarker)}
				}
			}
			writeErrorResponseHeadersOnly(w, toAPIError(ctx, err))
			return
		}
	}

	// Automatically remove the object/version is an expiry lifecycle rule can be applied
	if lc, err := globalLifecycleSys.Get(bucket); err == nil {
		rcfg, _ := globalBucketObjectLockSys.Get(bucket)
		action := evalActionFromLifecycle(ctx, *lc, rcfg, objInfo, false)
		var success bool
		switch action {
		case lifecycle.DeleteVersionAction, lifecycle.DeleteAction:
			success = applyExpiryRule(objInfo, false, action == lifecycle.DeleteVersionAction)
		case lifecycle.DeleteRestoredAction, lifecycle.DeleteRestoredVersionAction:
			// Restored object delete would be still allowed to proceed as success
			// since transition behavior is slightly different.
			applyExpiryRule(objInfo, true, action == lifecycle.DeleteRestoredVersionAction)
		}
		if success {
			writeErrorResponseHeadersOnly(w, errorCodes.ToAPIErr(ErrNoSuchKey))
			return
		}
	}

	// filter object lock metadata if permission does not permit
	getRetPerms := checkRequestAuthType(ctx, r, policy.GetObjectRetentionAction, bucket, object)
	legalHoldPerms := checkRequestAuthType(ctx, r, policy.GetObjectLegalHoldAction, bucket, object)

	// filter object lock metadata if permission does not permit
	objInfo.UserDefined = objectlock.FilterObjectLockMetadata(objInfo.UserDefined, getRetPerms != ErrNone, legalHoldPerms != ErrNone)

	if objectAPI.IsEncryptionSupported() {
		if _, err = DecryptObjectInfo(&objInfo, r); err != nil {
			writeErrorResponseHeadersOnly(w, toAPIError(ctx, err))
			return
		}
	}

	// Validate pre-conditions if any.
	if checkPreconditions(ctx, w, r, objInfo, opts) {
		return
	}

	if rangeHeader != "" {
		// Both 'Range' and 'partNumber' cannot be specified at the same time
		if opts.PartNumber > 0 {
			writeErrorResponseHeadersOnly(w, errorCodes.ToAPIErr(ErrInvalidRangePartNumber))
			return
		}

		if rs, err = parseRequestRangeSpec(rangeHeader); err != nil {
			// Handle only errInvalidRange. Ignore other
			// parse error and treat it as regular Get
			// request like Amazon S3.
			if err == errInvalidRange {
				writeErrorResponseHeadersOnly(w, errorCodes.ToAPIErr(ErrInvalidRange))
				return
			}

			logger.LogIf(ctx, err)
		}
	}

	// Set encryption response headers
	if objectAPI.IsEncryptionSupported() {
		switch kind, _ := crypto.IsEncrypted(objInfo.UserDefined); kind {
		case crypto.S3:
			w.Header().Set(xhttp.AmzServerSideEncryption, xhttp.AmzEncryptionAES)
		case crypto.S3KMS:
			w.Header().Set(xhttp.AmzServerSideEncryption, xhttp.AmzEncryptionKMS)
			w.Header().Set(xhttp.AmzServerSideEncryptionKmsID, objInfo.KMSKeyID())
			if kmsCtx, ok := objInfo.UserDefined[crypto.MetaContext]; ok {
				w.Header().Set(xhttp.AmzServerSideEncryptionKmsContext, kmsCtx)
			}
		case crypto.SSEC:
			// Validate the SSE-C Key set in the header.
			if _, err = crypto.SSEC.UnsealObjectKey(r.Header, objInfo.UserDefined, bucket, object); err != nil {
				writeErrorResponseHeadersOnly(w, toAPIError(ctx, err))
				return
			}
			w.Header().Set(xhttp.AmzServerSideEncryptionCustomerAlgorithm, r.Header.Get(xhttp.AmzServerSideEncryptionCustomerAlgorithm))
			w.Header().Set(xhttp.AmzServerSideEncryptionCustomerKeyMD5, r.Header.Get(xhttp.AmzServerSideEncryptionCustomerKeyMD5))
		}
	}

	// Set standard object headers.
	if err = setObjectHeaders(w, objInfo, rs, opts); err != nil {
		writeErrorResponseHeadersOnly(w, toAPIError(ctx, err))
		return
	}

	// Set Parts Count Header
	if opts.PartNumber > 0 && len(objInfo.Parts) > 0 {
		setPartsCountHeaders(w, objInfo)
	}

	// Set any additional requested response headers.
	setHeadGetRespHeaders(w, r.Form)

	// Successful response.
	if rs != nil || opts.PartNumber > 0 {
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}

	// Notify object accessed via a HEAD request.
	sendEvent(eventArgs{
		EventName:    event.ObjectAccessedHead,
		BucketName:   bucket,
		Object:       objInfo,
		ReqParams:    extractReqParams(r),
		RespElements: extractRespElements(w),
		UserAgent:    r.UserAgent(),
		Host:         handlers.GetSourceIP(r),
	})
}

// HeadObjectHandler - HEAD Object
// -----------
// The HEAD operation retrieves metadata from an object without returning the object itself.
func (api objectAPIHandlers) HeadObjectHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "HeadObject")
	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	objectAPI := api.ObjectAPI()
	if objectAPI == nil {
		writeErrorResponseHeadersOnly(w, errorCodes.ToAPIErr(ErrServerNotInitialized))
		return
	}

	vars := mux.Vars(r)
	bucket := vars["bucket"]
	object, err := unescapePath(vars["object"])
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	if r.Header.Get(xMinIOExtract) == "true" && strings.Contains(object, archivePattern) {
		api.headObjectInArchiveFileHandler(ctx, objectAPI, bucket, object, w, r)
	} else {
		api.headObjectHandler(ctx, objectAPI, bucket, object, w, r)
	}
}

// Extract metadata relevant for an CopyObject operation based on conditional
// header values specified in X-Amz-Metadata-Directive.
func getCpObjMetadataFromHeader(ctx context.Context, r *http.Request, userMeta map[string]string) (map[string]string, error) {
	// Make a copy of the supplied metadata to avoid
	// to change the original one.
	defaultMeta := make(map[string]string, len(userMeta))
	for k, v := range userMeta {
		defaultMeta[k] = v
	}

	// remove SSE Headers from source info
	crypto.RemoveSSEHeaders(defaultMeta)

	// Storage class is special, it can be replaced regardless of the
	// metadata directive, if set should be preserved and replaced
	// to the destination metadata.
	sc := r.Header.Get(xhttp.AmzStorageClass)
	if sc == "" {
		sc = r.Form.Get(xhttp.AmzStorageClass)
	}

	// if x-amz-metadata-directive says REPLACE then
	// we extract metadata from the input headers.
	if isDirectiveReplace(r.Header.Get(xhttp.AmzMetadataDirective)) {
		emetadata, err := extractMetadata(ctx, r)
		if err != nil {
			return nil, err
		}
		if sc != "" {
			emetadata[xhttp.AmzStorageClass] = sc
		}
		return emetadata, nil
	}

	if sc != "" {
		defaultMeta[xhttp.AmzStorageClass] = sc
	}

	// if x-amz-metadata-directive says COPY then we
	// return the default metadata.
	if isDirectiveCopy(r.Header.Get(xhttp.AmzMetadataDirective)) {
		return defaultMeta, nil
	}

	// Copy is default behavior if not x-amz-metadata-directive is set.
	return defaultMeta, nil
}

// getRemoteInstanceTransport contains a singleton roundtripper.
var (
	getRemoteInstanceTransport     *http.Transport
	getRemoteInstanceTransportOnce sync.Once
)

// Returns a minio-go Client configured to access remote host described by destDNSRecord
// Applicable only in a federated deployment
var getRemoteInstanceClient = func(r *http.Request, host string) (*miniogo.Core, error) {
	cred := getReqAccessCred(r, globalSite.Region)
	// In a federated deployment, all the instances share config files
	// and hence expected to have same credentials.
	return miniogo.NewCore(host, &miniogo.Options{
		Creds:     credentials.NewStaticV4(cred.AccessKey, cred.SecretKey, ""),
		Secure:    globalIsTLS,
		Transport: getRemoteInstanceTransport,
	})
}

// Check if the destination bucket is on a remote site, this code only gets executed
// when federation is enabled, ie when globalDNSConfig is non 'nil'.
//
// This function is similar to isRemoteCallRequired but specifically for COPY object API
// if destination and source are same we do not need to check for destination bucket
// to exist locally.
func isRemoteCopyRequired(ctx context.Context, srcBucket, dstBucket string, objAPI ObjectLayer) bool {
	if srcBucket == dstBucket {
		return false
	}
	return isRemoteCallRequired(ctx, dstBucket, objAPI)
}

// Check if the bucket is on a remote site, this code only gets executed when federation is enabled.
func isRemoteCallRequired(ctx context.Context, bucket string, objAPI ObjectLayer) bool {
	if globalDNSConfig == nil {
		return false
	}
	if globalBucketFederation {
		_, err := objAPI.GetBucketInfo(ctx, bucket)
		return err == toObjectErr(errVolumeNotFound, bucket)
	}
	return false
}

// CopyObjectHandler - Copy Object
// ----------
// This implementation of the PUT operation adds an object to a bucket
// while reading the object from another source.
// Notice: The S3 client can send secret keys in headers for encryption related jobs,
// the handler should ensure to remove these keys before sending them to the object layer.
// Currently these keys are:
//   - X-Amz-Server-Side-Encryption-Customer-Key
//   - X-Amz-Copy-Source-Server-Side-Encryption-Customer-Key
func (api objectAPIHandlers) CopyObjectHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "CopyObject")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	objectAPI := api.ObjectAPI()
	if objectAPI == nil {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrServerNotInitialized), r.URL)
		return
	}

	if _, ok := crypto.IsRequested(r.Header); ok {
		if globalIsGateway {
			if crypto.SSEC.IsRequested(r.Header) && !objectAPI.IsEncryptionSupported() {
				writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrNotImplemented), r.URL)
				return
			}
		} else {
			if !objectAPI.IsEncryptionSupported() {
				writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrNotImplemented), r.URL)
				return
			}
		}
	}

	vars := mux.Vars(r)
	dstBucket := vars["bucket"]
	dstObject, err := unescapePath(vars["object"])
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	if s3Error := checkRequestAuthType(ctx, r, policy.PutObjectAction, dstBucket, dstObject); s3Error != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Error), r.URL)
		return
	}

	// Read escaped copy source path to check for parameters.
	cpSrcPath := r.Header.Get(xhttp.AmzCopySource)
	var vid string
	if u, err := url.Parse(cpSrcPath); err == nil {
		vid = strings.TrimSpace(u.Query().Get(xhttp.VersionID))
		// Note that url.Parse does the unescaping
		cpSrcPath = u.Path
	}

	srcBucket, srcObject := path2BucketObject(cpSrcPath)
	// If source object is empty or bucket is empty, reply back invalid copy source.
	if srcObject == "" || srcBucket == "" {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrInvalidCopySource), r.URL)
		return
	}

	if vid != "" && vid != nullVersionID {
		_, err := uuid.Parse(vid)
		if err != nil {
			writeErrorResponse(ctx, w, toAPIError(ctx, VersionNotFound{
				Bucket:    srcBucket,
				Object:    srcObject,
				VersionID: vid,
			}), r.URL)
			return
		}
	}

	if s3Error := checkRequestAuthType(ctx, r, policy.GetObjectAction, srcBucket, srcObject); s3Error != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Error), r.URL)
		return
	}

	// Check if metadata directive is valid.
	if !isDirectiveValid(r.Header.Get(xhttp.AmzMetadataDirective)) {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrInvalidMetadataDirective), r.URL)
		return
	}

	// check if tag directive is valid
	if !isDirectiveValid(r.Header.Get(xhttp.AmzTagDirective)) {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrInvalidTagDirective), r.URL)
		return
	}

	// Validate storage class metadata if present
	dstSc := r.Header.Get(xhttp.AmzStorageClass)
	if dstSc != "" && !storageclass.IsValid(dstSc) {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrInvalidStorageClass), r.URL)
		return
	}

	// Check if bucket encryption is enabled
	sseConfig, _ := globalBucketSSEConfigSys.Get(dstBucket)
	sseConfig.Apply(r.Header, sse.ApplyOptions{
		AutoEncrypt: globalAutoEncryption,
		Passthrough: globalIsGateway && globalGatewayName == S3BackendGateway,
	})

	var srcOpts, dstOpts ObjectOptions
	srcOpts, err = copySrcOpts(ctx, r, srcBucket, srcObject)
	if err != nil {
		logger.LogIf(ctx, err)
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}
	srcOpts.VersionID = vid

	// convert copy src encryption options for GET calls
	getOpts := ObjectOptions{VersionID: srcOpts.VersionID, Versioned: srcOpts.Versioned}
	getSSE := encrypt.SSE(srcOpts.ServerSideEncryption)
	if getSSE != srcOpts.ServerSideEncryption {
		getOpts.ServerSideEncryption = getSSE
	}

	dstOpts, err = copyDstOpts(ctx, r, dstBucket, dstObject, nil)
	if err != nil {
		logger.LogIf(ctx, err)
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}
	cpSrcDstSame := isStringEqual(pathJoin(srcBucket, srcObject), pathJoin(dstBucket, dstObject))

	getObjectNInfo := objectAPI.GetObjectNInfo
	if api.CacheAPI() != nil {
		getObjectNInfo = api.CacheAPI().GetObjectNInfo
	}

	checkCopyPrecondFn := func(o ObjectInfo) bool {
		if objectAPI.IsEncryptionSupported() {
			if _, err := DecryptObjectInfo(&o, r); err != nil {
				writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
				return true
			}
		}
		return checkCopyObjectPreconditions(ctx, w, r, o)
	}
	getOpts.CheckPrecondFn = checkCopyPrecondFn

	// FIXME: a possible race exists between a parallel
	// GetObject v/s CopyObject with metadata updates, ideally
	// we should be holding write lock here but it is not
	// possible due to other constraints such as knowing
	// the type of source content etc.
	lock := noLock
	if !cpSrcDstSame {
		lock = readLock
	}

	var rs *HTTPRangeSpec
	gr, err := getObjectNInfo(ctx, srcBucket, srcObject, rs, r.Header, lock, getOpts)
	if err != nil {
		if isErrPreconditionFailed(err) {
			return
		}
		if globalBucketVersioningSys.Enabled(srcBucket) && gr != nil {
			// Versioning enabled quite possibly object is deleted might be delete-marker
			// if present set the headers, no idea why AWS S3 sets these headers.
			if gr.ObjInfo.VersionID != "" && gr.ObjInfo.DeleteMarker {
				w.Header()[xhttp.AmzVersionID] = []string{gr.ObjInfo.VersionID}
				w.Header()[xhttp.AmzDeleteMarker] = []string{strconv.FormatBool(gr.ObjInfo.DeleteMarker)}
			}
		}
		// Update context bucket & object names for correct S3 XML error response
		reqInfo := logger.GetReqInfo(ctx)
		reqInfo.BucketName = srcBucket
		reqInfo.ObjectName = srcObject
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}
	defer gr.Close()
	srcInfo := gr.ObjInfo

	// maximum Upload size for object in a single CopyObject operation.
	if isMaxObjectSize(srcInfo.Size) {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrEntityTooLarge), r.URL)
		return
	}

	// We have to copy metadata only if source and destination are same.
	// this changes for encryption which can be observed below.
	if cpSrcDstSame {
		srcInfo.metadataOnly = true
	}

	var chStorageClass bool
	if dstSc != "" {
		chStorageClass = true
		srcInfo.metadataOnly = false
	}

	var reader io.Reader = gr

	// Set the actual size to the compressed/decrypted size if encrypted.
	actualSize, err := srcInfo.GetActualSize()
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}
	length := actualSize

	if !cpSrcDstSame {
		if err := enforceBucketQuotaHard(ctx, dstBucket, actualSize); err != nil {
			writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
			return
		}
	}

	// Check if either the source is encrypted or the destination will be encrypted.
	_, objectEncryption := crypto.IsRequested(r.Header)
	objectEncryption = objectEncryption || crypto.IsSourceEncrypted(srcInfo.UserDefined)

	var compressMetadata map[string]string
	// No need to compress for remote etcd calls
	// Pass the decompressed stream to such calls.
	isDstCompressed := objectAPI.IsCompressionSupported() &&
		isCompressible(r.Header, dstObject) &&
		!isRemoteCopyRequired(ctx, srcBucket, dstBucket, objectAPI) && !cpSrcDstSame && !objectEncryption
	if isDstCompressed {
		compressMetadata = make(map[string]string, 2)
		// Preserving the compression metadata.
		compressMetadata[ReservedMetadataPrefix+"compression"] = compressionAlgorithmV2
		compressMetadata[ReservedMetadataPrefix+"actual-size"] = strconv.FormatInt(actualSize, 10)

		reader = etag.NewReader(reader, nil)
		s2c := newS2CompressReader(reader, actualSize)
		defer s2c.Close()
		reader = etag.Wrap(s2c, reader)
		length = -1
	} else {
		delete(srcInfo.UserDefined, ReservedMetadataPrefix+"compression")
		delete(srcInfo.UserDefined, ReservedMetadataPrefix+"actual-size")
		reader = gr
	}

	srcInfo.Reader, err = hash.NewReader(reader, length, "", "", actualSize)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	pReader := NewPutObjReader(srcInfo.Reader)

	// Handle encryption
	encMetadata := make(map[string]string)
	if objectAPI.IsEncryptionSupported() {
		// Encryption parameters not applicable for this object.
		if _, ok := crypto.IsEncrypted(srcInfo.UserDefined); !ok && crypto.SSECopy.IsRequested(r.Header) {
			writeErrorResponse(ctx, w, toAPIError(ctx, errInvalidEncryptionParameters), r.URL)
			return
		}
		// Encryption parameters not present for this object.
		if crypto.SSEC.IsEncrypted(srcInfo.UserDefined) && !crypto.SSECopy.IsRequested(r.Header) {
			writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrInvalidSSECustomerAlgorithm), r.URL)
			return
		}

		var oldKey, newKey []byte
		var newKeyID string
		var kmsCtx kms.Context
		var objEncKey crypto.ObjectKey
		sseCopyKMS := crypto.S3KMS.IsEncrypted(srcInfo.UserDefined)
		sseCopyS3 := crypto.S3.IsEncrypted(srcInfo.UserDefined)
		sseCopyC := crypto.SSEC.IsEncrypted(srcInfo.UserDefined) && crypto.SSECopy.IsRequested(r.Header)
		sseC := crypto.SSEC.IsRequested(r.Header)
		sseS3 := crypto.S3.IsRequested(r.Header)
		sseKMS := crypto.S3KMS.IsRequested(r.Header)

		isSourceEncrypted := sseCopyC || sseCopyS3 || sseCopyKMS
		isTargetEncrypted := sseC || sseS3 || sseKMS

		if sseC {
			newKey, err = ParseSSECustomerRequest(r)
			if err != nil {
				writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
				return
			}
		}
		if crypto.S3KMS.IsRequested(r.Header) {
			newKeyID, kmsCtx, err = crypto.S3KMS.ParseHTTP(r.Header)
			if err != nil {
				writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
				return
			}
		}

		// If src == dst and either
		// - the object is encrypted using SSE-C and two different SSE-C keys are present
		// - the object is encrypted using SSE-S3 and the SSE-S3 header is present
		// - the object storage class is not changing
		// then execute a key rotation.
		if cpSrcDstSame && (sseCopyC && sseC) && !chStorageClass {
			oldKey, err = ParseSSECopyCustomerRequest(r.Header, srcInfo.UserDefined)
			if err != nil {
				writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
				return
			}

			for k, v := range srcInfo.UserDefined {
				if strings.HasPrefix(strings.ToLower(k), ReservedMetadataPrefixLower) {
					encMetadata[k] = v
				}
			}

			if err = rotateKey(oldKey, newKeyID, newKey, srcBucket, srcObject, encMetadata, kmsCtx); err != nil {
				writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
				return
			}

			// Since we are rotating the keys, make sure to update the metadata.
			srcInfo.metadataOnly = true
			srcInfo.keyRotation = true
		} else {
			if isSourceEncrypted || isTargetEncrypted {
				// We are not only copying just metadata instead
				// we are creating a new object at this point, even
				// if source and destination are same objects.
				if !srcInfo.keyRotation {
					srcInfo.metadataOnly = false
				}
			}

			// Calculate the size of the target object
			var targetSize int64

			switch {
			case isDstCompressed:
				targetSize = -1
			case !isSourceEncrypted && !isTargetEncrypted:
				targetSize, _ = srcInfo.GetActualSize()
			case isSourceEncrypted && isTargetEncrypted:
				objInfo := ObjectInfo{Size: actualSize}
				targetSize = objInfo.EncryptedSize()
			case !isSourceEncrypted && isTargetEncrypted:
				targetSize = srcInfo.EncryptedSize()
			case isSourceEncrypted && !isTargetEncrypted:
				targetSize, _ = srcInfo.DecryptedSize()
			}

			if isTargetEncrypted {
				var encReader io.Reader
				kind, _ := crypto.IsRequested(r.Header)
				encReader, objEncKey, err = newEncryptReader(srcInfo.Reader, kind, newKeyID, newKey, dstBucket, dstObject, encMetadata, kmsCtx)
				if err != nil {
					writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
					return
				}
				reader = etag.Wrap(encReader, srcInfo.Reader)
			}

			if isSourceEncrypted {
				// Remove all source encrypted related metadata to
				// avoid copying them in target object.
				crypto.RemoveInternalEntries(srcInfo.UserDefined)
			}

			// do not try to verify encrypted content
			srcInfo.Reader, err = hash.NewReader(reader, targetSize, "", "", actualSize)
			if err != nil {
				writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
				return
			}

			if isTargetEncrypted {
				pReader, err = pReader.WithEncryption(srcInfo.Reader, &objEncKey)
				if err != nil {
					writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
					return
				}
			}
		}
	}

	srcInfo.PutObjReader = pReader

	srcInfo.UserDefined, err = getCpObjMetadataFromHeader(ctx, r, srcInfo.UserDefined)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	objTags := srcInfo.UserTags
	// If x-amz-tagging-directive header is REPLACE, get passed tags.
	if isDirectiveReplace(r.Header.Get(xhttp.AmzTagDirective)) {
		objTags = r.Header.Get(xhttp.AmzObjectTagging)
		if _, err := tags.ParseObjectTags(objTags); err != nil {
			writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
			return
		}
		if globalIsGateway {
			srcInfo.UserDefined[xhttp.AmzTagDirective] = replaceDirective
		}
	}

	if objTags != "" {
		lastTaggingTimestamp := srcInfo.UserDefined[ReservedMetadataPrefixLower+TaggingTimestamp]
		if dstOpts.ReplicationRequest {
			srcTimestamp := dstOpts.ReplicationSourceTaggingTimestamp
			if !srcTimestamp.IsZero() {
				ondiskTimestamp, err := time.Parse(lastTaggingTimestamp, time.RFC3339Nano)
				// update tagging metadata only if replica  timestamp is newer than what's on disk
				if err != nil || (err == nil && ondiskTimestamp.Before(srcTimestamp)) {
					srcInfo.UserDefined[ReservedMetadataPrefixLower+TaggingTimestamp] = srcTimestamp.Format(time.RFC3339Nano)
					srcInfo.UserDefined[xhttp.AmzObjectTagging] = objTags
				}
			}
		} else {
			srcInfo.UserDefined[xhttp.AmzObjectTagging] = objTags
			srcInfo.UserDefined[ReservedMetadataPrefixLower+TaggingTimestamp] = UTCNow().Format(time.RFC3339Nano)
		}

	}

	srcInfo.UserDefined = filterReplicationStatusMetadata(srcInfo.UserDefined)
	srcInfo.UserDefined = objectlock.FilterObjectLockMetadata(srcInfo.UserDefined, true, true)
	retPerms := isPutActionAllowed(ctx, getRequestAuthType(r), dstBucket, dstObject, r, iampolicy.PutObjectRetentionAction)
	holdPerms := isPutActionAllowed(ctx, getRequestAuthType(r), dstBucket, dstObject, r, iampolicy.PutObjectLegalHoldAction)
	getObjectInfo := objectAPI.GetObjectInfo
	if api.CacheAPI() != nil {
		getObjectInfo = api.CacheAPI().GetObjectInfo
	}

	// apply default bucket configuration/governance headers for dest side.
	retentionMode, retentionDate, legalHold, s3Err := checkPutObjectLockAllowed(ctx, r, dstBucket, dstObject, getObjectInfo, retPerms, holdPerms)
	if s3Err == ErrNone && retentionMode.Valid() {
		lastretentionTimestamp := srcInfo.UserDefined[ReservedMetadataPrefixLower+ObjectLockRetentionTimestamp]
		if dstOpts.ReplicationRequest {
			srcTimestamp := dstOpts.ReplicationSourceRetentionTimestamp
			if !srcTimestamp.IsZero() {
				ondiskTimestamp, err := time.Parse(lastretentionTimestamp, time.RFC3339Nano)
				// update retention metadata only if replica  timestamp is newer than what's on disk
				if err != nil || (err == nil && ondiskTimestamp.Before(srcTimestamp)) {
					srcInfo.UserDefined[strings.ToLower(xhttp.AmzObjectLockMode)] = string(retentionMode)
					srcInfo.UserDefined[strings.ToLower(xhttp.AmzObjectLockRetainUntilDate)] = retentionDate.UTC().Format(iso8601TimeFormat)
					srcInfo.UserDefined[ReservedMetadataPrefixLower+ObjectLockRetentionTimestamp] = srcTimestamp.Format(time.RFC3339Nano)
				}
			}
		} else {
			srcInfo.UserDefined[strings.ToLower(xhttp.AmzObjectLockMode)] = string(retentionMode)
			srcInfo.UserDefined[strings.ToLower(xhttp.AmzObjectLockRetainUntilDate)] = retentionDate.UTC().Format(iso8601TimeFormat)
			srcInfo.UserDefined[ReservedMetadataPrefixLower+ObjectLockRetentionTimestamp] = UTCNow().Format(time.RFC3339Nano)
		}
	}

	if s3Err == ErrNone && legalHold.Status.Valid() {
		lastLegalHoldTimestamp := srcInfo.UserDefined[ReservedMetadataPrefixLower+ObjectLockLegalHoldTimestamp]
		if dstOpts.ReplicationRequest {
			srcTimestamp := dstOpts.ReplicationSourceLegalholdTimestamp
			if !srcTimestamp.IsZero() {
				ondiskTimestamp, err := time.Parse(lastLegalHoldTimestamp, time.RFC3339Nano)
				// update legalhold metadata only if replica  timestamp is newer than what's on disk
				if err != nil || (err == nil && ondiskTimestamp.Before(srcTimestamp)) {
					srcInfo.UserDefined[strings.ToLower(xhttp.AmzObjectLockLegalHold)] = string(legalHold.Status)
					srcInfo.UserDefined[ReservedMetadataPrefixLower+ObjectLockRetentionTimestamp] = srcTimestamp.Format(time.RFC3339Nano)
				}
			}
		} else {
			srcInfo.UserDefined[strings.ToLower(xhttp.AmzObjectLockLegalHold)] = string(legalHold.Status)
		}
	}
	if s3Err != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Err), r.URL)
		return
	}
	if rs := r.Header.Get(xhttp.AmzBucketReplicationStatus); rs != "" {
		srcInfo.UserDefined[ReservedMetadataPrefixLower+ReplicaStatus] = replication.Replica.String()
		srcInfo.UserDefined[ReservedMetadataPrefixLower+ReplicaTimestamp] = UTCNow().Format(time.RFC3339Nano)
		srcInfo.UserDefined[xhttp.AmzBucketReplicationStatus] = rs
	}
	if dsc := mustReplicate(ctx, dstBucket, dstObject, getMustReplicateOptions(srcInfo, replication.UnsetReplicationType, dstOpts)); dsc.ReplicateAny() {
		srcInfo.UserDefined[ReservedMetadataPrefixLower+ReplicationStatus] = dsc.PendingStatus()
		srcInfo.UserDefined[ReservedMetadataPrefixLower+ReplicationTimestamp] = UTCNow().Format(time.RFC3339Nano)
	}
	// Store the preserved compression metadata.
	for k, v := range compressMetadata {
		srcInfo.UserDefined[k] = v
	}

	// We need to preserve the encryption headers set in EncryptRequest,
	// so we do not want to override them, copy them instead.
	for k, v := range encMetadata {
		srcInfo.UserDefined[k] = v
	}

	// Ensure that metadata does not contain sensitive information
	crypto.RemoveSensitiveEntries(srcInfo.UserDefined)

	// If we see legacy source, metadataOnly we have to overwrite the content.
	if srcInfo.Legacy {
		srcInfo.metadataOnly = false
	}

	// Check if x-amz-metadata-directive or x-amz-tagging-directive was not set to REPLACE and source,
	// destination are same objects. Apply this restriction also when
	// metadataOnly is true indicating that we are not overwriting the object.
	// if encryption is enabled we do not need explicit "REPLACE" metadata to
	// be enabled as well - this is to allow for key-rotation.
	if !isDirectiveReplace(r.Header.Get(xhttp.AmzMetadataDirective)) && !isDirectiveReplace(r.Header.Get(xhttp.AmzTagDirective)) &&
		srcInfo.metadataOnly && srcOpts.VersionID == "" && !objectEncryption {
		// If x-amz-metadata-directive is not set to REPLACE then we need
		// to error out if source and destination are same.
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrInvalidCopyDest), r.URL)
		return
	}

	var objInfo ObjectInfo

	if isRemoteCopyRequired(ctx, srcBucket, dstBucket, objectAPI) {
		var dstRecords []dns.SrvRecord
		dstRecords, err = globalDNSConfig.Get(dstBucket)
		if err != nil {
			writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
			return
		}

		// Send PutObject request to appropriate instance (in federated deployment)
		core, rerr := getRemoteInstanceClient(r, getHostFromSrv(dstRecords))
		if rerr != nil {
			writeErrorResponse(ctx, w, toAPIError(ctx, rerr), r.URL)
			return
		}
		tag, err := tags.ParseObjectTags(objTags)
		if err != nil {
			writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
			return
		}
		// Remove the metadata for remote calls.
		delete(srcInfo.UserDefined, ReservedMetadataPrefix+"compression")
		delete(srcInfo.UserDefined, ReservedMetadataPrefix+"actual-size")
		opts := miniogo.PutObjectOptions{
			UserMetadata:         srcInfo.UserDefined,
			ServerSideEncryption: dstOpts.ServerSideEncryption,
			UserTags:             tag.ToMap(),
		}
		remoteObjInfo, rerr := core.PutObject(ctx, dstBucket, dstObject, srcInfo.Reader,
			srcInfo.Size, "", "", opts)
		if rerr != nil {
			writeErrorResponse(ctx, w, toAPIError(ctx, rerr), r.URL)
			return
		}
		objInfo.ETag = remoteObjInfo.ETag
		objInfo.ModTime = remoteObjInfo.LastModified
	} else {

		os := newObjSweeper(dstBucket, dstObject).WithVersioning(dstOpts.Versioned, dstOpts.VersionSuspended)
		// Get appropriate object info to identify the remote object to delete
		if !srcInfo.metadataOnly {
			goiOpts := os.GetOpts()
			if !globalTierConfigMgr.Empty() {
				if goi, gerr := getObjectInfo(ctx, dstBucket, dstObject, goiOpts); gerr == nil {
					os.SetTransitionState(goi.TransitionedObject)
				}
			}
		}

		copyObjectFn := objectAPI.CopyObject
		if api.CacheAPI() != nil {
			copyObjectFn = api.CacheAPI().CopyObject
		}

		// Copy source object to destination, if source and destination
		// object is same then only metadata is updated.
		objInfo, err = copyObjectFn(ctx, srcBucket, srcObject, dstBucket, dstObject, srcInfo, srcOpts, dstOpts)
		if err != nil {
			writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
			return
		}

		// Remove the transitioned object whose object version is being overwritten.
		if !globalTierConfigMgr.Empty() {
			logger.LogIf(ctx, os.Sweep())
		}
	}

	objInfo.ETag = getDecryptedETag(r.Header, objInfo, false)
	response := generateCopyObjectResponse(objInfo.ETag, objInfo.ModTime)
	encodedSuccessResponse := encodeResponse(response)

	if dsc := mustReplicate(ctx, dstBucket, dstObject, getMustReplicateOptions(objInfo, replication.UnsetReplicationType, dstOpts)); dsc.ReplicateAny() {
		scheduleReplication(ctx, objInfo.Clone(), objectAPI, dsc, replication.ObjectReplicationType)
	}

	setPutObjHeaders(w, objInfo, false)
	// We must not use the http.Header().Set method here because some (broken)
	// clients expect the x-amz-copy-source-version-id header key to be literally
	// "x-amz-copy-source-version-id"- not in canonicalized form, preserve it.
	if srcOpts.VersionID != "" {
		w.Header()[strings.ToLower(xhttp.AmzCopySourceVersionID)] = []string{srcOpts.VersionID}
	}

	// Write success response.
	writeSuccessResponseXML(w, encodedSuccessResponse)

	// Notify object created event.
	sendEvent(eventArgs{
		EventName:    event.ObjectCreatedCopy,
		BucketName:   dstBucket,
		Object:       objInfo,
		ReqParams:    extractReqParams(r),
		RespElements: extractRespElements(w),
		UserAgent:    r.UserAgent(),
		Host:         handlers.GetSourceIP(r),
	})
	if !globalTierConfigMgr.Empty() {
		// Schedule object for immediate transition if eligible.
		enqueueTransitionImmediate(objInfo)
	}
}

// PutObjectHandler - PUT Object
// ----------
// This implementation of the PUT operation adds an object to a bucket.
// Notice: The S3 client can send secret keys in headers for encryption related jobs,
// the handler should ensure to remove these keys before sending them to the object layer.
// Currently these keys are:
//   - X-Amz-Server-Side-Encryption-Customer-Key
//   - X-Amz-Copy-Source-Server-Side-Encryption-Customer-Key
func (api objectAPIHandlers) PutObjectHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "PutObject")
	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	objectAPI := api.ObjectAPI()
	if objectAPI == nil {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrServerNotInitialized), r.URL)
		return
	}

	if _, ok := crypto.IsRequested(r.Header); ok {
		if globalIsGateway {
			if crypto.SSEC.IsRequested(r.Header) && !objectAPI.IsEncryptionSupported() {
				writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrNotImplemented), r.URL)
				return
			}
		} else {
			if !objectAPI.IsEncryptionSupported() {
				writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrNotImplemented), r.URL)
				return
			}
		}
	}

	vars := mux.Vars(r)
	bucket := vars["bucket"]
	object, err := unescapePath(vars["object"])
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	// X-Amz-Copy-Source shouldn't be set for this call.
	if _, ok := r.Header[xhttp.AmzCopySource]; ok {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrInvalidCopySource), r.URL)
		return
	}

	// Validate storage class metadata if present
	if sc := r.Header.Get(xhttp.AmzStorageClass); sc != "" {
		if !storageclass.IsValid(sc) {
			writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrInvalidStorageClass), r.URL)
			return
		}
	}

	clientETag, err := etag.FromContentMD5(r.Header)
	if err != nil {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrInvalidDigest), r.URL)
		return
	}

	// if Content-Length is unknown/missing, deny the request
	size := r.ContentLength
	rAuthType := getRequestAuthType(r)
	if rAuthType == authTypeStreamingSigned {
		if sizeStr, ok := r.Header[xhttp.AmzDecodedContentLength]; ok {
			if sizeStr[0] == "" {
				writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrMissingContentLength), r.URL)
				return
			}
			size, err = strconv.ParseInt(sizeStr[0], 10, 64)
			if err != nil {
				writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
				return
			}
		}
	}
	if size == -1 {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrMissingContentLength), r.URL)
		return
	}

	// maximum Upload size for objects in a single operation
	if isMaxObjectSize(size) {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrEntityTooLarge), r.URL)
		return
	}

	metadata, err := extractMetadata(ctx, r)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	if objTags := r.Header.Get(xhttp.AmzObjectTagging); objTags != "" {
		if !objectAPI.IsTaggingSupported() {
			writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrNotImplemented), r.URL)
			return
		}

		if _, err := tags.ParseObjectTags(objTags); err != nil {
			writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
			return
		}

		metadata[xhttp.AmzObjectTagging] = objTags
	}

	var (
		md5hex              = clientETag.String()
		sha256hex           = ""
		reader    io.Reader = r.Body
		s3Err     APIErrorCode
		putObject = objectAPI.PutObject
	)

	// Check if put is allowed
	if s3Err = isPutActionAllowed(ctx, rAuthType, bucket, object, r, iampolicy.PutObjectAction); s3Err != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Err), r.URL)
		return
	}

	switch rAuthType {
	case authTypeStreamingSigned:
		// Initialize stream signature verifier.
		reader, s3Err = newSignV4ChunkedReader(r)
		if s3Err != ErrNone {
			writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Err), r.URL)
			return
		}
	case authTypeSignedV2, authTypePresignedV2:
		s3Err = isReqAuthenticatedV2(r)
		if s3Err != ErrNone {
			writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Err), r.URL)
			return
		}

	case authTypePresigned, authTypeSigned:
		if s3Err = reqSignatureV4Verify(r, globalSite.Region, serviceS3); s3Err != ErrNone {
			writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Err), r.URL)
			return
		}
		if !skipContentSha256Cksum(r) {
			sha256hex = getContentSha256Cksum(r, serviceS3)
		}
	}

	if err := enforceBucketQuotaHard(ctx, bucket, size); err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}
	if r.Header.Get(xhttp.AmzBucketReplicationStatus) == replication.Replica.String() {
		if s3Err = isPutActionAllowed(ctx, getRequestAuthType(r), bucket, object, r, iampolicy.ReplicateObjectAction); s3Err != ErrNone {
			writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Err), r.URL)
			return
		}
		metadata[ReservedMetadataPrefixLower+ReplicaStatus] = replication.Replica.String()
		metadata[ReservedMetadataPrefixLower+ReplicaTimestamp] = UTCNow().Format(time.RFC3339Nano)
		defer globalReplicationStats.UpdateReplicaStat(bucket, size)
	}

	// Check if bucket encryption is enabled
	sseConfig, _ := globalBucketSSEConfigSys.Get(bucket)
	sseConfig.Apply(r.Header, sse.ApplyOptions{
		AutoEncrypt: globalAutoEncryption,
		Passthrough: globalIsGateway && globalGatewayName == S3BackendGateway,
	})

	actualSize := size
	if objectAPI.IsCompressionSupported() && isCompressible(r.Header, object) && size > 0 {
		// Storing the compression metadata.
		metadata[ReservedMetadataPrefix+"compression"] = compressionAlgorithmV2
		metadata[ReservedMetadataPrefix+"actual-size"] = strconv.FormatInt(size, 10)

		actualReader, err := hash.NewReader(reader, size, md5hex, sha256hex, actualSize)
		if err != nil {
			writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
			return
		}

		// Set compression metrics.
		s2c := newS2CompressReader(actualReader, actualSize)
		defer s2c.Close()
		reader = etag.Wrap(s2c, actualReader)
		size = -1   // Since compressed size is un-predictable.
		md5hex = "" // Do not try to verify the content.
		sha256hex = ""
	}

	hashReader, err := hash.NewReader(reader, size, md5hex, sha256hex, actualSize)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	rawReader := hashReader
	pReader := NewPutObjReader(rawReader)

	// get gateway encryption options
	var opts ObjectOptions
	opts, err = putOpts(ctx, r, bucket, object, metadata)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	if api.CacheAPI() != nil {
		putObject = api.CacheAPI().PutObject
	}

	retPerms := isPutActionAllowed(ctx, getRequestAuthType(r), bucket, object, r, iampolicy.PutObjectRetentionAction)
	holdPerms := isPutActionAllowed(ctx, getRequestAuthType(r), bucket, object, r, iampolicy.PutObjectLegalHoldAction)

	getObjectInfo := objectAPI.GetObjectInfo
	if api.CacheAPI() != nil {
		getObjectInfo = api.CacheAPI().GetObjectInfo
	}

	retentionMode, retentionDate, legalHold, s3Err := checkPutObjectLockAllowed(ctx, r, bucket, object, getObjectInfo, retPerms, holdPerms)
	if s3Err == ErrNone && retentionMode.Valid() {
		metadata[strings.ToLower(xhttp.AmzObjectLockMode)] = string(retentionMode)
		metadata[strings.ToLower(xhttp.AmzObjectLockRetainUntilDate)] = retentionDate.UTC().Format(iso8601TimeFormat)
	}
	if s3Err == ErrNone && legalHold.Status.Valid() {
		metadata[strings.ToLower(xhttp.AmzObjectLockLegalHold)] = string(legalHold.Status)
	}
	if s3Err != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Err), r.URL)
		return
	}
	if dsc := mustReplicate(ctx, bucket, object, getMustReplicateOptions(ObjectInfo{
		UserDefined: metadata,
	}, replication.ObjectReplicationType, opts)); dsc.ReplicateAny() {
		metadata[ReservedMetadataPrefixLower+ReplicationTimestamp] = UTCNow().Format(time.RFC3339Nano)
		metadata[ReservedMetadataPrefixLower+ReplicationStatus] = dsc.PendingStatus()
	}
	var objectEncryptionKey crypto.ObjectKey
	if objectAPI.IsEncryptionSupported() {
		if _, ok := crypto.IsRequested(r.Header); ok && !HasSuffix(object, SlashSeparator) { // handle SSE requests
			if crypto.SSECopy.IsRequested(r.Header) {
				writeErrorResponse(ctx, w, toAPIError(ctx, errInvalidEncryptionParameters), r.URL)
				return
			}

			reader, objectEncryptionKey, err = EncryptRequest(hashReader, r, bucket, object, metadata)
			if err != nil {
				writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
				return
			}

			wantSize := int64(-1)
			if size >= 0 {
				info := ObjectInfo{Size: size}
				wantSize = info.EncryptedSize()
			}

			// do not try to verify encrypted content
			hashReader, err = hash.NewReader(etag.Wrap(reader, hashReader), wantSize, "", "", actualSize)
			if err != nil {
				writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
				return
			}
			pReader, err = pReader.WithEncryption(hashReader, &objectEncryptionKey)
			if err != nil {
				writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
				return
			}
		}
	}

	// Ensure that metadata does not contain sensitive information
	crypto.RemoveSensitiveEntries(metadata)

	os := newObjSweeper(bucket, object).WithVersioning(opts.Versioned, opts.VersionSuspended)
	if !globalTierConfigMgr.Empty() {
		// Get appropriate object info to identify the remote object to delete
		goiOpts := os.GetOpts()
		if goi, gerr := getObjectInfo(ctx, bucket, object, goiOpts); gerr == nil {
			os.SetTransitionState(goi.TransitionedObject)
		}
	}

	// Create the object..
	objInfo, err := putObject(ctx, bucket, object, pReader, opts)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	if r.Header.Get(xMinIOExtract) == "true" && strings.HasSuffix(object, archiveExt) {
		opts := ObjectOptions{VersionID: objInfo.VersionID, MTime: objInfo.ModTime}
		if _, err := updateObjectMetadataWithZipInfo(ctx, objectAPI, bucket, object, opts); err != nil {
			writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
			return
		}
	}

	if kind, encrypted := crypto.IsEncrypted(objInfo.UserDefined); encrypted {
		switch kind {
		case crypto.S3:
			w.Header().Set(xhttp.AmzServerSideEncryption, xhttp.AmzEncryptionAES)
			objInfo.ETag, _ = DecryptETag(objectEncryptionKey, ObjectInfo{ETag: objInfo.ETag})
		case crypto.S3KMS:
			w.Header().Set(xhttp.AmzServerSideEncryption, xhttp.AmzEncryptionKMS)
			w.Header().Set(xhttp.AmzServerSideEncryptionKmsID, objInfo.KMSKeyID())
			if kmsCtx, ok := objInfo.UserDefined[crypto.MetaContext]; ok {
				w.Header().Set(xhttp.AmzServerSideEncryptionKmsContext, kmsCtx)
			}
			if len(objInfo.ETag) >= 32 && strings.Count(objInfo.ETag, "-") != 1 {
				objInfo.ETag = objInfo.ETag[len(objInfo.ETag)-32:]
			}
		case crypto.SSEC:
			w.Header().Set(xhttp.AmzServerSideEncryptionCustomerAlgorithm, r.Header.Get(xhttp.AmzServerSideEncryptionCustomerAlgorithm))
			w.Header().Set(xhttp.AmzServerSideEncryptionCustomerKeyMD5, r.Header.Get(xhttp.AmzServerSideEncryptionCustomerKeyMD5))

			if len(objInfo.ETag) >= 32 && strings.Count(objInfo.ETag, "-") != 1 {
				objInfo.ETag = objInfo.ETag[len(objInfo.ETag)-32:]
			}
		}
	}
	if dsc := mustReplicate(ctx, bucket, object, getMustReplicateOptions(ObjectInfo{
		UserDefined: metadata,
	}, replication.ObjectReplicationType, opts)); dsc.ReplicateAny() {
		scheduleReplication(ctx, objInfo.Clone(), objectAPI, dsc, replication.ObjectReplicationType)
	}

	setPutObjHeaders(w, objInfo, false)

	writeSuccessResponseHeadersOnly(w)

	// Notify object created event.
	sendEvent(eventArgs{
		EventName:    event.ObjectCreatedPut,
		BucketName:   bucket,
		Object:       objInfo,
		ReqParams:    extractReqParams(r),
		RespElements: extractRespElements(w),
		UserAgent:    r.UserAgent(),
		Host:         handlers.GetSourceIP(r),
	})

	// Remove the transitioned object whose object version is being overwritten.
	if !globalTierConfigMgr.Empty() {
		// Schedule object for immediate transition if eligible.
		enqueueTransitionImmediate(objInfo)
		logger.LogIf(ctx, os.Sweep())
	}
}

// PutObjectExtractHandler - PUT Object extract is an extended API
// based off from AWS Snowball feature to auto extract compressed
// stream will be extracted in the same directory it is stored in
// and the folder structures will be built out accordingly.
func (api objectAPIHandlers) PutObjectExtractHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "PutObjectExtract")
	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	objectAPI := api.ObjectAPI()
	if objectAPI == nil {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrServerNotInitialized), r.URL)
		return
	}

	if crypto.S3KMS.IsRequested(r.Header) { // SSE-KMS is not supported
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrNotImplemented), r.URL)
		return
	}

	if _, ok := crypto.IsRequested(r.Header); ok {
		if globalIsGateway {
			if crypto.SSEC.IsRequested(r.Header) && !objectAPI.IsEncryptionSupported() {
				writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrNotImplemented), r.URL)
				return
			}
		} else {
			if !objectAPI.IsEncryptionSupported() {
				writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrNotImplemented), r.URL)
				return
			}
		}
	}

	vars := mux.Vars(r)
	bucket := vars["bucket"]
	object, err := unescapePath(vars["object"])
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	// X-Amz-Copy-Source shouldn't be set for this call.
	if _, ok := r.Header[xhttp.AmzCopySource]; ok {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrInvalidCopySource), r.URL)
		return
	}

	// Validate storage class metadata if present
	sc := r.Header.Get(xhttp.AmzStorageClass)
	if sc != "" {
		if !storageclass.IsValid(sc) {
			writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrInvalidStorageClass), r.URL)
			return
		}
	}

	clientETag, err := etag.FromContentMD5(r.Header)
	if err != nil {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrInvalidDigest), r.URL)
		return
	}

	// if Content-Length is unknown/missing, deny the request
	size := r.ContentLength
	rAuthType := getRequestAuthType(r)
	if rAuthType == authTypeStreamingSigned {
		if sizeStr, ok := r.Header[xhttp.AmzDecodedContentLength]; ok {
			if sizeStr[0] == "" {
				writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrMissingContentLength), r.URL)
				return
			}
			size, err = strconv.ParseInt(sizeStr[0], 10, 64)
			if err != nil {
				writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
				return
			}
		}
	}

	if size == -1 {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrMissingContentLength), r.URL)
		return
	}

	// maximum Upload size for objects in a single operation
	if isMaxObjectSize(size) {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrEntityTooLarge), r.URL)
		return
	}

	var (
		md5hex              = clientETag.String()
		sha256hex           = ""
		reader    io.Reader = r.Body
		s3Err     APIErrorCode
		putObject = objectAPI.PutObject
	)

	// Check if put is allowed
	if s3Err = isPutActionAllowed(ctx, rAuthType, bucket, object, r, iampolicy.PutObjectAction); s3Err != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Err), r.URL)
		return
	}

	switch rAuthType {
	case authTypeStreamingSigned:
		// Initialize stream signature verifier.
		reader, s3Err = newSignV4ChunkedReader(r)
		if s3Err != ErrNone {
			writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Err), r.URL)
			return
		}
	case authTypeSignedV2, authTypePresignedV2:
		s3Err = isReqAuthenticatedV2(r)
		if s3Err != ErrNone {
			writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Err), r.URL)
			return
		}

	case authTypePresigned, authTypeSigned:
		if s3Err = reqSignatureV4Verify(r, globalSite.Region, serviceS3); s3Err != ErrNone {
			writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Err), r.URL)
			return
		}
		if !skipContentSha256Cksum(r) {
			sha256hex = getContentSha256Cksum(r, serviceS3)
		}
	}

	hreader, err := hash.NewReader(reader, size, md5hex, sha256hex, size)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	if err := enforceBucketQuotaHard(ctx, bucket, size); err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	// Check if bucket encryption is enabled
	sseConfig, _ := globalBucketSSEConfigSys.Get(bucket)
	sseConfig.Apply(r.Header, sse.ApplyOptions{
		AutoEncrypt: globalAutoEncryption,
		Passthrough: globalIsGateway && globalGatewayName == S3BackendGateway,
	})

	retPerms := isPutActionAllowed(ctx, getRequestAuthType(r), bucket, object, r, iampolicy.PutObjectRetentionAction)
	holdPerms := isPutActionAllowed(ctx, getRequestAuthType(r), bucket, object, r, iampolicy.PutObjectLegalHoldAction)

	if api.CacheAPI() != nil {
		putObject = api.CacheAPI().PutObject
	}

	getObjectInfo := objectAPI.GetObjectInfo
	if api.CacheAPI() != nil {
		getObjectInfo = api.CacheAPI().GetObjectInfo
	}

	putObjectTar := func(reader io.Reader, info os.FileInfo, object string) error {
		size := info.Size()
		metadata := map[string]string{
			xhttp.AmzStorageClass: sc,
		}

		actualSize := size
		if objectAPI.IsCompressionSupported() && isCompressible(r.Header, object) && size > 0 {
			// Storing the compression metadata.
			metadata[ReservedMetadataPrefix+"compression"] = compressionAlgorithmV2
			metadata[ReservedMetadataPrefix+"actual-size"] = strconv.FormatInt(size, 10)

			actualReader, err := hash.NewReader(reader, size, "", "", actualSize)
			if err != nil {
				return err
			}

			// Set compression metrics.
			s2c := newS2CompressReader(actualReader, actualSize)
			defer s2c.Close()
			reader = etag.Wrap(s2c, actualReader)
			size = -1 // Since compressed size is un-predictable.
		}

		hashReader, err := hash.NewReader(reader, size, "", "", actualSize)
		if err != nil {
			return err
		}

		rawReader := hashReader
		pReader := NewPutObjReader(rawReader)

		if r.Header.Get(xhttp.AmzBucketReplicationStatus) == replication.Replica.String() {
			if s3Err = isPutActionAllowed(ctx, getRequestAuthType(r), bucket, object, r, iampolicy.ReplicateObjectAction); s3Err != ErrNone {
				return err
			}
			metadata[ReservedMetadataPrefixLower+ReplicaStatus] = replication.Replica.String()
			metadata[ReservedMetadataPrefixLower+ReplicaTimestamp] = UTCNow().Format(time.RFC3339Nano)
		}

		// get encryption options
		opts, err := putOpts(ctx, r, bucket, object, metadata)
		if err != nil {
			return err
		}
		opts.MTime = info.ModTime()

		retentionMode, retentionDate, legalHold, s3err := checkPutObjectLockAllowed(ctx, r, bucket, object, getObjectInfo, retPerms, holdPerms)
		if s3err == ErrNone && retentionMode.Valid() {
			metadata[strings.ToLower(xhttp.AmzObjectLockMode)] = string(retentionMode)
			metadata[strings.ToLower(xhttp.AmzObjectLockRetainUntilDate)] = retentionDate.UTC().Format(iso8601TimeFormat)
		}

		if s3err == ErrNone && legalHold.Status.Valid() {
			metadata[strings.ToLower(xhttp.AmzObjectLockLegalHold)] = string(legalHold.Status)
		}

		if s3err != ErrNone {
			s3Err = s3err
			return ObjectLocked{}
		}

		if dsc := mustReplicate(ctx, bucket, object, getMustReplicateOptions(ObjectInfo{
			UserDefined: metadata,
		}, replication.ObjectReplicationType, opts)); dsc.ReplicateAny() {
			metadata[ReservedMetadataPrefixLower+ReplicationTimestamp] = UTCNow().Format(time.RFC3339Nano)
			metadata[ReservedMetadataPrefixLower+ReplicationStatus] = dsc.PendingStatus()

		}

		var objectEncryptionKey crypto.ObjectKey
		if objectAPI.IsEncryptionSupported() {
			if _, ok := crypto.IsRequested(r.Header); ok && !HasSuffix(object, SlashSeparator) { // handle SSE requests
				if crypto.SSECopy.IsRequested(r.Header) {
					return errInvalidEncryptionParameters
				}

				reader, objectEncryptionKey, err = EncryptRequest(hashReader, r, bucket, object, metadata)
				if err != nil {
					return err
				}

				wantSize := int64(-1)
				if size >= 0 {
					info := ObjectInfo{Size: size}
					wantSize = info.EncryptedSize()
				}

				// do not try to verify encrypted content
				hashReader, err = hash.NewReader(etag.Wrap(reader, hashReader), wantSize, "", "", actualSize)
				if err != nil {
					return err
				}

				pReader, err = pReader.WithEncryption(hashReader, &objectEncryptionKey)
				if err != nil {
					return err
				}
			}
		}

		// Ensure that metadata does not contain sensitive information
		crypto.RemoveSensitiveEntries(metadata)

		// Create the object..
		objInfo, err := putObject(ctx, bucket, object, pReader, opts)
		if err != nil {
			return err
		}

		if dsc := mustReplicate(ctx, bucket, object, getMustReplicateOptions(ObjectInfo{
			UserDefined: metadata,
		}, replication.ObjectReplicationType, opts)); dsc.ReplicateAny() {
			scheduleReplication(ctx, objInfo.Clone(), objectAPI, dsc, replication.ObjectReplicationType)
		}
		return nil
	}

	if err = untar(hreader, putObjectTar); err != nil {
		apiErr := errorCodes.ToAPIErr(s3Err)
		// If not set, convert or use BadRequest
		if s3Err == ErrNone {
			apiErr = toAPIError(ctx, err)
			if apiErr.Code == "InternalError" {
				// Convert generic internal errors to bad requests.
				apiErr = APIError{
					Code:           "BadRequest",
					Description:    err.Error(),
					HTTPStatusCode: http.StatusBadRequest,
				}
			}
		}
		writeErrorResponse(ctx, w, apiErr, r.URL)
		return
	}

	w.Header()[xhttp.ETag] = []string{`"` + hex.EncodeToString(hreader.MD5Current()) + `"`}
	writeSuccessResponseHeadersOnly(w)
}

// Multipart objectAPIHandlers

// NewMultipartUploadHandler - New multipart upload.
// Notice: The S3 client can send secret keys in headers for encryption related jobs,
// the handler should ensure to remove these keys before sending them to the object layer.
// Currently these keys are:
//   - X-Amz-Server-Side-Encryption-Customer-Key
//   - X-Amz-Copy-Source-Server-Side-Encryption-Customer-Key
func (api objectAPIHandlers) NewMultipartUploadHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "NewMultipartUpload")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	objectAPI := api.ObjectAPI()
	if objectAPI == nil {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrServerNotInitialized), r.URL)
		return
	}

	if _, ok := crypto.IsRequested(r.Header); ok {
		if globalIsGateway {
			if crypto.SSEC.IsRequested(r.Header) && !objectAPI.IsEncryptionSupported() {
				writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrNotImplemented), r.URL)
				return
			}
		} else {
			if !objectAPI.IsEncryptionSupported() {
				writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrNotImplemented), r.URL)
				return
			}
		}
	}

	vars := mux.Vars(r)
	bucket := vars["bucket"]
	object, err := unescapePath(vars["object"])
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	if s3Error := checkRequestAuthType(ctx, r, policy.PutObjectAction, bucket, object); s3Error != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Error), r.URL)
		return
	}

	// Check if bucket encryption is enabled
	sseConfig, _ := globalBucketSSEConfigSys.Get(bucket)
	sseConfig.Apply(r.Header, sse.ApplyOptions{
		AutoEncrypt: globalAutoEncryption,
		Passthrough: globalIsGateway && globalGatewayName == S3BackendGateway,
	})

	// Validate storage class metadata if present
	if sc := r.Header.Get(xhttp.AmzStorageClass); sc != "" {
		if !storageclass.IsValid(sc) {
			writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrInvalidStorageClass), r.URL)
			return
		}
	}

	encMetadata := map[string]string{}

	if objectAPI.IsEncryptionSupported() {
		if _, ok := crypto.IsRequested(r.Header); ok {
			if err = setEncryptionMetadata(r, bucket, object, encMetadata); err != nil {
				writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
				return
			}
			// Set this for multipart only operations, we need to differentiate during
			// decryption if the file was actually multipart or not.
			encMetadata[ReservedMetadataPrefix+"Encrypted-Multipart"] = ""
		}
	}

	// Extract metadata that needs to be saved.
	metadata, err := extractMetadata(ctx, r)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	retPerms := isPutActionAllowed(ctx, getRequestAuthType(r), bucket, object, r, iampolicy.PutObjectRetentionAction)
	holdPerms := isPutActionAllowed(ctx, getRequestAuthType(r), bucket, object, r, iampolicy.PutObjectLegalHoldAction)

	getObjectInfo := objectAPI.GetObjectInfo
	if api.CacheAPI() != nil {
		getObjectInfo = api.CacheAPI().GetObjectInfo
	}

	retentionMode, retentionDate, legalHold, s3Err := checkPutObjectLockAllowed(ctx, r, bucket, object, getObjectInfo, retPerms, holdPerms)
	if s3Err == ErrNone && retentionMode.Valid() {
		metadata[strings.ToLower(xhttp.AmzObjectLockMode)] = string(retentionMode)
		metadata[strings.ToLower(xhttp.AmzObjectLockRetainUntilDate)] = retentionDate.UTC().Format(iso8601TimeFormat)
	}
	if s3Err == ErrNone && legalHold.Status.Valid() {
		metadata[strings.ToLower(xhttp.AmzObjectLockLegalHold)] = string(legalHold.Status)
	}
	if s3Err != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Err), r.URL)
		return
	}
	if dsc := mustReplicate(ctx, bucket, object, getMustReplicateOptions(ObjectInfo{
		UserDefined: metadata,
	}, replication.ObjectReplicationType, ObjectOptions{})); dsc.ReplicateAny() {
		metadata[ReservedMetadataPrefixLower+ReplicationTimestamp] = UTCNow().Format(time.RFC3339Nano)
		metadata[ReservedMetadataPrefixLower+ReplicationStatus] = dsc.PendingStatus()
	}
	// We need to preserve the encryption headers set in EncryptRequest,
	// so we do not want to override them, copy them instead.
	for k, v := range encMetadata {
		metadata[k] = v
	}

	// Ensure that metadata does not contain sensitive information
	crypto.RemoveSensitiveEntries(metadata)

	if objectAPI.IsCompressionSupported() && isCompressible(r.Header, object) {
		// Storing the compression metadata.
		metadata[ReservedMetadataPrefix+"compression"] = compressionAlgorithmV2
	}

	opts, err := putOpts(ctx, r, bucket, object, metadata)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}
	newMultipartUpload := objectAPI.NewMultipartUpload
	if api.CacheAPI() != nil {
		newMultipartUpload = api.CacheAPI().NewMultipartUpload
	}

	uploadID, err := newMultipartUpload(ctx, bucket, object, opts)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	response := generateInitiateMultipartUploadResponse(bucket, object, uploadID)
	encodedSuccessResponse := encodeResponse(response)

	// Write success response.
	writeSuccessResponseXML(w, encodedSuccessResponse)
}

// CopyObjectPartHandler - uploads a part by copying data from an existing object as data source.
func (api objectAPIHandlers) CopyObjectPartHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "CopyObjectPart")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	objectAPI := api.ObjectAPI()
	if objectAPI == nil {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrServerNotInitialized), r.URL)
		return
	}

	if crypto.S3KMS.IsRequested(r.Header) { // SSE-KMS is not supported
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrNotImplemented), r.URL)
		return
	}

	if _, ok := crypto.IsRequested(r.Header); !objectAPI.IsEncryptionSupported() && ok {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrNotImplemented), r.URL)
		return
	}

	vars := mux.Vars(r)
	dstBucket := vars["bucket"]
	dstObject, err := unescapePath(vars["object"])
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	if s3Error := checkRequestAuthType(ctx, r, policy.PutObjectAction, dstBucket, dstObject); s3Error != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Error), r.URL)
		return
	}

	// Read escaped copy source path to check for parameters.
	cpSrcPath := r.Header.Get(xhttp.AmzCopySource)
	var vid string
	if u, err := url.Parse(cpSrcPath); err == nil {
		vid = strings.TrimSpace(u.Query().Get(xhttp.VersionID))
		// Note that url.Parse does the unescaping
		cpSrcPath = u.Path
	}

	srcBucket, srcObject := path2BucketObject(cpSrcPath)
	// If source object is empty or bucket is empty, reply back invalid copy source.
	if srcObject == "" || srcBucket == "" {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrInvalidCopySource), r.URL)
		return
	}

	if vid != "" && vid != nullVersionID {
		_, err := uuid.Parse(vid)
		if err != nil {
			writeErrorResponse(ctx, w, toAPIError(ctx, VersionNotFound{
				Bucket:    srcBucket,
				Object:    srcObject,
				VersionID: vid,
			}), r.URL)
			return
		}
	}

	if s3Error := checkRequestAuthType(ctx, r, policy.GetObjectAction, srcBucket, srcObject); s3Error != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Error), r.URL)
		return
	}

	uploadID := r.Form.Get(xhttp.UploadID)
	partIDString := r.Form.Get(xhttp.PartNumber)

	partID, err := strconv.Atoi(partIDString)
	if err != nil {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrInvalidPart), r.URL)
		return
	}

	// check partID with maximum part ID for multipart objects
	if isMaxPartID(partID) {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrInvalidMaxParts), r.URL)
		return
	}

	var srcOpts, dstOpts ObjectOptions
	srcOpts, err = copySrcOpts(ctx, r, srcBucket, srcObject)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}
	srcOpts.VersionID = vid

	// convert copy src and dst encryption options for GET/PUT calls
	getOpts := ObjectOptions{VersionID: srcOpts.VersionID}
	if srcOpts.ServerSideEncryption != nil {
		getOpts.ServerSideEncryption = encrypt.SSE(srcOpts.ServerSideEncryption)
	}

	dstOpts, err = copyDstOpts(ctx, r, dstBucket, dstObject, nil)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	getObjectNInfo := objectAPI.GetObjectNInfo
	if api.CacheAPI() != nil {
		getObjectNInfo = api.CacheAPI().GetObjectNInfo
	}

	// Get request range.
	var rs *HTTPRangeSpec
	var parseRangeErr error
	if rangeHeader := r.Header.Get(xhttp.AmzCopySourceRange); rangeHeader != "" {
		rs, parseRangeErr = parseCopyPartRangeSpec(rangeHeader)
	} else {
		// This check is to see if client specified a header but the value
		// is empty for 'x-amz-copy-source-range'
		_, ok := r.Header[xhttp.AmzCopySourceRange]
		if ok {
			parseRangeErr = errInvalidRange
		}
	}

	checkCopyPartPrecondFn := func(o ObjectInfo) bool {
		if objectAPI.IsEncryptionSupported() {
			if _, err := DecryptObjectInfo(&o, r); err != nil {
				writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
				return true
			}
		}
		if checkCopyObjectPartPreconditions(ctx, w, r, o) {
			return true
		}
		if parseRangeErr != nil {
			logger.LogIf(ctx, parseRangeErr)
			writeCopyPartErr(ctx, w, parseRangeErr, r.URL)
			// Range header mismatch is pre-condition like failure
			// so return true to indicate Range precondition failed.
			return true
		}
		return false
	}
	getOpts.CheckPrecondFn = checkCopyPartPrecondFn
	gr, err := getObjectNInfo(ctx, srcBucket, srcObject, rs, r.Header, readLock, getOpts)
	if err != nil {
		if isErrPreconditionFailed(err) {
			return
		}
		if globalBucketVersioningSys.Enabled(srcBucket) && gr != nil {
			// Versioning enabled quite possibly object is deleted might be delete-marker
			// if present set the headers, no idea why AWS S3 sets these headers.
			if gr.ObjInfo.VersionID != "" && gr.ObjInfo.DeleteMarker {
				w.Header()[xhttp.AmzVersionID] = []string{gr.ObjInfo.VersionID}
				w.Header()[xhttp.AmzDeleteMarker] = []string{strconv.FormatBool(gr.ObjInfo.DeleteMarker)}
			}
		}
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}
	defer gr.Close()
	srcInfo := gr.ObjInfo

	actualPartSize, err := srcInfo.GetActualSize()
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	if err := enforceBucketQuotaHard(ctx, dstBucket, actualPartSize); err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	// Special care for CopyObjectPart
	if partRangeErr := checkCopyPartRangeWithSize(rs, actualPartSize); partRangeErr != nil {
		writeCopyPartErr(ctx, w, partRangeErr, r.URL)
		return
	}

	// Get the object offset & length
	startOffset, length, err := rs.GetOffsetLength(actualPartSize)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	// maximum copy size for multipart objects in a single operation
	if isMaxAllowedPartSize(length) {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrEntityTooLarge), r.URL)
		return
	}

	if isRemoteCopyRequired(ctx, srcBucket, dstBucket, objectAPI) {
		var dstRecords []dns.SrvRecord
		dstRecords, err = globalDNSConfig.Get(dstBucket)
		if err != nil {
			writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
			return
		}

		// Send PutObject request to appropriate instance (in federated deployment)
		core, rerr := getRemoteInstanceClient(r, getHostFromSrv(dstRecords))
		if rerr != nil {
			writeErrorResponse(ctx, w, toAPIError(ctx, rerr), r.URL)
			return
		}

		partInfo, err := core.PutObjectPart(ctx, dstBucket, dstObject, uploadID, partID, gr, length, "", "", dstOpts.ServerSideEncryption)
		if err != nil {
			writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
			return
		}

		response := generateCopyObjectPartResponse(partInfo.ETag, partInfo.LastModified)
		encodedSuccessResponse := encodeResponse(response)

		// Write success response.
		writeSuccessResponseXML(w, encodedSuccessResponse)
		return
	}

	actualPartSize = length
	var reader io.Reader = etag.NewReader(gr, nil)

	mi, err := objectAPI.GetMultipartInfo(ctx, dstBucket, dstObject, uploadID, dstOpts)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	// Read compression metadata preserved in the init multipart for the decision.
	_, isCompressed := mi.UserDefined[ReservedMetadataPrefix+"compression"]
	// Compress only if the compression is enabled during initial multipart.
	if isCompressed {
		s2c := newS2CompressReader(reader, actualPartSize)
		defer s2c.Close()
		reader = etag.Wrap(s2c, reader)
		length = -1
	}

	srcInfo.Reader, err = hash.NewReader(reader, length, "", "", actualPartSize)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	dstOpts, err = copyDstOpts(ctx, r, dstBucket, dstObject, mi.UserDefined)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	rawReader := srcInfo.Reader
	pReader := NewPutObjReader(rawReader)

	_, isEncrypted := crypto.IsEncrypted(mi.UserDefined)
	var objectEncryptionKey crypto.ObjectKey
	if objectAPI.IsEncryptionSupported() && isEncrypted {
		if !crypto.SSEC.IsRequested(r.Header) && crypto.SSEC.IsEncrypted(mi.UserDefined) {
			writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrSSEMultipartEncrypted), r.URL)
			return
		}
		if crypto.S3.IsEncrypted(mi.UserDefined) && crypto.SSEC.IsRequested(r.Header) {
			writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrSSEMultipartEncrypted), r.URL)
			return
		}
		var key []byte
		if crypto.SSEC.IsRequested(r.Header) {
			key, err = ParseSSECustomerRequest(r)
			if err != nil {
				writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
				return
			}
		}
		key, err = decryptObjectInfo(key, dstBucket, dstObject, mi.UserDefined)
		if err != nil {
			writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
			return
		}
		copy(objectEncryptionKey[:], key)

		partEncryptionKey := objectEncryptionKey.DerivePartKey(uint32(partID))
		encReader, err := sio.EncryptReader(reader, sio.Config{Key: partEncryptionKey[:], CipherSuites: fips.CipherSuitesDARE()})
		if err != nil {
			writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
			return
		}
		reader = etag.Wrap(encReader, reader)

		wantSize := int64(-1)
		if length >= 0 {
			info := ObjectInfo{Size: length}
			wantSize = info.EncryptedSize()
		}

		srcInfo.Reader, err = hash.NewReader(reader, wantSize, "", "", actualPartSize)
		if err != nil {
			writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
			return
		}
		pReader, err = pReader.WithEncryption(srcInfo.Reader, &objectEncryptionKey)
		if err != nil {
			writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
			return
		}
	}

	srcInfo.PutObjReader = pReader
	copyObjectPart := objectAPI.CopyObjectPart
	if api.CacheAPI() != nil {
		copyObjectPart = api.CacheAPI().CopyObjectPart
	}
	// Copy source object to destination, if source and destination
	// object is same then only metadata is updated.
	partInfo, err := copyObjectPart(ctx, srcBucket, srcObject, dstBucket, dstObject, uploadID, partID,
		startOffset, length, srcInfo, srcOpts, dstOpts)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	if isEncrypted {
		partInfo.ETag = tryDecryptETag(objectEncryptionKey[:], partInfo.ETag, crypto.SSEC.IsRequested(r.Header))
	}

	response := generateCopyObjectPartResponse(partInfo.ETag, partInfo.LastModified)
	encodedSuccessResponse := encodeResponse(response)

	// Write success response.
	writeSuccessResponseXML(w, encodedSuccessResponse)
}

// PutObjectPartHandler - uploads an incoming part for an ongoing multipart operation.
func (api objectAPIHandlers) PutObjectPartHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "PutObjectPart")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	objectAPI := api.ObjectAPI()
	if objectAPI == nil {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrServerNotInitialized), r.URL)
		return
	}

	if _, ok := crypto.IsRequested(r.Header); ok {
		if globalIsGateway {
			if crypto.SSEC.IsRequested(r.Header) && !objectAPI.IsEncryptionSupported() {
				writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrNotImplemented), r.URL)
				return
			}
		} else {
			if !objectAPI.IsEncryptionSupported() {
				writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrNotImplemented), r.URL)
				return
			}
		}
	}

	vars := mux.Vars(r)
	bucket := vars["bucket"]
	object, err := unescapePath(vars["object"])
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	// X-Amz-Copy-Source shouldn't be set for this call.
	if _, ok := r.Header[xhttp.AmzCopySource]; ok {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrInvalidCopySource), r.URL)
		return
	}

	clientETag, err := etag.FromContentMD5(r.Header)
	if err != nil {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrInvalidDigest), r.URL)
		return
	}

	// if Content-Length is unknown/missing, throw away
	size := r.ContentLength

	rAuthType := getRequestAuthType(r)
	// For auth type streaming signature, we need to gather a different content length.
	if rAuthType == authTypeStreamingSigned {
		if sizeStr, ok := r.Header[xhttp.AmzDecodedContentLength]; ok {
			if sizeStr[0] == "" {
				writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrMissingContentLength), r.URL)
				return
			}
			size, err = strconv.ParseInt(sizeStr[0], 10, 64)
			if err != nil {
				writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
				return
			}
		}
	}
	if size == -1 {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrMissingContentLength), r.URL)
		return
	}

	// maximum Upload size for multipart objects in a single operation
	if isMaxAllowedPartSize(size) {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrEntityTooLarge), r.URL)
		return
	}

	uploadID := r.Form.Get(xhttp.UploadID)
	partIDString := r.Form.Get(xhttp.PartNumber)

	partID, err := strconv.Atoi(partIDString)
	if err != nil {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrInvalidPart), r.URL)
		return
	}

	// check partID with maximum part ID for multipart objects
	if isMaxPartID(partID) {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrInvalidMaxParts), r.URL)
		return
	}

	var (
		md5hex              = clientETag.String()
		sha256hex           = ""
		reader    io.Reader = r.Body
		s3Error   APIErrorCode
	)
	if s3Error = isPutActionAllowed(ctx, rAuthType, bucket, object, r, iampolicy.PutObjectAction); s3Error != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Error), r.URL)
		return
	}

	switch rAuthType {
	case authTypeStreamingSigned:
		// Initialize stream signature verifier.
		reader, s3Error = newSignV4ChunkedReader(r)
		if s3Error != ErrNone {
			writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Error), r.URL)
			return
		}
	case authTypeSignedV2, authTypePresignedV2:
		if s3Error = isReqAuthenticatedV2(r); s3Error != ErrNone {
			writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Error), r.URL)
			return
		}
	case authTypePresigned, authTypeSigned:
		if s3Error = reqSignatureV4Verify(r, globalSite.Region, serviceS3); s3Error != ErrNone {
			writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Error), r.URL)
			return
		}

		if !skipContentSha256Cksum(r) {
			sha256hex = getContentSha256Cksum(r, serviceS3)
		}
	}

	if err := enforceBucketQuotaHard(ctx, bucket, size); err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	actualSize := size

	// get encryption options
	var opts ObjectOptions
	if crypto.SSEC.IsRequested(r.Header) {
		opts, err = getOpts(ctx, r, bucket, object)
		if err != nil {
			writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
			return
		}
	}

	mi, err := objectAPI.GetMultipartInfo(ctx, bucket, object, uploadID, opts)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	// Read compression metadata preserved in the init multipart for the decision.
	_, isCompressed := mi.UserDefined[ReservedMetadataPrefix+"compression"]

	if objectAPI.IsCompressionSupported() && isCompressed {
		actualReader, err := hash.NewReader(reader, size, md5hex, sha256hex, actualSize)
		if err != nil {
			writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
			return
		}

		// Set compression metrics.
		s2c := newS2CompressReader(actualReader, actualSize)
		defer s2c.Close()
		reader = etag.Wrap(s2c, actualReader)
		size = -1   // Since compressed size is un-predictable.
		md5hex = "" // Do not try to verify the content.
		sha256hex = ""
	}

	hashReader, err := hash.NewReader(reader, size, md5hex, sha256hex, actualSize)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}
	rawReader := hashReader
	pReader := NewPutObjReader(rawReader)

	_, isEncrypted := crypto.IsEncrypted(mi.UserDefined)
	var objectEncryptionKey crypto.ObjectKey
	if objectAPI.IsEncryptionSupported() && isEncrypted {
		if !crypto.SSEC.IsRequested(r.Header) && crypto.SSEC.IsEncrypted(mi.UserDefined) {
			writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrSSEMultipartEncrypted), r.URL)
			return
		}

		opts, err = putOpts(ctx, r, bucket, object, mi.UserDefined)
		if err != nil {
			writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
			return
		}

		var key []byte
		if crypto.SSEC.IsRequested(r.Header) {
			key, err = ParseSSECustomerRequest(r)
			if err != nil {
				writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
				return
			}
		}

		// Calculating object encryption key
		key, err = decryptObjectInfo(key, bucket, object, mi.UserDefined)
		if err != nil {
			writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
			return
		}
		copy(objectEncryptionKey[:], key)

		partEncryptionKey := objectEncryptionKey.DerivePartKey(uint32(partID))
		in := io.Reader(hashReader)
		if size > encryptBufferThreshold {
			// The encryption reads in blocks of 64KB.
			// We add a buffer on bigger files to reduce the number of syscalls upstream.
			in = bufio.NewReaderSize(hashReader, encryptBufferSize)
		}
		reader, err = sio.EncryptReader(in, sio.Config{Key: partEncryptionKey[:], CipherSuites: fips.CipherSuitesDARE()})
		if err != nil {
			writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
			return
		}
		wantSize := int64(-1)
		if size >= 0 {
			info := ObjectInfo{Size: size}
			wantSize = info.EncryptedSize()
		}
		// do not try to verify encrypted content
		hashReader, err = hash.NewReader(etag.Wrap(reader, hashReader), wantSize, "", "", actualSize)
		if err != nil {
			writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
			return
		}
		pReader, err = pReader.WithEncryption(hashReader, &objectEncryptionKey)
		if err != nil {
			writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
			return
		}
	}

	putObjectPart := objectAPI.PutObjectPart
	if api.CacheAPI() != nil {
		putObjectPart = api.CacheAPI().PutObjectPart
	}

	partInfo, err := putObjectPart(ctx, bucket, object, uploadID, partID, pReader, opts)
	if err != nil {
		// Verify if the underlying error is signature mismatch.
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	etag := partInfo.ETag
	if kind, encrypted := crypto.IsEncrypted(mi.UserDefined); encrypted {
		switch kind {
		case crypto.S3KMS:
			w.Header().Set(xhttp.AmzServerSideEncryption, xhttp.AmzEncryptionKMS)
			w.Header().Set(xhttp.AmzServerSideEncryptionKmsID, mi.KMSKeyID())
			if kmsCtx, ok := mi.UserDefined[crypto.MetaContext]; ok {
				w.Header().Set(xhttp.AmzServerSideEncryptionKmsContext, kmsCtx)
			}
			if len(etag) >= 32 && strings.Count(etag, "-") != 1 {
				etag = etag[len(etag)-32:]
			}
		case crypto.S3:
			w.Header().Set(xhttp.AmzServerSideEncryption, xhttp.AmzEncryptionAES)
			etag, _ = DecryptETag(objectEncryptionKey, ObjectInfo{ETag: etag})
		case crypto.SSEC:
			w.Header().Set(xhttp.AmzServerSideEncryptionCustomerAlgorithm, r.Header.Get(xhttp.AmzServerSideEncryptionCustomerAlgorithm))
			w.Header().Set(xhttp.AmzServerSideEncryptionCustomerKeyMD5, r.Header.Get(xhttp.AmzServerSideEncryptionCustomerKeyMD5))

			if len(etag) >= 32 && strings.Count(etag, "-") != 1 {
				etag = etag[len(etag)-32:]
			}
		}
	}

	// We must not use the http.Header().Set method here because some (broken)
	// clients expect the ETag header key to be literally "ETag" - not "Etag" (case-sensitive).
	// Therefore, we have to set the ETag directly as map entry.
	w.Header()[xhttp.ETag] = []string{"\"" + etag + "\""}

	writeSuccessResponseHeadersOnly(w)
}

// AbortMultipartUploadHandler - Abort multipart upload
func (api objectAPIHandlers) AbortMultipartUploadHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "AbortMultipartUpload")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	vars := mux.Vars(r)
	bucket := vars["bucket"]
	object, err := unescapePath(vars["object"])
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	objectAPI := api.ObjectAPI()
	if objectAPI == nil {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrServerNotInitialized), r.URL)
		return
	}
	abortMultipartUpload := objectAPI.AbortMultipartUpload
	if api.CacheAPI() != nil {
		abortMultipartUpload = api.CacheAPI().AbortMultipartUpload
	}

	if s3Error := checkRequestAuthType(ctx, r, policy.AbortMultipartUploadAction, bucket, object); s3Error != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Error), r.URL)
		return
	}

	uploadID, _, _, _, s3Error := getObjectResources(r.Form)
	if s3Error != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Error), r.URL)
		return
	}
	opts := ObjectOptions{}
	if err := abortMultipartUpload(ctx, bucket, object, uploadID, opts); err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	writeSuccessNoContent(w)
}

// ListObjectPartsHandler - List object parts
func (api objectAPIHandlers) ListObjectPartsHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "ListObjectParts")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	vars := mux.Vars(r)
	bucket := vars["bucket"]
	object, err := unescapePath(vars["object"])
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	objectAPI := api.ObjectAPI()
	if objectAPI == nil {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrServerNotInitialized), r.URL)
		return
	}

	if s3Error := checkRequestAuthType(ctx, r, policy.ListMultipartUploadPartsAction, bucket, object); s3Error != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Error), r.URL)
		return
	}

	uploadID, partNumberMarker, maxParts, encodingType, s3Error := getObjectResources(r.Form)
	if s3Error != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Error), r.URL)
		return
	}
	if partNumberMarker < 0 {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrInvalidPartNumberMarker), r.URL)
		return
	}
	if maxParts < 0 {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrInvalidMaxParts), r.URL)
		return
	}

	opts := ObjectOptions{}
	listPartsInfo, err := objectAPI.ListObjectParts(ctx, bucket, object, uploadID, partNumberMarker, maxParts, opts)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	// We have to adjust the size of encrypted parts since encrypted parts
	// are slightly larger due to encryption overhead.
	// Further, we have to adjust the ETags of parts when using SSE-S3.
	// Due to AWS S3, SSE-S3 encrypted parts return the plaintext ETag
	// being the content MD5 of that particular part. This is not the
	// case for SSE-C and SSE-KMS objects.
	if kind, ok := crypto.IsEncrypted(listPartsInfo.UserDefined); ok && objectAPI.IsEncryptionSupported() {
		var objectEncryptionKey []byte
		if kind == crypto.S3 {
			objectEncryptionKey, err = decryptObjectInfo(nil, bucket, object, listPartsInfo.UserDefined)
			if err != nil {
				writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
				return
			}
		}
		for i, p := range listPartsInfo.Parts {
			listPartsInfo.Parts[i].ETag = tryDecryptETag(objectEncryptionKey, p.ETag, kind != crypto.S3)
			size, err := sio.DecryptedSize(uint64(p.Size))
			if err != nil {
				writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
				return
			}
			listPartsInfo.Parts[i].Size = int64(size)
		}
	}

	response := generateListPartsResponse(listPartsInfo, encodingType)
	encodedSuccessResponse := encodeResponse(response)

	// Write success response.
	writeSuccessResponseXML(w, encodedSuccessResponse)
}

type whiteSpaceWriter struct {
	http.ResponseWriter
	http.Flusher
	written bool
}

func (w *whiteSpaceWriter) Write(b []byte) (n int, err error) {
	n, err = w.ResponseWriter.Write(b)
	w.written = true
	return
}

func (w *whiteSpaceWriter) WriteHeader(statusCode int) {
	if !w.written {
		w.ResponseWriter.WriteHeader(statusCode)
	}
}

// Send empty whitespaces every 10 seconds to the client till completeMultiPartUpload() is
// done so that the client does not time out. Downside is we might send 200 OK and
// then send error XML. But accoording to S3 spec the client is supposed to check
// for error XML even if it received 200 OK. But for erasure this is not a problem
// as completeMultiPartUpload() is quick. Even For FS, it would not be an issue as
// we do background append as and when the parts arrive and completeMultiPartUpload
// is quick. Only in a rare case where parts would be out of order will
// FS:completeMultiPartUpload() take a longer time.
func sendWhiteSpace(w http.ResponseWriter) <-chan bool {
	doneCh := make(chan bool)
	go func() {
		ticker := time.NewTicker(time.Second * 10)
		headerWritten := false
		for {
			select {
			case <-ticker.C:
				// Write header if not written yet.
				if !headerWritten {
					_, err := w.Write([]byte(xml.Header))
					headerWritten = err == nil
				}

				// Once header is written keep writing empty spaces
				// which are ignored by client SDK XML parsers.
				// This occurs when server takes long time to completeMultiPartUpload()
				_, err := w.Write([]byte(" "))
				if err != nil {
					return
				}
				w.(http.Flusher).Flush()
			case doneCh <- headerWritten:
				ticker.Stop()
				return
			}
		}
	}()
	return doneCh
}

// CompleteMultipartUploadHandler - Complete multipart upload.
func (api objectAPIHandlers) CompleteMultipartUploadHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "CompleteMultipartUpload")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	vars := mux.Vars(r)
	bucket := vars["bucket"]
	object, err := unescapePath(vars["object"])
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	objectAPI := api.ObjectAPI()
	if objectAPI == nil {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrServerNotInitialized), r.URL)
		return
	}

	if s3Error := checkRequestAuthType(ctx, r, policy.PutObjectAction, bucket, object); s3Error != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Error), r.URL)
		return
	}

	// Content-Length is required and should be non-zero
	if r.ContentLength <= 0 {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrMissingContentLength), r.URL)
		return
	}

	// Get upload id.
	uploadID, _, _, _, s3Error := getObjectResources(r.Form)
	if s3Error != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Error), r.URL)
		return
	}

	complMultipartUpload := &CompleteMultipartUpload{}
	if err = xmlDecoder(r.Body, complMultipartUpload, r.ContentLength); err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}
	if len(complMultipartUpload.Parts) == 0 {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrMalformedXML), r.URL)
		return
	}
	if !sort.IsSorted(CompletedParts(complMultipartUpload.Parts)) {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrInvalidPartOrder), r.URL)
		return
	}

	// Reject retention or governance headers if set, CompleteMultipartUpload spec
	// does not use these headers, and should not be passed down to checkPutObjectLockAllowed
	if objectlock.IsObjectLockRequested(r.Header) || objectlock.IsObjectLockGovernanceBypassSet(r.Header) {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrInvalidRequest), r.URL)
		return
	}

	if _, _, _, s3Err := checkPutObjectLockAllowed(ctx, r, bucket, object, objectAPI.GetObjectInfo, ErrNone, ErrNone); s3Err != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Err), r.URL)
		return
	}

	completeMultiPartUpload := objectAPI.CompleteMultipartUpload
	if api.CacheAPI() != nil {
		completeMultiPartUpload = api.CacheAPI().CompleteMultipartUpload
	}
	// This code is specifically to handle the requirements for slow
	// complete multipart upload operations on FS mode.
	writeErrorResponseWithoutXMLHeader := func(ctx context.Context, w http.ResponseWriter, err APIError, reqURL *url.URL) {
		switch err.Code {
		case "SlowDown", "XMinioServerNotInitialized", "XMinioReadQuorum", "XMinioWriteQuorum":
			// Set retxry-after header to indicate user-agents to retry request after 120secs.
			// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Retry-After
			w.Header().Set(xhttp.RetryAfter, "120")
		}

		// Generate error response.
		errorResponse := getAPIErrorResponse(ctx, err, reqURL.Path,
			w.Header().Get(xhttp.AmzRequestID), globalDeploymentID)
		encodedErrorResponse, _ := xml.Marshal(errorResponse)
		setCommonHeaders(w)
		w.Header().Set(xhttp.ContentType, string(mimeXML))
		w.Write(encodedErrorResponse)
	}

	versioned := globalBucketVersioningSys.Enabled(bucket)
	suspended := globalBucketVersioningSys.Suspended(bucket)
	os := newObjSweeper(bucket, object).WithVersioning(versioned, suspended)
	if !globalTierConfigMgr.Empty() {
		// Get appropriate object info to identify the remote object to delete
		goiOpts := os.GetOpts()
		if goi, gerr := objectAPI.GetObjectInfo(ctx, bucket, object, goiOpts); gerr == nil {
			os.SetTransitionState(goi.TransitionedObject)
		}
	}

	setEventStreamHeaders(w)

	opts, err := completeMultipartOpts(ctx, r, bucket, object)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	// First, we compute the ETag of the multipart object.
	// The ETag of a multi-part object is always:
	//   ETag := MD5(ETag_p1, ETag_p2, ...)+"-N"   (N being the number of parts)
	//
	// This is independent of encryption. An encrypted multipart
	// object also has an ETag that is the MD5 of its part ETags.
	// The fact the in case of encryption the ETag of a part is
	// not the MD5 of the part content does not change that.
	var completeETags []etag.ETag
	for _, part := range complMultipartUpload.Parts {
		ETag, err := etag.Parse(part.ETag)
		if err != nil {
			continue
		}
		completeETags = append(completeETags, ETag)
	}
	multipartETag := etag.Multipart(completeETags...)
	opts.UserDefined["etag"] = multipartETag.String()

	// However, in case of encryption, the persisted part ETags don't match
	// what we have sent to the client during PutObjectPart. The reason is
	// that ETags are encrypted. Hence, the client will send a list of complete
	// part ETags of which non can match the ETag of any part. For example
	//   ETag (client):          30902184f4e62dd8f98f0aaff810c626
	//   ETag (server-internal): 20000f00ce5dc16e3f3b124f586ae1d88e9caa1c598415c2759bbb50e84a59f630902184f4e62dd8f98f0aaff810c626
	//
	// Therefore, we adjust all ETags sent by the client to match what is stored
	// on the backend.
	if objectAPI.IsEncryptionSupported() {
		mi, err := objectAPI.GetMultipartInfo(ctx, bucket, object, uploadID, ObjectOptions{})
		if err != nil {
			writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
			return
		}

		if _, ok := crypto.IsEncrypted(mi.UserDefined); ok {
			const MaxParts = 10000
			listPartsInfo, err := objectAPI.ListObjectParts(ctx, bucket, object, uploadID, 0, MaxParts, ObjectOptions{})
			if err != nil {
				writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
				return
			}
			sort.Slice(listPartsInfo.Parts, func(i, j int) bool {
				return listPartsInfo.Parts[i].PartNumber < listPartsInfo.Parts[j].PartNumber
			})
			sort.Slice(complMultipartUpload.Parts, func(i, j int) bool {
				return complMultipartUpload.Parts[i].PartNumber < complMultipartUpload.Parts[j].PartNumber
			})
			for i := range listPartsInfo.Parts {
				for j := range complMultipartUpload.Parts {
					if listPartsInfo.Parts[i].PartNumber == complMultipartUpload.Parts[j].PartNumber {
						complMultipartUpload.Parts[j].ETag = listPartsInfo.Parts[i].ETag
						continue
					}
				}
			}
		}
	}

	w = &whiteSpaceWriter{ResponseWriter: w, Flusher: w.(http.Flusher)}
	completeDoneCh := sendWhiteSpace(w)
	objInfo, err := completeMultiPartUpload(ctx, bucket, object, uploadID, complMultipartUpload.Parts, opts)
	// Stop writing white spaces to the client. Note that close(doneCh) style is not used as it
	// can cause white space to be written after we send XML response in a race condition.
	headerWritten := <-completeDoneCh
	if err != nil {
		if headerWritten {
			writeErrorResponseWithoutXMLHeader(ctx, w, toAPIError(ctx, err), r.URL)
		} else {
			writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		}
		return
	}

	// Get object location.
	location := getObjectLocation(r, globalDomainNames, bucket, object)
	// Generate complete multipart response.
	response := generateCompleteMultpartUploadResponse(bucket, object, location, objInfo.ETag)
	var encodedSuccessResponse []byte
	if !headerWritten {
		encodedSuccessResponse = encodeResponse(response)
	} else {
		encodedSuccessResponse, err = xml.Marshal(response)
		if err != nil {
			writeErrorResponseWithoutXMLHeader(ctx, w, toAPIError(ctx, err), r.URL)
			return
		}
	}

	if r.Header.Get(xMinIOExtract) == "true" && strings.HasSuffix(object, archiveExt) {
		opts := ObjectOptions{VersionID: objInfo.VersionID, MTime: objInfo.ModTime}
		if _, err := updateObjectMetadataWithZipInfo(ctx, objectAPI, bucket, object, opts); err != nil {
			writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
			return
		}
	}

	setPutObjHeaders(w, objInfo, false)
	if dsc := mustReplicate(ctx, bucket, object, getMustReplicateOptions(objInfo, replication.ObjectReplicationType, opts)); dsc.ReplicateAny() {
		scheduleReplication(ctx, objInfo.Clone(), objectAPI, dsc, replication.ObjectReplicationType)
	}
	if _, ok := r.Header[xhttp.MinIOSourceReplicationRequest]; ok {
		actualSize, _ := objInfo.GetActualSize()
		defer globalReplicationStats.UpdateReplicaStat(bucket, actualSize)
	}

	// Write success response.
	writeSuccessResponseXML(w, encodedSuccessResponse)

	// Notify object created event.
	sendEvent(eventArgs{
		EventName:    event.ObjectCreatedCompleteMultipartUpload,
		BucketName:   bucket,
		Object:       objInfo,
		ReqParams:    extractReqParams(r),
		RespElements: extractRespElements(w),
		UserAgent:    r.UserAgent(),
		Host:         handlers.GetSourceIP(r),
	})

	// Remove the transitioned object whose object version is being overwritten.
	if !globalTierConfigMgr.Empty() {
		// Schedule object for immediate transition if eligible.
		enqueueTransitionImmediate(objInfo)
		logger.LogIf(ctx, os.Sweep())
	}
}

// Delete objectAPIHandlers

// DeleteObjectHandler - delete an object
func (api objectAPIHandlers) DeleteObjectHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "DeleteObject")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	vars := mux.Vars(r)
	bucket := vars["bucket"]
	object, err := unescapePath(vars["object"])
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	objectAPI := api.ObjectAPI()
	if objectAPI == nil {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrServerNotInitialized), r.URL)
		return
	}

	if s3Error := checkRequestAuthType(ctx, r, policy.DeleteObjectAction, bucket, object); s3Error != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Error), r.URL)
		return
	}

	if globalDNSConfig != nil {
		_, err := globalDNSConfig.Get(bucket)
		if err != nil && err != dns.ErrNotImplemented {
			writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
			return
		}
	}

	opts, err := delOpts(ctx, r, bucket, object)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}
	var (
		goi  ObjectInfo
		gerr error
	)

	getObjectInfo := objectAPI.GetObjectInfo
	if api.CacheAPI() != nil {
		getObjectInfo = api.CacheAPI().GetObjectInfo
	}

	os := newObjSweeper(bucket, object).WithVersion(opts.VersionID).WithVersioning(opts.Versioned, opts.VersionSuspended)
	// Mutations of objects on versioning suspended buckets
	// affect its null version. Through opts below we select
	// the null version's remote object to delete if
	// transitioned.
	goiOpts := os.GetOpts()
	goi, gerr = getObjectInfo(ctx, bucket, object, goiOpts)
	if gerr == nil {
		os.SetTransitionState(goi.TransitionedObject)
	}

	dsc := checkReplicateDelete(ctx, bucket, ObjectToDelete{
		ObjectV: ObjectV{
			ObjectName: object,
			VersionID:  opts.VersionID,
		},
	}, goi, opts, gerr)
	if dsc.ReplicateAny() {
		opts.SetDeleteReplicationState(dsc, opts.VersionID)
	}

	vID := opts.VersionID
	if r.Header.Get(xhttp.AmzBucketReplicationStatus) == replication.Replica.String() {
		// check if replica has permission to be deleted.
		if apiErrCode := checkRequestAuthType(ctx, r, policy.ReplicateDeleteAction, bucket, object); apiErrCode != ErrNone {
			writeErrorResponse(ctx, w, errorCodes.ToAPIErr(apiErrCode), r.URL)
			return
		}
		opts.SetReplicaStatus(replication.Replica)
		if opts.VersionPurgeStatus().Empty() {
			// opts.VersionID holds delete marker version ID to replicate and not yet present on disk
			vID = ""
		}
	}

	apiErr := ErrNone
	if rcfg, _ := globalBucketObjectLockSys.Get(bucket); rcfg.LockEnabled {
		if opts.DeletePrefix {
			writeErrorResponse(ctx, w, toAPIError(ctx, errors.New("force-delete is forbidden in a locked-enabled bucket")), r.URL)
			return
		}
		if vID != "" {
			apiErr = enforceRetentionBypassForDelete(ctx, r, bucket, ObjectToDelete{
				ObjectV: ObjectV{
					ObjectName: object,
					VersionID:  vID,
				},
			}, goi, gerr)
			if apiErr != ErrNone && apiErr != ErrNoSuchKey {
				writeErrorResponse(ctx, w, errorCodes.ToAPIErr(apiErr), r.URL)
				return
			}
		}
	}

	if apiErr == ErrNoSuchKey {
		writeSuccessNoContent(w)
		return
	}

	deleteObject := objectAPI.DeleteObject
	if api.CacheAPI() != nil {
		deleteObject = api.CacheAPI().DeleteObject
	}

	// http://docs.aws.amazon.com/AmazonS3/latest/API/RESTObjectDELETE.html
	objInfo, err := deleteObject(ctx, bucket, object, opts)
	if err != nil {
		switch err.(type) {
		case BucketNotFound:
			// When bucket doesn't exist specially handle it.
			writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
			return
		}
	}

	if objInfo.Name == "" {
		writeSuccessNoContent(w)
		return
	}

	setPutObjHeaders(w, objInfo, true)
	writeSuccessNoContent(w)

	eventName := event.ObjectRemovedDelete
	if objInfo.DeleteMarker {
		eventName = event.ObjectRemovedDeleteMarkerCreated
	}

	// Notify object deleted event.
	sendEvent(eventArgs{
		EventName:    eventName,
		BucketName:   bucket,
		Object:       objInfo,
		ReqParams:    extractReqParams(r),
		RespElements: extractRespElements(w),
		UserAgent:    r.UserAgent(),
		Host:         handlers.GetSourceIP(r),
	})

	if dsc.ReplicateAny() {
		dmVersionID := ""
		versionID := ""
		if objInfo.DeleteMarker {
			dmVersionID = objInfo.VersionID
		} else {
			versionID = objInfo.VersionID
		}
		dobj := DeletedObjectReplicationInfo{
			DeletedObject: DeletedObject{
				ObjectName:            object,
				VersionID:             versionID,
				DeleteMarkerVersionID: dmVersionID,
				DeleteMarkerMTime:     DeleteMarkerMTime{objInfo.ModTime},
				DeleteMarker:          objInfo.DeleteMarker,
				ReplicationState:      objInfo.getReplicationState(dsc.String(), opts.VersionID, false),
			},
			Bucket: bucket,
		}
		scheduleReplicationDelete(ctx, dobj, objectAPI)
	}

	// Remove the transitioned object whose object version is being overwritten.
	if !globalTierConfigMgr.Empty() {
		os.Sweep()
	}
}

// PutObjectLegalHoldHandler - set legal hold configuration to object,
func (api objectAPIHandlers) PutObjectLegalHoldHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "PutObjectLegalHold")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	vars := mux.Vars(r)
	bucket := vars["bucket"]
	object, err := unescapePath(vars["object"])
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	objectAPI := api.ObjectAPI()
	if objectAPI == nil {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrServerNotInitialized), r.URL)
		return
	}

	// Check permissions to perform this legal hold operation
	if s3Err := checkRequestAuthType(ctx, r, policy.PutObjectLegalHoldAction, bucket, object); s3Err != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Err), r.URL)
		return
	}

	if _, err := objectAPI.GetBucketInfo(ctx, bucket); err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}
	if !hasContentMD5(r.Header) {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrMissingContentMD5), r.URL)
		return
	}

	if rcfg, _ := globalBucketObjectLockSys.Get(bucket); !rcfg.LockEnabled {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrInvalidBucketObjectLockConfiguration), r.URL)
		return
	}

	legalHold, err := objectlock.ParseObjectLegalHold(io.LimitReader(r.Body, r.ContentLength))
	if err != nil {
		apiErr := errorCodes.ToAPIErr(ErrMalformedXML)
		apiErr.Description = err.Error()
		writeErrorResponse(ctx, w, apiErr, r.URL)
		return
	}

	opts, err := getOpts(ctx, r, bucket, object)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	popts := ObjectOptions{
		MTime:     opts.MTime,
		VersionID: opts.VersionID,
		EvalMetadataFn: func(oi ObjectInfo) error {
			oi.UserDefined[strings.ToLower(xhttp.AmzObjectLockLegalHold)] = strings.ToUpper(string(legalHold.Status))
			oi.UserDefined[ReservedMetadataPrefixLower+ObjectLockLegalHoldTimestamp] = UTCNow().Format(time.RFC3339Nano)

			dsc := mustReplicate(ctx, bucket, object, getMustReplicateOptions(oi, replication.MetadataReplicationType, opts))
			if dsc.ReplicateAny() {
				oi.UserDefined[ReservedMetadataPrefixLower+ReplicationTimestamp] = UTCNow().Format(time.RFC3339Nano)
				oi.UserDefined[ReservedMetadataPrefixLower+ReplicationStatus] = dsc.PendingStatus()
			}
			return nil
		},
	}

	objInfo, err := objectAPI.PutObjectMetadata(ctx, bucket, object, popts)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	dsc := mustReplicate(ctx, bucket, object, getMustReplicateOptions(objInfo, replication.MetadataReplicationType, opts))
	if dsc.ReplicateAny() {
		scheduleReplication(ctx, objInfo.Clone(), objectAPI, dsc, replication.MetadataReplicationType)
	}

	writeSuccessResponseHeadersOnly(w)

	// Notify object event.
	sendEvent(eventArgs{
		EventName:    event.ObjectCreatedPutLegalHold,
		BucketName:   bucket,
		Object:       objInfo,
		ReqParams:    extractReqParams(r),
		RespElements: extractRespElements(w),
		UserAgent:    r.UserAgent(),
		Host:         handlers.GetSourceIP(r),
	})
}

// GetObjectLegalHoldHandler - get legal hold configuration to object,
func (api objectAPIHandlers) GetObjectLegalHoldHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "GetObjectLegalHold")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	vars := mux.Vars(r)
	bucket := vars["bucket"]
	object, err := unescapePath(vars["object"])
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	objectAPI := api.ObjectAPI()
	if objectAPI == nil {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrServerNotInitialized), r.URL)
		return
	}
	if s3Error := checkRequestAuthType(ctx, r, policy.GetObjectLegalHoldAction, bucket, object); s3Error != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Error), r.URL)
		return
	}

	getObjectInfo := objectAPI.GetObjectInfo
	if api.CacheAPI() != nil {
		getObjectInfo = api.CacheAPI().GetObjectInfo
	}

	if rcfg, _ := globalBucketObjectLockSys.Get(bucket); !rcfg.LockEnabled {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrInvalidBucketObjectLockConfiguration), r.URL)
		return
	}

	opts, err := getOpts(ctx, r, bucket, object)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	objInfo, err := getObjectInfo(ctx, bucket, object, opts)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	legalHold := objectlock.GetObjectLegalHoldMeta(objInfo.UserDefined)
	if legalHold.IsEmpty() {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrNoSuchObjectLockConfiguration), r.URL)
		return
	}

	writeSuccessResponseXML(w, encodeResponse(legalHold))
	// Notify object legal hold accessed via a GET request.
	sendEvent(eventArgs{
		EventName:    event.ObjectAccessedGetLegalHold,
		BucketName:   bucket,
		Object:       objInfo,
		ReqParams:    extractReqParams(r),
		RespElements: extractRespElements(w),
		UserAgent:    r.UserAgent(),
		Host:         handlers.GetSourceIP(r),
	})
}

// PutObjectRetentionHandler - set object hold configuration to object,
func (api objectAPIHandlers) PutObjectRetentionHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "PutObjectRetention")

	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	vars := mux.Vars(r)
	bucket := vars["bucket"]
	object, err := unescapePath(vars["object"])
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	objectAPI := api.ObjectAPI()
	if objectAPI == nil {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrServerNotInitialized), r.URL)
		return
	}

	cred, owner, s3Err := validateSignature(getRequestAuthType(r), r)
	if s3Err != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Err), r.URL)
		return
	}

	if _, err := objectAPI.GetBucketInfo(ctx, bucket); err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	if !hasContentMD5(r.Header) {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrMissingContentMD5), r.URL)
		return
	}

	if rcfg, _ := globalBucketObjectLockSys.Get(bucket); !rcfg.LockEnabled {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrInvalidBucketObjectLockConfiguration), r.URL)
		return
	}

	objRetention, err := objectlock.ParseObjectRetention(r.Body)
	if err != nil {
		apiErr := errorCodes.ToAPIErr(ErrMalformedXML)
		apiErr.Description = err.Error()
		writeErrorResponse(ctx, w, apiErr, r.URL)
		return
	}

	opts, err := getOpts(ctx, r, bucket, object)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	popts := ObjectOptions{
		MTime:     opts.MTime,
		VersionID: opts.VersionID,
		EvalMetadataFn: func(oi ObjectInfo) error {
			if err := enforceRetentionBypassForPut(ctx, r, oi, objRetention, cred, owner); err != nil {
				return err
			}
			if objRetention.Mode.Valid() {
				oi.UserDefined[strings.ToLower(xhttp.AmzObjectLockMode)] = string(objRetention.Mode)
				oi.UserDefined[strings.ToLower(xhttp.AmzObjectLockRetainUntilDate)] = objRetention.RetainUntilDate.UTC().Format(time.RFC3339)
			} else {
				oi.UserDefined[strings.ToLower(xhttp.AmzObjectLockMode)] = ""
				oi.UserDefined[strings.ToLower(xhttp.AmzObjectLockRetainUntilDate)] = ""
			}
			oi.UserDefined[ReservedMetadataPrefixLower+ObjectLockRetentionTimestamp] = UTCNow().Format(time.RFC3339Nano)
			dsc := mustReplicate(ctx, bucket, object, getMustReplicateOptions(oi, replication.MetadataReplicationType, opts))
			if dsc.ReplicateAny() {
				oi.UserDefined[ReservedMetadataPrefixLower+ReplicationTimestamp] = UTCNow().Format(time.RFC3339Nano)
				oi.UserDefined[ReservedMetadataPrefixLower+ReplicationStatus] = dsc.PendingStatus()
			}
			return nil
		},
	}

	objInfo, err := objectAPI.PutObjectMetadata(ctx, bucket, object, popts)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	dsc := mustReplicate(ctx, bucket, object, getMustReplicateOptions(objInfo, replication.MetadataReplicationType, opts))
	if dsc.ReplicateAny() {
		scheduleReplication(ctx, objInfo.Clone(), objectAPI, dsc, replication.MetadataReplicationType)
	}

	writeSuccessNoContent(w)
	// Notify object  event.
	sendEvent(eventArgs{
		EventName:    event.ObjectCreatedPutRetention,
		BucketName:   bucket,
		Object:       objInfo,
		ReqParams:    extractReqParams(r),
		RespElements: extractRespElements(w),
		UserAgent:    r.UserAgent(),
		Host:         handlers.GetSourceIP(r),
	})
}

// GetObjectRetentionHandler - get object retention configuration of object,
func (api objectAPIHandlers) GetObjectRetentionHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "GetObjectRetention")
	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	vars := mux.Vars(r)
	bucket := vars["bucket"]
	object, err := unescapePath(vars["object"])
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	objectAPI := api.ObjectAPI()
	if objectAPI == nil {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrServerNotInitialized), r.URL)
		return
	}
	if s3Error := checkRequestAuthType(ctx, r, policy.GetObjectRetentionAction, bucket, object); s3Error != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Error), r.URL)
		return
	}

	getObjectInfo := objectAPI.GetObjectInfo
	if api.CacheAPI() != nil {
		getObjectInfo = api.CacheAPI().GetObjectInfo
	}

	if rcfg, _ := globalBucketObjectLockSys.Get(bucket); !rcfg.LockEnabled {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrInvalidBucketObjectLockConfiguration), r.URL)
		return
	}

	opts, err := getOpts(ctx, r, bucket, object)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	objInfo, err := getObjectInfo(ctx, bucket, object, opts)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	retention := objectlock.GetObjectRetentionMeta(objInfo.UserDefined)
	if !retention.Mode.Valid() {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrNoSuchObjectLockConfiguration), r.URL)
		return
	}

	writeSuccessResponseXML(w, encodeResponse(retention))
	// Notify object retention accessed via a GET request.
	sendEvent(eventArgs{
		EventName:    event.ObjectAccessedGetRetention,
		BucketName:   bucket,
		Object:       objInfo,
		ReqParams:    extractReqParams(r),
		RespElements: extractRespElements(w),
		UserAgent:    r.UserAgent(),
		Host:         handlers.GetSourceIP(r),
	})
}

// GetObjectTaggingHandler - GET object tagging
func (api objectAPIHandlers) GetObjectTaggingHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "GetObjectTagging")
	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	vars := mux.Vars(r)
	bucket := vars["bucket"]
	object, err := unescapePath(vars["object"])
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	objAPI := api.ObjectAPI()
	if objAPI == nil {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrServerNotInitialized), r.URL)
		return
	}

	if !objAPI.IsTaggingSupported() {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrNotImplemented), r.URL)
		return
	}

	// Allow getObjectTagging if policy action is set.
	if s3Error := checkRequestAuthType(ctx, r, policy.GetObjectTaggingAction, bucket, object); s3Error != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Error), r.URL)
		return
	}

	opts, err := getOpts(ctx, r, bucket, object)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	// Get object tags
	tags, err := objAPI.GetObjectTags(ctx, bucket, object, opts)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	if opts.VersionID != "" {
		w.Header()[xhttp.AmzVersionID] = []string{opts.VersionID}
	}

	writeSuccessResponseXML(w, encodeResponse(tags))
}

// PutObjectTaggingHandler - PUT object tagging
func (api objectAPIHandlers) PutObjectTaggingHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "PutObjectTagging")
	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	vars := mux.Vars(r)
	bucket := vars["bucket"]
	object, err := unescapePath(vars["object"])
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	objAPI := api.ObjectAPI()
	if objAPI == nil {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrServerNotInitialized), r.URL)
		return
	}
	if !objAPI.IsTaggingSupported() {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrNotImplemented), r.URL)
		return
	}

	// Allow putObjectTagging if policy action is set
	if s3Error := checkRequestAuthType(ctx, r, policy.PutObjectTaggingAction, bucket, object); s3Error != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Error), r.URL)
		return
	}

	tags, err := tags.ParseObjectXML(io.LimitReader(r.Body, r.ContentLength))
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	opts, err := getOpts(ctx, r, bucket, object)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	objInfo, err := objAPI.GetObjectInfo(ctx, bucket, object, opts)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}
	tagsStr := tags.String()

	oi := objInfo.Clone()
	oi.UserTags = tagsStr
	dsc := mustReplicate(ctx, bucket, object, getMustReplicateOptions(oi, replication.MetadataReplicationType, opts))
	if dsc.ReplicateAny() {
		opts.UserDefined = make(map[string]string)
		opts.UserDefined[ReservedMetadataPrefixLower+ReplicationTimestamp] = UTCNow().Format(time.RFC3339Nano)
		opts.UserDefined[ReservedMetadataPrefixLower+ReplicationStatus] = dsc.PendingStatus()
		opts.UserDefined[ReservedMetadataPrefixLower+TaggingTimestamp] = UTCNow().Format(time.RFC3339Nano)
	}

	// Put object tags
	objInfo, err = objAPI.PutObjectTags(ctx, bucket, object, tagsStr, opts)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	if dsc.ReplicateAny() {
		scheduleReplication(ctx, objInfo.Clone(), objAPI, dsc, replication.MetadataReplicationType)
	}

	if objInfo.VersionID != "" {
		w.Header()[xhttp.AmzVersionID] = []string{objInfo.VersionID}
	}

	writeSuccessResponseHeadersOnly(w)

	sendEvent(eventArgs{
		EventName:    event.ObjectCreatedPutTagging,
		BucketName:   bucket,
		Object:       objInfo,
		ReqParams:    extractReqParams(r),
		RespElements: extractRespElements(w),
		UserAgent:    r.UserAgent(),
		Host:         handlers.GetSourceIP(r),
	})
}

// DeleteObjectTaggingHandler - DELETE object tagging
func (api objectAPIHandlers) DeleteObjectTaggingHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "DeleteObjectTagging")
	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))

	objAPI := api.ObjectAPI()
	if objAPI == nil {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrServerNotInitialized), r.URL)
		return
	}
	if !objAPI.IsTaggingSupported() {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrNotImplemented), r.URL)
		return
	}

	vars := mux.Vars(r)
	bucket := vars["bucket"]
	object, err := unescapePath(vars["object"])
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	// Allow deleteObjectTagging if policy action is set
	if s3Error := checkRequestAuthType(ctx, r, policy.DeleteObjectTaggingAction, bucket, object); s3Error != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Error), r.URL)
		return
	}

	opts, err := getOpts(ctx, r, bucket, object)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	oi, err := objAPI.GetObjectInfo(ctx, bucket, object, opts)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}
	dsc := mustReplicate(ctx, bucket, object, getMustReplicateOptions(oi, replication.MetadataReplicationType, opts))
	if dsc.ReplicateAny() {
		opts.UserDefined = make(map[string]string)
		opts.UserDefined[ReservedMetadataPrefixLower+ReplicationTimestamp] = UTCNow().Format(time.RFC3339Nano)
		opts.UserDefined[ReservedMetadataPrefixLower+ReplicationStatus] = dsc.PendingStatus()
	}

	oi, err = objAPI.DeleteObjectTags(ctx, bucket, object, opts)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	if dsc.ReplicateAny() {
		scheduleReplication(ctx, oi.Clone(), objAPI, dsc, replication.MetadataReplicationType)
	}

	if oi.VersionID != "" {
		w.Header()[xhttp.AmzVersionID] = []string{oi.VersionID}
	}
	writeSuccessNoContent(w)

	sendEvent(eventArgs{
		EventName:    event.ObjectCreatedDeleteTagging,
		BucketName:   bucket,
		Object:       oi,
		ReqParams:    extractReqParams(r),
		RespElements: extractRespElements(w),
		UserAgent:    r.UserAgent(),
		Host:         handlers.GetSourceIP(r),
	})
}

// RestoreObjectHandler - POST restore object handler.
// ----------
func (api objectAPIHandlers) PostRestoreObjectHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "PostRestoreObject")
	defer logger.AuditLog(ctx, w, r, mustGetClaimsFromToken(r))
	vars := mux.Vars(r)
	bucket := vars["bucket"]
	object, err := unescapePath(vars["object"])
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	// Fetch object stat info.
	objectAPI := api.ObjectAPI()
	if objectAPI == nil {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrServerNotInitialized), r.URL)
		return
	}

	getObjectInfo := objectAPI.GetObjectInfo
	if api.CacheAPI() != nil {
		getObjectInfo = api.CacheAPI().GetObjectInfo
	}

	// Check for auth type to return S3 compatible error.
	if s3Error := checkRequestAuthType(ctx, r, policy.RestoreObjectAction, bucket, object); s3Error != ErrNone {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(s3Error), r.URL)
		return
	}

	if r.ContentLength <= 0 {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrEmptyRequestBody), r.URL)
		return
	}
	opts, err := postRestoreOpts(ctx, r, bucket, object)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	objInfo, err := getObjectInfo(ctx, bucket, object, opts)
	if err != nil {
		writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
		return
	}

	if objInfo.TransitionedObject.Status != lifecycle.TransitionComplete {
		writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrInvalidObjectState), r.URL)
		return
	}

	rreq, err := parseRestoreRequest(io.LimitReader(r.Body, r.ContentLength))
	if err != nil {
		apiErr := errorCodes.ToAPIErr(ErrMalformedXML)
		apiErr.Description = err.Error()
		writeErrorResponse(ctx, w, apiErr, r.URL)
		return
	}
	// validate the request
	if err := rreq.validate(ctx, objectAPI); err != nil {
		apiErr := errorCodes.ToAPIErr(ErrMalformedXML)
		apiErr.Description = err.Error()
		writeErrorResponse(ctx, w, apiErr, r.URL)
		return
	}
	statusCode := http.StatusOK
	alreadyRestored := false
	if err == nil {
		if objInfo.RestoreOngoing && rreq.Type != SelectRestoreRequest {
			writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrObjectRestoreAlreadyInProgress), r.URL)
			return
		}
		if !objInfo.RestoreOngoing && !objInfo.RestoreExpires.IsZero() {
			statusCode = http.StatusAccepted
			alreadyRestored = true
		}
	}
	// set or upgrade restore expiry
	restoreExpiry := lifecycle.ExpectedExpiryTime(time.Now(), rreq.Days)
	metadata := cloneMSS(objInfo.UserDefined)

	// update self with restore metadata
	if rreq.Type != SelectRestoreRequest {
		objInfo.metadataOnly = true // Perform only metadata updates.
		metadata[xhttp.AmzRestoreExpiryDays] = strconv.Itoa(rreq.Days)
		metadata[xhttp.AmzRestoreRequestDate] = time.Now().UTC().Format(http.TimeFormat)
		if alreadyRestored {
			metadata[xhttp.AmzRestore] = completedRestoreObj(restoreExpiry).String()
		} else {
			metadata[xhttp.AmzRestore] = ongoingRestoreObj().String()
		}
		objInfo.UserDefined = metadata
		if _, err := objectAPI.CopyObject(GlobalContext, bucket, object, bucket, object, objInfo, ObjectOptions{
			VersionID: objInfo.VersionID,
		}, ObjectOptions{
			VersionID: objInfo.VersionID,
		}); err != nil {
			logger.LogIf(ctx, fmt.Errorf("Unable to update replication metadata for %s: %s", objInfo.VersionID, err))
			writeErrorResponse(ctx, w, errorCodes.ToAPIErr(ErrInvalidObjectState), r.URL)
			return
		}
		// for previously restored object, just update the restore expiry
		if alreadyRestored {
			return
		}
	}

	restoreObject := mustGetUUID()
	if rreq.OutputLocation.S3.BucketName != "" {
		w.Header()[xhttp.AmzRestoreOutputPath] = []string{pathJoin(rreq.OutputLocation.S3.BucketName, rreq.OutputLocation.S3.Prefix, restoreObject)}
	}
	w.WriteHeader(statusCode)
	// Notify object restore started via a POST request.
	sendEvent(eventArgs{
		EventName:  event.ObjectRestorePostInitiated,
		BucketName: bucket,
		Object:     objInfo,
		ReqParams:  extractReqParams(r),
		UserAgent:  r.UserAgent(),
		Host:       handlers.GetSourceIP(r),
	})
	// now process the restore in background
	go func() {
		rctx := GlobalContext
		if !rreq.SelectParameters.IsEmpty() {
			actualSize, err := objInfo.GetActualSize()
			if err != nil {
				writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
				return
			}

			objectRSC := s3select.NewObjectReadSeekCloser(
				func(offset int64) (io.ReadCloser, error) {
					rs := &HTTPRangeSpec{
						IsSuffixLength: false,
						Start:          offset,
						End:            -1,
					}
					return getTransitionedObjectReader(rctx, bucket, object, rs, r.Header,
						objInfo, ObjectOptions{VersionID: objInfo.VersionID})
				},
				actualSize,
			)

			if err = rreq.SelectParameters.Open(objectRSC); err != nil {
				if serr, ok := err.(s3select.SelectError); ok {
					encodedErrorResponse := encodeResponse(APIErrorResponse{
						Code:       serr.ErrorCode(),
						Message:    serr.ErrorMessage(),
						BucketName: bucket,
						Key:        object,
						Resource:   r.URL.Path,
						RequestID:  w.Header().Get(xhttp.AmzRequestID),
						HostID:     globalDeploymentID,
					})
					writeResponse(w, serr.HTTPStatusCode(), encodedErrorResponse, mimeXML)
				} else {
					writeErrorResponse(ctx, w, toAPIError(ctx, err), r.URL)
				}
				return
			}
			nr := httptest.NewRecorder()
			rw := logger.NewResponseWriter(nr)
			rw.LogErrBody = true
			rw.LogAllBody = true
			rreq.SelectParameters.Evaluate(rw)
			rreq.SelectParameters.Close()
			return
		}
		opts := ObjectOptions{
			Transition: TransitionOptions{
				RestoreRequest: rreq,
				RestoreExpiry:  restoreExpiry,
			},
			VersionID: objInfo.VersionID,
		}
		if err := objectAPI.RestoreTransitionedObject(rctx, bucket, object, opts); err != nil {
			logger.LogIf(ctx, err)
			return
		}

		// Notify object restore completed via a POST request.
		sendEvent(eventArgs{
			EventName:  event.ObjectRestorePostCompleted,
			BucketName: bucket,
			Object:     objInfo,
			ReqParams:  extractReqParams(r),
			UserAgent:  r.UserAgent(),
			Host:       handlers.GetSourceIP(r),
		})
	}()
}
