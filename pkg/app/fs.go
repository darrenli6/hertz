/*
 * Copyright 2022 CloudWeGo Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * The MIT License (MIT)
 *
 * Copyright (c) 2015-present Aliaksandr Valialkin, VertaMedia, Kirill Danshin, Erik Dubbelboer, FastHTTP Authors
 *
 * Permission is hereby granted, free of charge, to any person obtaining a copy
 * of this software and associated documentation files (the "Software"), to deal
 * in the Software without restriction, including without limitation the rights
 * to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
 * copies of the Software, and to permit persons to whom the Software is
 * furnished to do so, subject to the following conditions:
 *
 * The above copyright notice and this permission notice shall be included in
 * all copies or substantial portions of the Software.
 *
 * THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
 * IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
 * FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
 * AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
 * LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
 * OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
 * THE SOFTWARE.
 *
 * This file may have been modified by CloudWeGo authors. All CloudWeGo
 * Modifications are Copyright 2022 CloudWeGo Authors.
 */

package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"html"
	"io"
	"io/ioutil"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/hertz/internal/bytesconv"
	"github.com/cloudwego/hertz/internal/bytestr"
	"github.com/cloudwego/hertz/internal/nocopy"
	"github.com/cloudwego/hertz/pkg/common/bytebufferpool"
	"github.com/cloudwego/hertz/pkg/common/compress"
	"github.com/cloudwego/hertz/pkg/common/errors"
	"github.com/cloudwego/hertz/pkg/common/hlog"
	"github.com/cloudwego/hertz/pkg/common/utils"
	"github.com/cloudwego/hertz/pkg/network"
	"github.com/cloudwego/hertz/pkg/protocol"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
)

var (
	errDirIndexRequired   = errors.NewPublic("directory index required")
	errNoCreatePermission = errors.NewPublic("no 'create file' permissions")

	rootFSOnce sync.Once
	rootFS     = &FS{
		Root:               "/",
		GenerateIndexPages: true,
		Compress:           true,
		AcceptByteRange:    true,
	}
	rootFSHandler  HandlerFunc
	strInvalidHost = []byte("invalid-host")
)

// PathRewriteFunc must return new request path based on arbitrary ctx
// info such as ctx.Path().
//
// Path rewriter is used in FS for translating the current request
// to the local filesystem path relative to FS.Root.
//
// The returned path must not contain '/../' substrings due to security reasons,
// since such paths may refer files outside FS.Root.
//
// The returned path may refer to ctx members. For example, ctx.Path().
type PathRewriteFunc func(ctx *RequestContext) []byte

// FS represents settings for request handler serving static files
// from the local filesystem.
//
// It is prohibited copying FS values. Create new values instead.
type FS struct {
	noCopy nocopy.NoCopy //lint:ignore U1000 until noCopy is used

	// Path to the root directory to serve files from.
	Root string

	// List of index file names to try opening during directory access.
	//
	// For example:
	//
	//     * index.html
	//     * index.htm
	//     * my-super-index.xml
	//
	// By default the list is empty.
	IndexNames []string

	// Index pages for directories without files matching IndexNames
	// are automatically generated if set.
	//
	// Directory index generation may be quite slow for directories
	// with many files (more than 1K), so it is discouraged enabling
	// index pages' generation for such directories.
	//
	// By default index pages aren't generated.
	GenerateIndexPages bool

	// Transparently compresses responses if set to true.
	//
	// The server tries minimizing CPU usage by caching compressed files.
	// It adds CompressedFileSuffix suffix to the original file name and
	// tries saving the resulting compressed file under the new file name.
	// So it is advisable to give the server write access to Root
	// and to all inner folders in order to minimize CPU usage when serving
	// compressed responses.
	//
	// Transparent compression is disabled by default.
	Compress bool

	// Enables byte range requests if set to true.
	//
	// Byte range requests are disabled by default.
	AcceptByteRange bool

	// Path rewriting function.
	//
	// By default request path is not modified.
	PathRewrite PathRewriteFunc

	// PathNotFound fires when file is not found in filesystem
	// this functions tries to replace "Cannot open requested path"
	// server response giving to the programmer the control of server flow.
	//
	// By default PathNotFound returns
	// "Cannot open requested path"
	PathNotFound HandlerFunc

	// Expiration duration for inactive file handlers.
	//
	// FSHandlerCacheDuration is used by default.
	CacheDuration time.Duration

	// Suffix to add to the name of cached compressed file.
	//
	// This value has sense only if Compress is set.
	//
	// FSCompressedFileSuffix is used by default.
	CompressedFileSuffix string

	once sync.Once
	h    HandlerFunc
}

type byteRangeUpdater interface {
	UpdateByteRange(startPos, endPos int) error
}

