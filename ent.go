// Copyright (c) 2014, SoundCloud Ltd.
// Use of this source code is governed by the MIT
// license that can be found in the LICENSE file.
// Source code and contact info at http://github.com/soundcloud/ent

package main

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/pat"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/exp"
)

const (
	fileRoute = `/{bucket}/{key:[a-zA-Z0-9\-_\.~\+\/]+}`
)

var (
	Program = "ent"
	Commit  = "0000000"
	Version = "0.0.0"

	requestBytes     = prometheus.NewCounter()
	requestDuration  = prometheus.NewCounter()
	requestDurations = prometheus.NewDefaultHistogram()
	requestTotal     = prometheus.NewCounter()
	responseBytes    = prometheus.NewCounter()
)

func main() {
	var (
		fsRoot      = flag.String("fs.root", "/tmp", "FileSystem root directory")
		httpAddress = flag.String("http.addr", ":5555", "HTTP listen address")
		providerDir = flag.String("provider.dir", "/tmp", "Provider directory with bucket policies")
	)
	flag.Parse()

	prometheus.Register("ent_requests_total", "Total number of requests made", prometheus.NilLabels, requestTotal)
	prometheus.Register("ent_requests_duration_nanoseconds_total", "Total amount of time ent has spent to answer requests in nanoseconds", prometheus.NilLabels, requestDuration)
	prometheus.Register("ent_requests_duration_nanoseconds", "Amounts of time ent has spent answering requests in nanoseconds", prometheus.NilLabels, requestDurations)
	prometheus.Register("ent_request_bytes_total", "Total volume of request payloads emitted in bytes", prometheus.NilLabels, requestBytes)
	prometheus.Register("ent_response_bytes_total", "Total volume of response payloads emitted in bytes", prometheus.NilLabels, responseBytes)

	p, err := NewDiskProvider(*providerDir)
	if err != nil {
		log.Fatal(err)
	}

	fs := NewDiskFS(*fsRoot)
	r := pat.New()
	r.Get(fileRoute, handleGet(p, fs))
	r.Post(fileRoute, handleCreate(p, fs))
	r.Handle("/metrics", prometheus.DefaultRegistry.Handler())
	r.Get("/", handleBucketList(p))

	log.Fatal(http.ListenAndServe(*httpAddress, http.Handler(r)))
}

func handleCreate(p Provider, fs FileSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var (
			began  = time.Now()
			bucket = r.URL.Query().Get(":bucket")
			key    = r.URL.Query().Get(":key")
		)
		defer r.Body.Close()

		b, err := p.Get(bucket)
		if err != nil {
			respondError(w, r.Method, r.URL.String(), err)
			return
		}

		rd := NewReaderDelegator(r.Body)
		f, err := fs.Create(b, key, rd)
		if err != nil {
			respondError(w, r.Method, r.URL.String(), err)
			return
		}
		h, err := f.Hash()
		if err != nil {
			respondError(w, r.Method, r.URL.String(), err)
			return
		}

		rwd := exp.NewResponseWriterDelegator(w)
		defer reportMetrics(rwd, r, rd, b, began, "handleCreate")

		respondCreated(rwd, b, key, h, began)
	}
}

func handleGet(p Provider, fs FileSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var (
			began  = time.Now()
			bucket = r.URL.Query().Get(":bucket")
			key    = r.URL.Query().Get(":key")
		)

		b, err := p.Get(bucket)
		if err != nil {
			respondError(w, r.Method, r.URL.String(), err)
			return
		}

		f, err := fs.Open(b, key)
		if err != nil {
			respondError(w, r.Method, r.URL.String(), err)
			return
		}

		rwd := exp.NewResponseWriterDelegator(w)
		defer reportMetrics(rwd, r, nil, b, began, "handleGet")

		http.ServeContent(rwd, r, key, time.Now(), f)
	}
}

func handleBucketList(p Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		began := time.Now()

		bs, err := p.List()
		if err != nil {
			respondError(w, r.Method, r.URL.String(), err)
			return
		}

		rwd := exp.NewResponseWriterDelegator(w)
		defer reportMetrics(rwd, r, nil, nil, began, "handleBucketList")

		respondBucketList(rwd, bs, began)
	}
}

