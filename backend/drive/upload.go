// Upload for drive
//
// Docs
// Resumable upload: https://developers.google.com/drive/web/manage-uploads#resumable
// Best practices: https://developers.google.com/drive/web/manage-uploads#best-practices
// Files insert: https://developers.google.com/drive/v2/reference/files/insert
// Files update: https://developers.google.com/drive/v2/reference/files/update
//
// This contains code adapted from google.golang.org/api (C) the GO AUTHORS

package drive

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/valyala/bytebufferpool"
	"io"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/lib/readers"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"

	"github.com/go-redis/redis/v8"
)

const (
	// statusResumeIncomplete is the code returned by the Google uploader when the transfer is not yet complete.
	statusResumeIncomplete = 308
)

// resumableUpload is used by the generated APIs to provide resumable uploads.
// It is not used by developers directly.
type resumableUpload struct {
	f      *Fs
	remote string
	// URI is the resumable resource destination provided by the server after specifying "&uploadType=resumable".
	URI string
	// Media is the object being uploaded.
	Media io.Reader
	// MediaType defines the media type, e.g. "image/jpeg".
	MediaType string
	// ContentLength is the full size of the object being uploaded.
	ContentLength int64
	// Return value
	ret *drive.File
}

var RedisClient *redis.Client
var RedisKey string

type _Empty struct{}

func init() {
	RedisClient = redis.NewClient(
		&redis.Options{
			Addr:     "localhost:6379",
			Username: "rclone",
			Password: "rclone",
			DB:       0,
		})
	pkgs := strings.Split(reflect.TypeOf(_Empty{}).PkgPath(), "/")
	RedisKey = fmt.Sprintf("rclone-%s-streams", pkgs[len(pkgs)-1])
}

// Upload the io.Reader in of size bytes with contentType and info
func (f *Fs) Upload(ctx context.Context, in io.Reader, size int64, contentType, fileID, remote string, info *drive.File) (*drive.File, error) {
	params := url.Values{
		"alt":        {"json"},
		"uploadType": {"resumable"},
		"fields":     {partialFields},
	}
	params.Set("supportsAllDrives", "true")
	if f.opt.KeepRevisionForever {
		params.Set("keepRevisionForever", "true")
	}
	urls := "https://www.googleapis.com/upload/drive/v3/files"
	method := "POST"
	if fileID != "" {
		params.Set("setModifiedDate", "true")
		urls += "/{fileId}"
		method = "PATCH"
	}
	urls += "?" + params.Encode()
	var res *http.Response
	var err error
	err = f.pacer.Call(func() (bool, error) {
		var body io.Reader
		body, err = googleapi.WithoutDataWrapper.JSONReader(info)
		if err != nil {
			return false, err
		}
		var req *http.Request
		req, err = http.NewRequest(method, urls, body)
		if err != nil {
			return false, err
		}
		req = req.WithContext(ctx) // go1.13 can use NewRequestWithContext
		googleapi.Expand(req.URL, map[string]string{
			"fileId": fileID,
		})
		req.Header.Set("Content-Type", "application/json; charset=UTF-8")
		req.Header.Set("X-Upload-Content-Type", contentType)
		if size >= 0 {
			req.Header.Set("X-Upload-Content-Length", fmt.Sprintf("%v", size))
		}
		req.Header.Set("x-goog-api-client", "gl-go/1.13.8 gdcl/20200721")
		res, err = f.client.Do(req)
		if err == nil {
			defer googleapi.CloseBody(res)
			err = googleapi.CheckResponse(res)
		}
		return f.shouldRetry(err)
	})
	if err != nil {
		return nil, err
	}
	loc := res.Header.Get("Location")
	rx := &resumableUpload{
		f:             f,
		remote:        remote,
		URI:           loc,
		Media:         in,
		MediaType:     contentType,
		ContentLength: size,
	}
	go func() {
		ret := RedisClient.HSet(context.Background(), RedisKey, loc, fmt.Sprintf("uploading-pid%d", os.Getpid()))
		id, err := ret.Result()
		fs.Debugf(rx.remote, "Got redis ret: %v %v", id, err)
	}()
	defer func() {
		go RedisClient.HSet(context.Background(), RedisKey, loc, "finished")
	}()
	return rx.Upload(ctx)
}

// Make an http.Request for the range passed in
func (rx *resumableUpload) makeRequest(ctx context.Context, start int64, body io.ReadSeeker, reqSize int64) *http.Request {
	req, _ := http.NewRequest("POST", rx.URI, body)
	req = req.WithContext(ctx) // go1.13 can use NewRequestWithContext
	req.ContentLength = reqSize
	totalSize := "*"
	if rx.ContentLength >= 0 {
		totalSize = strconv.FormatInt(rx.ContentLength, 10)
	}
	if reqSize != 0 {
		req.Header.Set("Content-Range", fmt.Sprintf("bytes %v-%v/%v", start, start+reqSize-1, totalSize))
	} else {
		req.Header.Set("Content-Range", fmt.Sprintf("bytes */%v", totalSize))
	}
	req.Header.Set("Content-Type", rx.MediaType)
	return req
}