type fsSmallFileReader struct {
	ff       *fsFile
	startPos int
	endPos   int
}

func (r *fsSmallFileReader) Close() error {
	ff := r.ff
	ff.decReadersCount()
	r.ff = nil
	r.startPos = 0
	r.endPos = 0
	ff.h.smallFileReaderPool.Put(r)
	return nil
}

func (r *fsSmallFileReader) UpdateByteRange(startPos, endPos int) error {
	r.startPos = startPos
	r.endPos = endPos + 1
	return nil
}

func (r *fsSmallFileReader) Read(p []byte) (int, error) {
	tailLen := r.endPos - r.startPos
	if tailLen <= 0 {
		return 0, io.EOF
	}
	if len(p) > tailLen {
		p = p[:tailLen]
	}

	ff := r.ff
	if ff.f != nil {
		n, err := ff.f.ReadAt(p, int64(r.startPos))
		r.startPos += n
		return n, err
	}

	n := copy(p, ff.dirIndex[r.startPos:])
	r.startPos += n
	return n, nil
}

func (r *fsSmallFileReader) WriteTo(w io.Writer) (int64, error) {
	ff := r.ff

	var n int
	var err error
	if ff.f == nil {
		n, err = w.Write(ff.dirIndex[r.startPos:r.endPos])
		return int64(n), err
	}

	if rf, ok := w.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}

	curPos := r.startPos
	bufv := utils.CopyBufPool.Get()
	buf := bufv.([]byte)
	for err == nil {
		tailLen := r.endPos - curPos
		if tailLen <= 0 {
			break
		}
		if len(buf) > tailLen {
			buf = buf[:tailLen]
		}
		n, err = ff.f.ReadAt(buf, int64(curPos))
		nw, errw := w.Write(buf[:n])
		curPos += nw
		if errw == nil && nw != n {
			panic("BUG: Write(p) returned (n, nil), where n != len(p)")
		}
		if err == nil {
			err = errw
		}
	}
	utils.CopyBufPool.Put(bufv)

	if err == io.EOF {
		err = nil
	}
	return int64(curPos - r.startPos), err
}

// ServeFile returns HTTP response containing compressed file contents
// from the given path.
//
// HTTP response may contain uncompressed file contents in the following cases:
//
//   - Missing 'Accept-Encoding: gzip' request header.
//   - No write access to directory containing the file.
//
// Directory contents is returned if path points to directory.
//
// Use ServeFileUncompressed is you don't need serving compressed file contents.
//
// See also RequestCtx.SendFile.
func ServeFile(ctx *RequestContext, path string) {
	rootFSOnce.Do(func() {
		rootFSHandler = rootFS.NewRequestHandler()
	})
	if len(path) == 0 || path[0] != '/' {
		// extend relative path to absolute path
		var err error
		if path, err = filepath.Abs(path); err != nil {
			hlog.SystemLogger().Errorf("Cannot resolve path=%q to absolute file error=%s", path, err)
			ctx.AbortWithMsg("Internal Server Error", consts.StatusInternalServerError)
			return
		}
	}
	ctx.Request.SetRequestURI(path)
	rootFSHandler(context.Background(), ctx)
}

// NewRequestHandler returns new request handler with the given FS settings.
//
// The returned handler caches requested file handles
// for FS.CacheDuration.
// Make sure your program has enough 'max open files' limit aka
// 'ulimit -n' if FS.Root folder contains many files.
//
// Do not create multiple request handlers from a single FS instance -
// just reuse a single request handler.
func (fs *FS) NewRequestHandler() HandlerFunc {
	fs.once.Do(fs.initRequestHandler)
	return fs.h
}

func (fs *FS) initRequestHandler() {
	root := fs.Root

	// serve files from the current working directory if root is empty
	if len(root) == 0 {
		root = "."
	}

	// strip trailing slashes from the root path
	for len(root) > 0 && root[len(root)-1] == '/' {
		root = root[:len(root)-1]
	}

	cacheDuration := fs.CacheDuration
	if cacheDuration <= 0 {
		cacheDuration = consts.FSHandlerCacheDuration
	}
	compressedFileSuffix := fs.CompressedFileSuffix
	if len(compressedFileSuffix) == 0 {
		compressedFileSuffix = consts.FSCompressedFileSuffix
	}

	h := &fsHandler{
		root:                 root,
		indexNames:           fs.IndexNames,
		pathRewrite:          fs.PathRewrite,
		generateIndexPages:   fs.GenerateIndexPages,
		compress:             fs.Compress,
		pathNotFound:         fs.PathNotFound,
		acceptByteRange:      fs.AcceptByteRange,
		cacheDuration:        cacheDuration,
		compressedFileSuffix: compressedFileSuffix,
		cache:                make(map[string]*fsFile),
		compressedCache:      make(map[string]*fsFile),
	}

	go func() {
		var pendingFiles []*fsFile
		for {
			time.Sleep(cacheDuration / 2)
			pendingFiles = h.cleanCache(pendingFiles)
		}
	}()

	fs.h = h.handleRequest
}

