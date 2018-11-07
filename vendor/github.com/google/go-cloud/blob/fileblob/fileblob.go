// Copyright 2018 The Go Cloud Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package fileblob provides a bucket implementation that operates on the local
// filesystem. This should not be used for production: it is intended for local
// development.
//
// Blob names must only contain alphanumeric characters, slashes, periods,
// spaces, underscores, and dashes. Repeated slashes, a leading "./" or "../",
// or the sequence "/./" is not permitted. This is to ensure that blob names map
// cleanly onto files underneath a directory.
//
// It does not support any types for As.
package fileblob

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	slashpath "path"
	"path/filepath"
	"strings"

	"github.com/google/go-cloud/blob"
	"github.com/google/go-cloud/blob/driver"
)

const defaultPageSize = 1000

type bucket struct {
	dir string
}

// OpenBucket creates a *blob.Bucket that reads and writes to dir.
// dir must exist.
func OpenBucket(dir string) (*blob.Bucket, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("open file bucket: %v", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("open file bucket: %s is not a directory", dir)
	}
	return blob.NewBucket(&bucket{dir}), nil
}

// resolvePath converts a key into a relative filesystem path. It guarantees
// that there will only be one valid key for a given path and that the resulting
// path will not reach outside the directory.
func resolvePath(key string) (string, error) {
	for _, c := range key {
		if !('A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '/' || c == '.' || c == ' ' || c == '_' || c == '-') {
			return "", fmt.Errorf("contains invalid character %q", c)
		}
	}
	if cleaned := slashpath.Clean(key); key != cleaned {
		return "", fmt.Errorf("not a clean slash-separated path")
	}
	if slashpath.IsAbs(key) {
		return "", fmt.Errorf("starts with a slash")
	}
	if key == "." {
		return "", fmt.Errorf("invalid path \".\"")
	}
	if strings.HasPrefix(key, "../") {
		return "", fmt.Errorf("starts with \"../\"")
	}
	return filepath.FromSlash(key), nil
}

func (b *bucket) forKey(key string) (string, os.FileInfo, *xattrs, error) {
	relpath, err := resolvePath(key)
	if err != nil {
		return "", nil, nil, fmt.Errorf("open file blob %s: %v", key, err)
	}
	path := filepath.Join(b.dir, relpath)
	if strings.HasSuffix(path, attrsExt) {
		return "", nil, nil, fmt.Errorf("open file blob %s: extension %q cannot be directly read", key, attrsExt)
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil, nil, fileError{relpath: relpath, msg: err.Error(), kind: driver.NotFound}
		}
		return "", nil, nil, fmt.Errorf("open file blob %s: %v", key, err)
	}
	xa, err := getAttrs(path)
	if err != nil {
		return "", nil, nil, fmt.Errorf("open file attributes %s: %v", key, err)
	}
	return path, info, &xa, nil
}

// ListPaged implements driver.ListPaged.
func (b *bucket) ListPaged(ctx context.Context, opts *driver.ListOptions) (*driver.ListPage, error) {
	// List everything in the directory, sorted by name.
	// TODO(Issue #541): This should be doing a recursive walk of the directory
	// as well as translating into the abstract namespace that we've created.
	fileinfos, err := ioutil.ReadDir(b.dir)
	if err != nil {
		return nil, err
	}
	pageSize := opts.PageSize
	if pageSize == 0 {
		pageSize = defaultPageSize
	}
	var result driver.ListPage
	for _, info := range fileinfos {
		// Skip the self-generated attribute files.
		if strings.HasSuffix(info.Name(), attrsExt) {
			continue
		}
		// Skip files that don't match the Prefix.
		if opts.Prefix != "" && !strings.HasPrefix(info.Name(), opts.Prefix) {
			continue
		}
		// If a PageToken was provided, skip to it.
		if len(opts.PageToken) > 0 && info.Name() < string(opts.PageToken) {
			continue
		}
		// If we've got a full page of results, and there are more
		// to come, set NextPageToken and stop here.
		if len(result.Objects) == pageSize {
			result.NextPageToken = []byte(info.Name())
			break
		}
		// Add this object.
		result.Objects = append(result.Objects, &driver.ListObject{
			Key:     info.Name(),
			ModTime: info.ModTime(),
			Size:    info.Size(),
		})
	}
	return &result, nil
}

// As implements driver.As.
func (b *bucket) As(i interface{}) bool { return false }