// Transfer a chunk - caller must call googleapi.CloseBody(res) if err == nil || res != nil
func (rx *resumableUpload) transferChunk(ctx context.Context, start int64, chunk io.ReadSeeker, chunkSize int64) (int, error) {
	_, _ = chunk.Seek(0, io.SeekStart)
	req := rx.makeRequest(ctx, start, chunk, chunkSize)
	res, err := rx.f.client.Do(req)
	if err != nil {
		return 599, err
	}
	defer googleapi.CloseBody(res)
	if res.StatusCode == statusResumeIncomplete {
		return res.StatusCode, nil
	}
	err = googleapi.CheckResponse(res)
	if err != nil {
		return res.StatusCode, err
	}

	// When the entire file upload is complete, the server
	// responds with an HTTP 201 Created along with any metadata
	// associated with this resource. I	f this request had been
	// updating an existing entity rather than creating a new one,
	// the HTTP response code for a completed upload would have
	// been 200 OK.
	//
	// So parse the response out of the body.  We aren't expecting
	// any other 2xx codes, so we parse it unconditionally on
	// StatusCode
	if err = json.NewDecoder(res.Body).Decode(&rx.ret); err != nil {
		return 598, err
	}

	return res.StatusCode, nil
}

var bufPool bytebufferpool.Pool
var OpenedFiles sync.Map

// Upload uploads the chunks from the input
// It retries each chunk using the pacer and --low-level-retries
func (rx *resumableUpload) Upload(ctx context.Context) (*drive.File, error) {
	start := int64(0)
	var StatusCode int
	var err error
	overtime := 0
	curChunkSize := 8 * 1024 * 1024
	//curChunkSize := 256 * 1024
	buf := make([]byte, curChunkSize)

	OpenedFiles.Store(rx.remote, rx.Media)
	defer OpenedFiles.Delete(rx.remote)

	for finished := false; !finished; {
		var reqSize int64
		var chunk io.ReadSeeker
		if rx.ContentLength >= 0 {
			// If size known use repeatable reader for smoother bwlimit
			if start >= rx.ContentLength {
				break
			}
			reqSize = rx.ContentLength - start
			if reqSize >= int64(rx.f.opt.ChunkSize) {
				reqSize = int64(rx.f.opt.ChunkSize)
			}
			chunk = readers.NewRepeatableLimitReaderBuffer(rx.Media, buf, reqSize)
		} else {
			// If size unknown read into buffer
			var n int

			n, err = func(r io.Reader, buf []byte) (n int, err error) {
				s := time.Now()
				var nn int
				for n < len(buf) && err == nil {
					e := n + int(minChunkSize)
					if e > len(buf) {
						e = len(buf)
					}
					nn, err = readers.ReadFill(r, buf[n:e])
					n += nn
					if n > len(buf)/2 && time.Now().Sub(s) > time.Second*6 {
						break
					}
				}
				return n, err
			}(rx.Media, buf)
			//n, err = readers.ReadFill(rx.Media, buf)
			if err != nil {
				if err != io.EOF {
					fs.Errorf(rx.remote, "Error reading: %s", err)
				}
				// Send the last chunk with the correct ContentLength
				// otherwise Google doesn't know we've finished
				rx.ContentLength = start + int64(n)
				finished = true
			}
			reqSize = int64(n)
			chunk = bytes.NewReader(buf[:reqSize])
			if n > curChunkSize-262144*3 {
				overtime++
			} else {
				if overtime > -5 {
					overtime--
				}
			}
			if overtime > 2 {
				if curChunkSize < int(rx.f.opt.ChunkSize) {
					curChunkSize += 1 * 1024 * 1024
					buf = make([]byte, curChunkSize)
				}
				overtime = 0
			} else if overtime < -3 {
				if curChunkSize > 4*1024*1024 {
					curChunkSize -= 1 * 1024 * 1024
					buf = make([]byte, curChunkSize)
				}
				overtime = 0
			}
		}

		// Transfer the chunk
		err = rx.f.pacer.Call(func() (bool, error) {
			fs.Debugf(rx.remote, "Sending chunk %d length %d", start, reqSize)
			s := time.Now()
			StatusCode, err = rx.transferChunk(ctx, start, chunk, reqSize)
			usedTime := time.Now().Sub(s)
			f := fs.Debugf
			if usedTime > time.Millisecond*1200 {
				f = fs.Infof
			}
			f(rx.remote, "Sent chunk %d length %d in time %s", start, reqSize, usedTime)
			again, err := rx.f.shouldRetry(err)
			if StatusCode == statusResumeIncomplete || StatusCode == http.StatusCreated || StatusCode == http.StatusOK {
				again = false
				err = nil
			}
			return again, err
		})
		if err != nil {
			return nil, err
		}

		start += reqSize
	}
	// Resume or retry uploads that fail due to connection interruptions or
	// any 5xx errors, including:
	//
	// 500 Internal Server Error
	// 502 Bad Gateway
	// 503 Service Unavailable
	// 504 Gateway Timeout
	//
	// Use an exponential backoff strategy if any 5xx server error is
	// returned when resuming or retrying upload requests. These errors can
	// occur if a server is getting overloaded. Exponential backoff can help
	// alleviate these kinds of problems during periods of high volume of
	// requests or heavy network traffic.  Other kinds of requests should not
	// be handled by exponential backoff but you can still retry a number of
	// them. When retrying these requests, limit the number of times you
	// retry them. For example your code could limit to ten retries or less
	// before reporting an error.
	//
	// Handle 404 Not Found errors when doing resumable uploads by starting
	// the entire upload over from the beginning.
	if rx.ret == nil {
		return nil, fserrors.RetryErrorf("Incomplete upload - retry, last error %d", StatusCode)
	}
	return rx.ret, nil
}