type fsHandler struct {
	root                 string
	indexNames           []string
	pathRewrite          PathRewriteFunc
	pathNotFound         HandlerFunc
	generateIndexPages   bool
	compress             bool
	acceptByteRange      bool
	cacheDuration        time.Duration
	compressedFileSuffix string

	cache           map[string]*fsFile
	compressedCache map[string]*fsFile
	cacheLock       sync.Mutex

	smallFileReaderPool sync.Pool
}

// bigFileReader attempts to trigger sendfile
// for sending big files over the wire.
type bigFileReader struct {
	f  *os.File
	ff *fsFile
	r  io.Reader
	lr io.LimitedReader
}

func (r *bigFileReader) UpdateByteRange(startPos, endPos int) error {
	if _, err := r.f.Seek(int64(startPos), 0); err != nil {
		return err
	}
	r.r = &r.lr
	r.lr.R = r.f
	r.lr.N = int64(endPos - startPos + 1)
	return nil
}

func (r *bigFileReader) Read(p []byte) (int, error) {
	return r.r.Read(p)
}

func (r *bigFileReader) WriteTo(w io.Writer) (int64, error) {
	if rf, ok := w.(io.ReaderFrom); ok {
		// fast path. Sendfile must be triggered
		return rf.ReadFrom(r.r)
	}
	zw := network.NewWriter(w)
	// slow pathw
	return utils.CopyZeroAlloc(zw, r.r)
}

func (r *bigFileReader) Close() error {
	r.r = r.f
	n, err := r.f.Seek(0, 0)
	if err == nil {
		if n != 0 {
			panic("BUG: File.Seek(0,0) returned (non-zero, nil)")
		}

		ff := r.ff
		ff.bigFilesLock.Lock()
		ff.bigFiles = append(ff.bigFiles, r)
		ff.bigFilesLock.Unlock()
	} else {
		r.f.Close()
	}
	r.ff.decReadersCount()
	return err
}

func (h *fsHandler) cleanCache(pendingFiles []*fsFile) []*fsFile {
	var filesToRelease []*fsFile

	h.cacheLock.Lock()

	// Close files which couldn't be closed before due to non-zero
	// readers count on the previous run.
	var remainingFiles []*fsFile
	for _, ff := range pendingFiles {
		if ff.readersCount > 0 {
			remainingFiles = append(remainingFiles, ff)
		} else {
			filesToRelease = append(filesToRelease, ff)
		}
	}
	pendingFiles = remainingFiles

	pendingFiles, filesToRelease = cleanCacheNolock(h.cache, pendingFiles, filesToRelease, h.cacheDuration)
	pendingFiles, filesToRelease = cleanCacheNolock(h.compressedCache, pendingFiles, filesToRelease, h.cacheDuration)

	h.cacheLock.Unlock()

	for _, ff := range filesToRelease {
		ff.Release()
	}

	return pendingFiles
}

func (h *fsHandler) compressAndOpenFSFile(filePath string) (*fsFile, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}

	fileInfo, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("cannot obtain info for file %q: %s", filePath, err)
	}

	if fileInfo.IsDir() {
		f.Close()
		return nil, errDirIndexRequired
	}

	if strings.HasSuffix(filePath, h.compressedFileSuffix) ||
		fileInfo.Size() > consts.FsMaxCompressibleFileSize ||
		!isFileCompressible(f, consts.FsMinCompressRatio) {
		return h.newFSFile(f, fileInfo, false)
	}

	compressedFilePath := filePath + h.compressedFileSuffix
	absPath, err := filepath.Abs(compressedFilePath)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("cannot determine absolute path for %q: %s", compressedFilePath, err)
	}

	flock := getFileLock(absPath)
	flock.Lock()
	ff, err := h.compressFileNolock(f, fileInfo, filePath, compressedFilePath)
	flock.Unlock()

	return ff, err
}

func (h *fsHandler) newCompressedFSFile(filePath string) (*fsFile, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("cannot open compressed file %q: %s", filePath, err)
	}
	fileInfo, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("cannot obtain info for compressed file %q: %s", filePath, err)
	}
	return h.newFSFile(f, fileInfo, true)
}

