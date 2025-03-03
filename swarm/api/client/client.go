// Copyright 2017 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package client

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/eeefan/dpeth/swarm/api"
)

var (
	DefaultGateway = "http://localhost:8500"
	DefaultClient  = NewClient(DefaultGateway)
)

func NewClient(gateway string) *Client {
	return &Client{
		Gateway: gateway,
	}
}

// Client wraps interaction with a swarm HTTP gateway.
type Client struct {
	Gateway string
}

// UploadRaw uploads raw data to swarm and returns the resulting hash
func (c *Client) UploadRaw(r io.Reader, size int64) (string, error) {
	if size <= 0 {
		return "", errors.New("data size must be greater than zero")
	}
	req, err := http.NewRequest("POST", c.Gateway+"/bzz-raw:/", r)
	if err != nil {
		return "", err
	}
	req.ContentLength = size
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected HTTP status: %s", res.Status)
	}
	data, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// DownloadRaw downloads raw data from swarm
func (c *Client) DownloadRaw(hash string) (io.ReadCloser, error) {
	uri := c.Gateway + "/bzz-raw:/" + hash
	res, err := http.DefaultClient.Get(uri)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return nil, fmt.Errorf("unexpected HTTP status: %s", res.Status)
	}
	return res.Body, nil
}

// File represents a file in a swarm manifest and is used for uploading and
// downloading content to and from swarm
type File struct {
	io.ReadCloser
	api.ManifestEntry
}

// Open opens a local file which can then be passed to client.Upload to upload
// it to swarm
func Open(path string) (*File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	stat, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &File{
		ReadCloser: f,
		ManifestEntry: api.ManifestEntry{
			ContentType: mime.TypeByExtension(filepath.Ext(path)),
			Mode:        int64(stat.Mode()),
			Size:        stat.Size(),
			ModTime:     stat.ModTime(),
		},
	}, nil
}

// Upload uploads a file to swarm and either adds it to an existing manifest
// (if the manifest argument is non-empty) or creates a new manifest containing
// the file, returning the resulting manifest hash (the file will then be
// available at bzz:/<hash>/<path>)
func (c *Client) Upload(file *File, manifest string) (string, error) {
	if file.Size <= 0 {
		return "", errors.New("file size must be greater than zero")
	}
	return c.TarUpload(manifest, &FileUploader{file})
}

// Download downloads a file with the given path from the swarm manifest with
// the given hash (i.e. it gets bzz:/<hash>/<path>)
func (c *Client) Download(hash, path string) (*File, error) {
	uri := c.Gateway + "/bzz:/" + hash + "/" + path
	res, err := http.DefaultClient.Get(uri)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return nil, fmt.Errorf("unexpected HTTP status: %s", res.Status)
	}
	return &File{
		ReadCloser: res.Body,
		ManifestEntry: api.ManifestEntry{
			ContentType: res.Header.Get("Content-Type"),
			Size:        res.ContentLength,
		},
	}, nil
}

// UploadDirectory uploads a directory tree to swarm and either adds the files
// to an existing manifest (if the manifest argument is non-empty) or creates a
// new manifest, returning the resulting manifest hash (files from the
// directory will then be available at bzz:/<hash>/path/to/file), with
// the file specified in defaultPath being uploaded to the root of the manifest
// (i.e. bzz:/<hash>/)
func (c *Client) UploadDirectory(dir, defaultPath, manifest string) (string, error) {
	stat, err := os.Stat(dir)
	if err != nil {
		return "", err
	} else if !stat.IsDir() {
		return "", fmt.Errorf("not a directory: %s", dir)
	}
	return c.TarUpload(manifest, &DirectoryUploader{dir, defaultPath})
}

// DownloadDirectory downloads the files contained in a swarm manifest under
// the given path into a local directory (existing files will be overwritten)
func (c *Client) DownloadDirectory(hash, path, destDir string) error {
	stat, err := os.Stat(destDir)
	if err != nil {
		return err
	} else if !stat.IsDir() {
		return fmt.Errorf("not a directory: %s", destDir)
	}

	uri := c.Gateway + "/bzz:/" + hash + "/" + path
	req, err := http.NewRequest("GET", uri, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/x-tar")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected HTTP status: %s", res.Status)
	}
	tr := tar.NewReader(res.Body)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
		// ignore the default path file
		if hdr.Name == "" {
			continue
		}

		dstPath := filepath.Join(destDir, filepath.Clean(strings.TrimPrefix(hdr.Name, path)))
		if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
			return err
		}
		var mode os.FileMode = 0644
		if hdr.Mode > 0 {
			mode = os.FileMode(hdr.Mode)
		}
		dst, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
		if err != nil {
			return err
		}
		n, err := io.Copy(dst, tr)
		dst.Close()
		if err != nil {
			return err
		} else if n != hdr.Size {
			return fmt.Errorf("expected %s to be %d bytes but got %d", hdr.Name, hdr.Size, n)
		}
	}
}

// UploadManifest uploads the given manifest to swarm
func (c *Client) UploadManifest(m *api.Manifest) (string, error) {
	data, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return c.UploadRaw(bytes.NewReader(data), int64(len(data)))
}

// DownloadManifest downloads a swarm manifest
func (c *Client) DownloadManifest(hash string) (*api.Manifest, error) {
	res, err := c.DownloadRaw(hash)
	if err != nil {
		return nil, err
	}
	defer res.Close()
	var manifest api.Manifest
	if err := json.NewDecoder(res).Decode(&manifest); err != nil {
		return nil, err
	}
	return &manifest, nil
}