// Attributes implements driver.Attributes.
func (b *bucket) Attributes(ctx context.Context, key string) (driver.Attributes, error) {
	_, info, xa, err := b.forKey(key)
	if err != nil {
		return driver.Attributes{}, err
	}
	return driver.Attributes{
		ContentType: xa.ContentType,
		Metadata:    xa.Metadata,
		ModTime:     info.ModTime(),
		Size:        info.Size(),
	}, nil
}

// NewRangeReader implements driver.NewRangeReader.
func (b *bucket) NewRangeReader(ctx context.Context, key string, offset, length int64) (driver.Reader, error) {
	path, info, xa, err := b.forKey(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open file blob %s: %v", key, err)
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return nil, fmt.Errorf("open file blob %s: %v", key, err)
		}
	}
	r := io.Reader(f)
	if length > 0 {
		r = io.LimitReader(r, length)
	}
	return reader{
		r: r,
		c: f,
		attrs: driver.ReaderAttributes{
			ContentType: xa.ContentType,
			ModTime:     info.ModTime(),
			Size:        info.Size(),
		},
	}, nil
}

type reader struct {
	r     io.Reader
	c     io.Closer
	attrs driver.ReaderAttributes
}

func (r reader) Read(p []byte) (int, error) {
	if r.r == nil {
		return 0, io.EOF
	}
	return r.r.Read(p)
}

func (r reader) Close() error {
	if r.c == nil {
		return nil
	}
	return r.c.Close()
}

func (r reader) Attributes() driver.ReaderAttributes {
	return r.attrs
}

func (r reader) As(i interface{}) bool { return false }

// NewTypedWriter implements driver.NewTypedWriter.
func (b *bucket) NewTypedWriter(ctx context.Context, key string, contentType string, opts *driver.WriterOptions) (driver.Writer, error) {
	relpath, err := resolvePath(key)
	if err != nil {
		return nil, fmt.Errorf("open file blob %s: %v", key, err)
	}
	path := filepath.Join(b.dir, relpath)
	if strings.HasSuffix(path, attrsExt) {
		return nil, fmt.Errorf("open file blob %s: extension %q is reserved and cannot be used", key, attrsExt)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0777); err != nil {
		return nil, fmt.Errorf("open file blob %s: %v", key, err)
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("open file blob %s: %v", key, err)
	}
	if opts.BeforeWrite != nil {
		if err := opts.BeforeWrite(func(interface{}) bool { return false }); err != nil {
			return nil, err
		}
	}
	var metadata map[string]string
	if len(opts.Metadata) > 0 {
		metadata = opts.Metadata
	}
	attrs := xattrs{
		ContentType: contentType,
		Metadata:    metadata,
	}
	return &writer{
		ctx:   ctx,
		w:     f,
		path:  path,
		attrs: attrs,
	}, nil
}

type writer struct {
	ctx   context.Context
	w     io.WriteCloser
	path  string
	attrs xattrs
}

func (w writer) Write(p []byte) (n int, err error) {
	return w.w.Write(p)
}

func (w writer) Close() error {
	// If the write was cancelled, delete the file.
	if err := w.ctx.Err(); err != nil {
		_ = os.Remove(w.path)
		return err
	}
	if err := setAttrs(w.path, w.attrs); err != nil {
		return fmt.Errorf("write blob attributes: %v", err)
	}
	return w.w.Close()
}

// Delete implements driver.Delete.
func (b *bucket) Delete(ctx context.Context, key string) error {
	relpath, err := resolvePath(key)
	if err != nil {
		return fmt.Errorf("delete file blob %s: %v", key, err)
	}
	path := filepath.Join(b.dir, relpath)
	if strings.HasSuffix(path, attrsExt) {
		return fmt.Errorf("delete file blob %s: extension %q cannot be directly deleted", key, attrsExt)
	}
	err = os.Remove(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fileError{relpath: relpath, msg: err.Error(), kind: driver.NotFound}
		}
		return fmt.Errorf("delete file blob %s: %v", key, err)
	}
	if err = os.Remove(path + attrsExt); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete file blob %s: %v", key, err)
	}
	return nil
}

func (b *bucket) SignedURL(ctx context.Context, key string, opts *driver.SignedURLOptions) (string, error) {
	// TODO(Issue #546): Implemented SignedURL for fileblob.
	return "", fileError{msg: "SignedURL not supported (see issue #546)", kind: driver.NotImplemented}
}

type fileError struct {
	relpath, msg string
	kind         driver.ErrorKind
}

func (e fileError) Error() string {
	if e.relpath == "" {
		return fmt.Sprintf("fileblob: %s", e.msg)
	}
	return fmt.Sprintf("fileblob: object %s: %s", e.relpath, e.msg)
}

func (e fileError) Kind() driver.ErrorKind {
	return e.kind
}