func (h *fsHandler) compressFileNolock(f *os.File, fileInfo os.FileInfo, filePath, compressedFilePath string) (*fsFile, error) {
	// Attempt to open compressed file created by another concurrent
	// goroutine.
	// It is safe opening such a file, since the file creation
	// is guarded by file mutex - see getFileLock call.
	if _, err := os.Stat(compressedFilePath); err == nil {
		f.Close()
		return h.newCompressedFSFile(compressedFilePath)
	}

	// Create temporary file, so concurrent goroutines don't use
	// it until it is created.
	tmpFilePath := compressedFilePath + ".tmp"
	zf, err := os.Create(tmpFilePath)
	if err != nil {
		f.Close()
		if !os.IsPermission(err) {
			return nil, fmt.Errorf("cannot create temporary file %q: %s", tmpFilePath, err)
		}
		return nil, errNoCreatePermission
	}

	zw := compress.AcquireStacklessGzipWriter(zf, compress.CompressDefaultCompression)
	zrw := network.NewWriter(zw)
	_, err = utils.CopyZeroAlloc(zrw, f)
	if err1 := zw.Flush(); err == nil {
		err = err1
	}
	compress.ReleaseStacklessGzipWriter(zw, compress.CompressDefaultCompression)
	zf.Close()
	f.Close()
	if err != nil {
		return nil, fmt.Errorf("error when compressing file %q to %q: %s", filePath, tmpFilePath, err)
	}
	if err = os.Chtimes(tmpFilePath, time.Now(), fileInfo.ModTime()); err != nil {
		return nil, fmt.Errorf("cannot change modification time to %s for tmp file %q: %s",
			fileInfo.ModTime(), tmpFilePath, err)
	}
	if err = os.Rename(tmpFilePath, compressedFilePath); err != nil {
		return nil, fmt.Errorf("cannot move compressed file from %q to %q: %s", tmpFilePath, compressedFilePath, err)
	}
	return h.newCompressedFSFile(compressedFilePath)
}

func (h *fsHandler) openFSFile(filePath string, mustCompress bool) (*fsFile, error) {
	filePathOriginal := filePath
	if mustCompress {
		filePath += h.compressedFileSuffix
	}

	f, err := os.Open(filePath)
	if err != nil {
		if mustCompress && os.IsNotExist(err) {
			return h.compressAndOpenFSFile(filePathOriginal)
		}
		return nil, err
	}

	fileInfo, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("cannot obtain info for file %q: %s", filePath, err)
	}

	if fileInfo.IsDir() {
		f.Close()
		if mustCompress {
			return nil, fmt.Errorf("directory with unexpected suffix found: %q. Suffix: %q",
				filePath, h.compressedFileSuffix)
		}
		return nil, errDirIndexRequired
	}

	if mustCompress {
		fileInfoOriginal, err := os.Stat(filePathOriginal)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("cannot obtain info for original file %q: %s", filePathOriginal, err)
		}

		if fileInfoOriginal.ModTime() != fileInfo.ModTime() {
			// The compressed file became stale. Re-create it.
			f.Close()
			os.Remove(filePath)
			return h.compressAndOpenFSFile(filePathOriginal)
		}
	}

	return h.newFSFile(f, fileInfo, mustCompress)
}

func (h *fsHandler) newFSFile(f *os.File, fileInfo os.FileInfo, compressed bool) (*fsFile, error) {
	n := fileInfo.Size()
	contentLength := int(n)
	if n != int64(contentLength) {
		f.Close()
		return nil, fmt.Errorf("too big file: %d bytes", n)
	}

	// detect content-type
	ext := fileExtension(fileInfo.Name(), compressed, h.compressedFileSuffix)
	contentType := mime.TypeByExtension(ext)
	if len(contentType) == 0 {
		data, err := readFileHeader(f, compressed)
		if err != nil {
			return nil, fmt.Errorf("cannot read header of the file %q: %s", f.Name(), err)
		}
		contentType = http.DetectContentType(data)
	}

	lastModified := fileInfo.ModTime()
	ff := &fsFile{
		h:               h,
		f:               f,
		contentType:     contentType,
		contentLength:   contentLength,
		compressed:      compressed,
		lastModified:    lastModified,
		lastModifiedStr: bytesconv.AppendHTTPDate(make([]byte, 0, len(http.TimeFormat)), lastModified),

		t: time.Now(),
	}
	return ff, nil
}