// List list files in a swarm manifest which have the given prefix, grouping
// common prefixes using "/" as a delimiter.
//
// For example, if the manifest represents the following directory structure:
//
// file1.txt
// file2.txt
// dir1/file3.txt
// dir1/dir2/file4.txt
//
// Then:
//
// - a prefix of ""      would return [dir1/, file1.txt, file2.txt]
// - a prefix of "file"  would return [file1.txt, file2.txt]
// - a prefix of "dir1/" would return [dir1/dir2/, dir1/file3.txt]
//
// where entries ending with "/" are common prefixes.
func (c *Client) List(hash, prefix string) (*api.ManifestList, error) {
	res, err := http.DefaultClient.Get(c.Gateway + "/bzz-list:/" + hash + "/" + prefix)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected HTTP status: %s", res.Status)
	}
	var list api.ManifestList
	if err := json.NewDecoder(res.Body).Decode(&list); err != nil {
		return nil, err
	}
	return &list, nil
}

// Uploader uploads files to swarm using a provided UploadFn
type Uploader interface {
	Upload(UploadFn) error
}

type UploaderFunc func(UploadFn) error

func (u UploaderFunc) Upload(upload UploadFn) error {
	return u(upload)
}

// DirectoryUploader uploads all files in a directory, optionally uploading
// a file to the default path
type DirectoryUploader struct {
	Dir         string
	DefaultPath string
}

// Upload performs the upload of the directory and default path
func (d *DirectoryUploader) Upload(upload UploadFn) error {
	if d.DefaultPath != "" {
		file, err := Open(d.DefaultPath)
		if err != nil {
			return err
		}
		if err := upload(file); err != nil {
			return err
		}
	}
	return filepath.Walk(d.Dir, func(path string, f os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if f.IsDir() {
			return nil
		}
		file, err := Open(path)
		if err != nil {
			return err
		}
		relPath, err := filepath.Rel(d.Dir, path)
		if err != nil {
			return err
		}
		file.Path = filepath.ToSlash(relPath)
		return upload(file)
	})
}

// FileUploader uploads a single file
type FileUploader struct {
	File *File
}

// Upload performs the upload of the file
func (f *FileUploader) Upload(upload UploadFn) error {
	return upload(f.File)
}

// UploadFn is the type of function passed to an Uploader to perform the upload
// of a single file (for example, a directory uploader would call a provided
// UploadFn for each file in the directory tree)
type UploadFn func(file *File) error

// TarUpload uses the given Uploader to upload files to swarm as a tar stream,
// returning the resulting manifest hash
func (c *Client) TarUpload(hash string, uploader Uploader) (string, error) {
	reqR, reqW := io.Pipe()
	defer reqR.Close()
	req, err := http.NewRequest("POST", c.Gateway+"/bzz:/"+hash, reqR)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-tar")

	// use 'Expect: 100-continue' so we don't send the request body if
	// the server refuses the request
	req.Header.Set("Expect", "100-continue")

	tw := tar.NewWriter(reqW)

	// define an UploadFn which adds files to the tar stream
	uploadFn := func(file *File) error {
		hdr := &tar.Header{
			Name:    file.Path,
			Mode:    file.Mode,
			Size:    file.Size,
			ModTime: file.ModTime,
			Xattrs: map[string]string{
				"user.swarm.content-type": file.ContentType,
			},
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err = io.Copy(tw, file)
		return err
	}

	// run the upload in a goroutine so we can send the request headers and
	// wait for a '100 Continue' response before sending the tar stream
	go func() {
		err := uploader.Upload(uploadFn)
		if err == nil {
			err = tw.Close()
		}
		reqW.CloseWithError(err)
	}()

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected HTTP status: %s", res.Status)
	}
	data, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// MultipartUpload uses the given Uploader to upload files to swarm as a
// multipart form, returning the resulting manifest hash
func (c *Client) MultipartUpload(hash string, uploader Uploader) (string, error) {
	reqR, reqW := io.Pipe()
	defer reqR.Close()
	req, err := http.NewRequest("POST", c.Gateway+"/bzz:/"+hash, reqR)
	if err != nil {
		return "", err
	}

	// use 'Expect: 100-continue' so we don't send the request body if
	// the server refuses the request
	req.Header.Set("Expect", "100-continue")

	mw := multipart.NewWriter(reqW)
	req.Header.Set("Content-Type", fmt.Sprintf("multipart/form-data; boundary=%q", mw.Boundary()))

	// define an UploadFn which adds files to the multipart form
	uploadFn := func(file *File) error {
		hdr := make(textproto.MIMEHeader)
		hdr.Set("Content-Disposition", fmt.Sprintf("form-data; name=%q", file.Path))
		hdr.Set("Content-Type", file.ContentType)
		hdr.Set("Content-Length", strconv.FormatInt(file.Size, 10))
		w, err := mw.CreatePart(hdr)
		if err != nil {
			return err
		}
		_, err = io.Copy(w, file)
		return err
	}

	// run the upload in a goroutine so we can send the request headers and
	// wait for a '100 Continue' response before sending the multipart form
	go func() {
		err := uploader.Upload(uploadFn)
		if err == nil {
			err = mw.Close()
		}
		reqW.CloseWithError(err)
	}()

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected HTTP status: %s", res.Status)
	}
	data, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