// ResponseCreated is used as the intermediate type to craft a response for
// a successful file upload.
type ResponseCreated struct {
	Duration time.Duration `json:"duration"`
	File     ResponseFile  `json:"file"`
}

func respondCreated(
	w http.ResponseWriter,
	b *Bucket,
	k string,
	h []byte,
	d time.Time,
) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)

	json.NewEncoder(w).Encode(ResponseCreated{
		Duration: time.Since(d),
		File: ResponseFile{
			Bucket: b,
			Key:    k,
			SHA1:   h,
		},
	})
}

// ResponseBucketList is used as the intermediate type to craft a response for
// the retrieval of all buckets.
type ResponseBucketList struct {
	Count    int           `json:"count"`
	Duration time.Duration `json:"duration"`
	Buckets  []*Bucket     `json:"buckets"`
}

func respondBucketList(w http.ResponseWriter, bs []*Bucket, began time.Time) {
	w.Header().Set("Content-Type", "application/json")

	json.NewEncoder(w).Encode(ResponseBucketList{
		Count:    len(bs),
		Duration: time.Since(began),
		Buckets:  bs,
	})
}

// ResponseError is used as the intermediate type to craft a response for any
// kind of error condition in the http path. This includes common error cases
// like an entity could not be found.
type ResponseError struct {
	Code        int    `json:"code"`
	Error       string `json:"error"`
	Description string `json:"description"`
}

func respondError(w http.ResponseWriter, method, url string, err error) {
	code := http.StatusInternalServerError

	switch err {
	case ErrBucketNotFound, ErrFileNotFound:
		code = http.StatusNotFound
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(ResponseError{
		Code:        code,
		Error:       err.Error(),
		Description: http.StatusText(code),
	})
}

// ResponseFile is used as the intermediate type to return metadata of a File.
type ResponseFile struct {
	Bucket *Bucket
	Key    string
	SHA1   []byte
}

// MarshalJSON returns a ResponseFile JSON encoding with conversion of the
// files SHA1 to hex.
func (r ResponseFile) MarshalJSON() ([]byte, error) {
	return json.Marshal(responseFileWrapper{
		Bucket: r.Bucket,
		Key:    r.Key,
		SHA1:   hex.EncodeToString(r.SHA1),
	})
}

// UnmarshalJSON marshals data into *r with conversion of the hex
// representation of SHA1 into a []byte.
func (r *ResponseFile) UnmarshalJSON(d []byte) error {
	var w responseFileWrapper

	err := json.Unmarshal(d, &w)
	if err != nil {
		return err
	}
	h, err := hex.DecodeString(w.SHA1)
	if err != nil {
		return err
	}

	r.Bucket = w.Bucket
	r.Key = w.Key
	r.SHA1 = h

	return nil
}

type responseFileWrapper struct {
	Bucket *Bucket `json:"bucket"`
	Key    string  `json:"key"`
	SHA1   string  `json:"sha1"`
}

func reportMetrics(
	rwd *exp.ResponseWriterDelegator,
	r *http.Request,
	rd *ReaderDelegator,
	b *Bucket,
	began time.Time,
	op string,
) {
	d := float64(time.Since(began))
	labels := map[string]string{
		"method":    strings.ToLower(r.Method),
		"operation": op,
		"status":    rwd.Status(),
	}

	if b != nil {
		labels["bucket"] = b.Name
	}

	if rd != nil {
		requestBytes.IncrementBy(labels, float64(rd.BytesRead))
	}

	requestTotal.Increment(labels)
	requestDuration.IncrementBy(labels, d)
	requestDurations.Add(labels, d)
	responseBytes.IncrementBy(labels, float64(rwd.BytesWritten))
}

type ReaderDelegator struct {
	io.Reader
	BytesRead int
}

func (r *ReaderDelegator) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.BytesRead += n
	return n, err
}

func NewReaderDelegator(r io.Reader) *ReaderDelegator {
	return &ReaderDelegator{
		Reader: r,
	}
}