func (h *fsHandler) createDirIndex(base *protocol.URI, dirPath string, mustCompress bool) (*fsFile, error) {
	w := &bytebufferpool.ByteBuffer{}

	basePathEscaped := html.EscapeString(string(base.Path()))
	fmt.Fprintf(w, "<html><head><title>%s</title><style>.dir { font-weight: bold }</style></head><body>", basePathEscaped)
	fmt.Fprintf(w, "<h1>%s</h1>", basePathEscaped)
	fmt.Fprintf(w, "<ul>")

	if len(basePathEscaped) > 1 {
		var parentURI protocol.URI
		base.CopyTo(&parentURI)
		parentURI.Update(string(base.Path()) + "/..")
		parentPathEscaped := html.EscapeString(string(parentURI.Path()))
		fmt.Fprintf(w, `<li><a href="%s" class="dir">..</a></li>`, parentPathEscaped)
	}

	f, err := os.Open(dirPath)
	if err != nil {
		return nil, err
	}

	fileinfos, err := f.Readdir(0)
	f.Close()
	if err != nil {
		return nil, err
	}

	fm := make(map[string]os.FileInfo, len(fileinfos))
	filenames := make([]string, 0, len(fileinfos))
	for _, fi := range fileinfos {
		name := fi.Name()
		if strings.HasSuffix(name, h.compressedFileSuffix) {
			// Do not show compressed files on index page.
			continue
		}
		fm[name] = fi
		filenames = append(filenames, name)
	}

	var u protocol.URI
	base.CopyTo(&u)
	u.Update(string(u.Path()) + "/")

	sort.Strings(filenames)
	for _, name := range filenames {
		u.Update(name)
		pathEscaped := html.EscapeString(string(u.Path()))
		fi := fm[name]
		auxStr := "dir"
		className := "dir"
		if !fi.IsDir() {
			auxStr = fmt.Sprintf("file, %d bytes", fi.Size())
			className = "file"
		}
		fmt.Fprintf(w, `<li><a href="%s" class="%s">%s</a>, %s, last modified %s</li>`,
			pathEscaped, className, html.EscapeString(name), auxStr, fsModTime(fi.ModTime()))
	}

	fmt.Fprintf(w, "</ul></body></html>")
	if mustCompress {
		var zbuf bytebufferpool.ByteBuffer
		zbuf.B = compress.AppendGzipBytesLevel(zbuf.B, w.B, compress.CompressDefaultCompression)
		w = &zbuf
	}

	dirIndex := w.B
	lastModified := time.Now()
	ff := &fsFile{
		h:               h,
		dirIndex:        dirIndex,
		contentType:     "text/html; charset=utf-8",
		contentLength:   len(dirIndex),
		compressed:      mustCompress,
		lastModified:    lastModified,
		lastModifiedStr: bytesconv.AppendHTTPDate(make([]byte, 0, len(http.TimeFormat)), lastModified),

		t: lastModified,
	}
	return ff, nil
}

func (h *fsHandler) openIndexFile(ctx *RequestContext, dirPath string, mustCompress bool) (*fsFile, error) {
	for _, indexName := range h.indexNames {
		indexFilePath := dirPath + "/" + indexName
		ff, err := h.openFSFile(indexFilePath, mustCompress)
		if err == nil {
			return ff, nil
		}
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("cannot open file %q: %s", indexFilePath, err)
		}
	}

	if !h.generateIndexPages {
		return nil, fmt.Errorf("cannot access directory without index page. Directory %q", dirPath)
	}

	return h.createDirIndex(ctx.URI(), dirPath, mustCompress)
}

func (ff *fsFile) decReadersCount() {
	ff.h.cacheLock.Lock()
	defer ff.h.cacheLock.Unlock()
	ff.readersCount--
	if ff.readersCount < 0 {
		panic("BUG: negative fsFile.readersCount!")
	}
}

func (ff *fsFile) bigFileReader() (io.Reader, error) {
	if ff.f == nil {
		panic("BUG: ff.f must be non-nil in bigFileReader")
	}

	var r io.Reader

	ff.bigFilesLock.Lock()
	n := len(ff.bigFiles)
	if n > 0 {
		r = ff.bigFiles[n-1]
		ff.bigFiles = ff.bigFiles[:n-1]
	}
	ff.bigFilesLock.Unlock()

	if r != nil {
		return r, nil
	}

	f, err := os.Open(ff.f.Name())
	if err != nil {
		return nil, fmt.Errorf("cannot open already opened file: %s", err)
	}
	return &bigFileReader{
		f:  f,
		ff: ff,
		r:  f,
	}, nil
}

func (ff *fsFile) NewReader() (io.Reader, error) {
	if ff.isBig() {
		r, err := ff.bigFileReader()
		if err != nil {
			ff.decReadersCount()
		}
		return r, err
	}
	return ff.smallFileReader(), nil
}

func (ff *fsFile) smallFileReader() io.Reader {
	v := ff.h.smallFileReaderPool.Get()
	if v == nil {
		v = &fsSmallFileReader{}
	}
	r := v.(*fsSmallFileReader)
	r.ff = ff
	r.endPos = ff.contentLength
	if r.startPos > 0 {
		panic("BUG: fsSmallFileReader with non-nil startPos found in the pool")
	}
	return r
}

func (h *fsHandler) handleRequest(c context.Context, ctx *RequestContext) {
	var path []byte
	if h.pathRewrite != nil {
		path = h.pathRewrite(ctx)
	} else {
		path = ctx.Path()
	}
	path = stripTrailingSlashes(path)

	if n := bytes.IndexByte(path, 0); n >= 0 {
		hlog.SystemLogger().Errorf("Cannot serve path with nil byte at position=%d, path=%q", n, path)
		ctx.AbortWithMsg("Are you a hacker?", consts.StatusBadRequest)
		return
	}
	if h.pathRewrite != nil {
		// There is no need to check for '/../' if path = ctx.Path(),
		// since ctx.Path must normalize and sanitize the path.

		if n := bytes.Index(path, bytestr.StrSlashDotDotSlash); n >= 0 {
			hlog.SystemLogger().Errorf("Cannot serve path with '/../' at position=%d due to security reasons, path=%q", n, path)
			ctx.AbortWithMsg("Internal Server Error", consts.StatusInternalServerError)
			return
		}
	}

	mustCompress := false
	fileCache := h.cache
	byteRange := ctx.Request.Header.PeekRange()
	if len(byteRange) == 0 && h.compress && ctx.Request.Header.HasAcceptEncodingBytes(bytestr.StrGzip) {
		mustCompress = true
		fileCache = h.compressedCache
	}

	h.cacheLock.Lock()
	ff, ok := fileCache[string(path)]
	if ok {
		ff.readersCount++
	}
	h.cacheLock.Unlock()

	if !ok {
		pathStr := string(path)
		filePath := h.root + pathStr
		var err error
		ff, err = h.openFSFile(filePath, mustCompress)

		if mustCompress && err == errNoCreatePermission {
			hlog.SystemLogger().Errorf("Insufficient permissions for saving compressed file for path=%q. Serving uncompressed file. "+
				"Allow write access to the directory with this file in order to improve hertz performance", filePath)
			mustCompress = false
			ff, err = h.openFSFile(filePath, mustCompress)
		}
		if err == errDirIndexRequired {
			ff, err = h.openIndexFile(ctx, filePath, mustCompress)
			if err != nil {
				hlog.SystemLogger().Errorf("Cannot open dir index, path=%q, error=%s", filePath, err)
				ctx.AbortWithMsg("Directory index is forbidden", consts.StatusForbidden)
				return
			}
		} else if err != nil {
			hlog.SystemLogger().Errorf("Cannot open file=%q, error=%s", filePath, err)
			if h.pathNotFound == nil {
				ctx.AbortWithMsg("Cannot open requested path", consts.StatusNotFound)
			} else {
				ctx.SetStatusCode(consts.StatusNotFound)
				h.pathNotFound(c, ctx)
			}
			return
		}

		h.cacheLock.Lock()
		ff1, ok := fileCache[pathStr]
		if !ok {
			fileCache[pathStr] = ff
			ff.readersCount++
		} else {
			ff1.readersCount++
		}
		h.cacheLock.Unlock()

		if ok {
			// The file has been already opened by another
			// goroutine, so close the current file and use
			// the file opened by another goroutine instead.
			ff.Release()
			ff = ff1
		}
	}

	if !ctx.IfModifiedSince(ff.lastModified) {
		ff.decReadersCount()
		ctx.NotModified()
		return
	}

	r, err := ff.NewReader()
	if err != nil {
		hlog.SystemLogger().Errorf("Cannot obtain file reader for path=%q, error=%s", path, err)
		ctx.AbortWithMsg("Internal Server Error", consts.StatusInternalServerError)
		return
	}

	hdr := &ctx.Response.Header
	if ff.compressed {
		hdr.SetContentEncodingBytes(bytestr.StrGzip)
	}

	statusCode := consts.StatusOK
	contentLength := ff.contentLength
	if h.acceptByteRange {
		hdr.SetCanonical(bytestr.StrAcceptRanges, bytestr.StrBytes)
		if len(byteRange) > 0 {
			startPos, endPos, err := ParseByteRange(byteRange, contentLength)
			if err != nil {
				r.(io.Closer).Close()
				hlog.SystemLogger().Errorf("Cannot parse byte range %q for path=%q,error=%s", byteRange, path, err)
				ctx.AbortWithMsg("Range Not Satisfiable", consts.StatusRequestedRangeNotSatisfiable)
				return
			}

			if err = r.(byteRangeUpdater).UpdateByteRange(startPos, endPos); err != nil {
				r.(io.Closer).Close()
				hlog.SystemLogger().Errorf("Cannot seek byte range %q for path=%q, error=%s", byteRange, path, err)
				ctx.AbortWithMsg("Internal Server Error", consts.StatusInternalServerError)
				return
			}

			hdr.SetContentRange(startPos, endPos, contentLength)
			contentLength = endPos - startPos + 1
			statusCode = consts.StatusPartialContent
		}
	}

	hdr.SetCanonical(bytestr.StrLastModified, ff.lastModifiedStr)
	if !ctx.IsHead() {
		ctx.SetBodyStream(r, contentLength)
	} else {
		ctx.Response.ResetBody()
		ctx.Response.SkipBody = true
		ctx.Response.Header.SetContentLength(contentLength)
		if rc, ok := r.(io.Closer); ok {
			if err := rc.Close(); err != nil {
				hlog.SystemLogger().Errorf("Cannot close file reader: error=%s", err)
				ctx.AbortWithMsg("Internal Server Error", consts.StatusInternalServerError)
				return
			}
		}
	}
	hdr.SetNoDefaultContentType(true)
	if len(hdr.ContentType()) == 0 {
		ctx.SetContentType(ff.contentType)
	}
	ctx.SetStatusCode(statusCode)
}

type fsFile struct {
	h             *fsHandler
	f             *os.File
	dirIndex      []byte
	contentType   string
	contentLength int
	compressed    bool

	lastModified    time.Time
	lastModifiedStr []byte

	t            time.Time
	readersCount int

	bigFiles     []*bigFileReader
	bigFilesLock sync.Mutex
}

func (ff *fsFile) Release() {
	if ff.f != nil {
		ff.f.Close()

		if ff.isBig() {
			ff.bigFilesLock.Lock()
			for _, r := range ff.bigFiles {
				r.f.Close()
			}
			ff.bigFilesLock.Unlock()
		}
	}
}

func (ff *fsFile) isBig() bool {
	return ff.contentLength > consts.MaxSmallFileSize && len(ff.dirIndex) == 0
}

func cleanCacheNolock(cache map[string]*fsFile, pendingFiles, filesToRelease []*fsFile, cacheDuration time.Duration) ([]*fsFile, []*fsFile) {
	t := time.Now()
	for k, ff := range cache {
		if t.Sub(ff.t) > cacheDuration {
			if ff.readersCount > 0 {
				// There are pending readers on stale file handle,
				// so we cannot close it. Put it into pendingFiles
				// so it will be closed later.
				pendingFiles = append(pendingFiles, ff)
			} else {
				filesToRelease = append(filesToRelease, ff)
			}
			delete(cache, k)
		}
	}
	return pendingFiles, filesToRelease
}

func stripTrailingSlashes(path []byte) []byte {
	for len(path) > 0 && path[len(path)-1] == '/' {
		path = path[:len(path)-1]
	}
	return path
}

func isFileCompressible(f *os.File, minCompressRatio float64) bool {
	// Try compressing the first 4kb of the file
	// and see if it can be compressed by more than
	// the given minCompressRatio.
	b := bytebufferpool.Get()
	zw := compress.AcquireStacklessGzipWriter(b, compress.CompressDefaultCompression)
	lr := &io.LimitedReader{
		R: f,
		N: 4096,
	}
	zrw := network.NewWriter(zw)
	_, err := utils.CopyZeroAlloc(zrw, lr)
	compress.ReleaseStacklessGzipWriter(zw, compress.CompressDefaultCompression)
	f.Seek(0, 0) //nolint:errcheck
	if err != nil {
		return false
	}

	n := 4096 - lr.N
	zn := len(b.B)
	bytebufferpool.Put(b)
	return float64(zn) < float64(n)*minCompressRatio
}

var (
	filesLockMap     = make(map[string]*sync.Mutex)
	filesLockMapLock sync.Mutex
)

func getFileLock(absPath string) *sync.Mutex {
	filesLockMapLock.Lock()
	flock := filesLockMap[absPath]
	if flock == nil {
		flock = &sync.Mutex{}
		filesLockMap[absPath] = flock
	}
	filesLockMapLock.Unlock()
	return flock
}

func fileExtension(path string, compressed bool, compressedFileSuffix string) string {
	if compressed && strings.HasSuffix(path, compressedFileSuffix) {
		path = path[:len(path)-len(compressedFileSuffix)]
	}
	n := strings.LastIndexByte(path, '.')
	if n < 0 {
		return ""
	}
	return path[n:]
}

func readFileHeader(f *os.File, compressed bool) ([]byte, error) {
	r := io.Reader(f)
	var zr *gzip.Reader
	if compressed {
		var err error
		if zr, err = compress.AcquireGzipReader(f); err != nil {
			return nil, err
		}
		r = zr
	}

	lr := &io.LimitedReader{
		R: r,
		N: 512,
	}
	data, err := ioutil.ReadAll(lr)
	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}

	if zr != nil {
		compress.ReleaseGzipReader(zr)
	}

	return data, err
}

func fsModTime(t time.Time) time.Time {
	return t.In(time.UTC).Truncate(time.Second)
}

// ParseByteRange parses 'Range: bytes=...' header value.
//
// It follows https://www.w3.org/Protocols/rfc2616/rfc2616-sec14.html#sec14.35 .
func ParseByteRange(byteRange []byte, contentLength int) (startPos, endPos int, err error) {
	b := byteRange
	if !bytes.HasPrefix(b, bytestr.StrBytes) {
		return 0, 0, fmt.Errorf("unsupported range units: %q. Expecting %q", byteRange, bytestr.StrBytes)
	}

	b = b[len(bytestr.StrBytes):]
	if len(b) == 0 || b[0] != '=' {
		return 0, 0, fmt.Errorf("missing byte range in %q", byteRange)
	}
	b = b[1:]

	n := bytes.IndexByte(b, '-')
	if n < 0 {
		return 0, 0, fmt.Errorf("missing the end position of byte range in %q", byteRange)
	}

	if n == 0 {
		v, err := bytesconv.ParseUint(b[n+1:])
		if err != nil {
			return 0, 0, err
		}
		startPos := contentLength - v
		if startPos < 0 {
			startPos = 0
		}
		return startPos, contentLength - 1, nil
	}

	if startPos, err = bytesconv.ParseUint(b[:n]); err != nil {
		return 0, 0, err
	}
	if startPos >= contentLength {
		return 0, 0, fmt.Errorf("the start position of byte range cannot exceed %d. byte range %q", contentLength-1, byteRange)
	}

	b = b[n+1:]
	if len(b) == 0 {
		return startPos, contentLength - 1, nil
	}

	if endPos, err = bytesconv.ParseUint(b); err != nil {
		return 0, 0, err
	}
	if endPos >= contentLength {
		endPos = contentLength - 1
	}
	if endPos < startPos {
		return 0, 0, fmt.Errorf("the start position of byte range cannot exceed the end position. byte range %q", byteRange)
	}
	return startPos, endPos, nil
}

// NewVHostPathRewriter returns path rewriter, which strips slashesCount
// leading slashes from the path and prepends the path with request's host,
// thus simplifying virtual hosting for static files.
//
// Examples:
//
//   - host=foobar.com, slashesCount=0, original path="/foo/bar".
//     Resulting path: "/foobar.com/foo/bar"
//
//   - host=img.aaa.com, slashesCount=1, original path="/images/123/456.jpg"
//     Resulting path: "/img.aaa.com/123/456.jpg"
func NewVHostPathRewriter(slashesCount int) PathRewriteFunc {
	return func(ctx *RequestContext) []byte {
		path := stripLeadingSlashes(ctx.Path(), slashesCount)
		host := ctx.Host()
		if n := bytes.IndexByte(host, '/'); n >= 0 {
			host = nil
		}
		if len(host) == 0 {
			host = strInvalidHost
		}
		b := bytebufferpool.Get()
		b.B = append(b.B, '/')
		b.B = append(b.B, host...)
		b.B = append(b.B, path...)
		ctx.URI().SetPathBytes(b.B)
		bytebufferpool.Put(b)

		return ctx.Path()
	}
}

func stripLeadingSlashes(path []byte, stripSlashes int) []byte {
	for stripSlashes > 0 && len(path) > 0 {
		if path[0] != '/' {
			panic("BUG: path must start with slash")
		}
		n := bytes.IndexByte(path[1:], '/')
		if n < 0 {
			path = path[:0]
			break
		}
		path = path[n+1:]
		stripSlashes--
	}
	return path
}

// ServeFileUncompressed returns HTTP response containing file contents
// from the given path.
//
// Directory contents is returned if path points to directory.
//
// ServeFile may be used for saving network traffic when serving files
// with good compression ratio.
//
// See also RequestCtx.SendFile.
func ServeFileUncompressed(ctx *RequestContext, path string) {
	ctx.Request.Header.DelBytes(bytestr.StrAcceptEncoding)
	ServeFile(ctx, path)
}

// NewPathSlashesStripper returns path rewriter, which strips slashesCount
// leading slashes from the path.
//
// Examples:
//
//   - slashesCount = 0, original path: "/foo/bar", result: "/foo/bar"
//   - slashesCount = 1, original path: "/foo/bar", result: "/bar"
//   - slashesCount = 2, original path: "/foo/bar", result: ""
//
// The returned path rewriter may be used as FS.PathRewrite .
func NewPathSlashesStripper(slashesCount int) PathRewriteFunc {
	return func(ctx *RequestContext) []byte {
		return stripLeadingSlashes(ctx.Path(), slashesCount)
	}
}
