package seller

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type basicWriter struct{ http.ResponseWriter }

type failedWriter struct {
	http.ResponseWriter
	err error
}

func (w failedWriter) Write([]byte) (int, error) { return 0, w.err }

func TestPrivateWriterPreservesFailure(t *testing.T) {
	recorder := httptest.NewRecorder()
	sentinel := errors.New("write failure")
	servePrivate(failedWriter{recorder, sentinel}, httptest.NewRequest("GET", "/", nil), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if n, err := w.Write([]byte("paid")); n != 0 || err != sentinel {
			t.Fatal(n, err)
		}
	}))
	func() {
		defer func() {
			if recover() != sentinel {
				t.Error("handler panic changed")
			}
		}()
		servePrivate(recorder, httptest.NewRequest("GET", "/", nil), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Cache-Control", "public")
			panic(sentinel)
		}))
	}()
	if recorder.Header().Get("Cache-Control") != "public, private" {
		t.Fatal(recorder.Header())
	}
}

type capableWriter struct {
	http.ResponseWriter
	flushed, copied, pushed, hijacked bool
	closed                            chan bool
	err                               error
}

func (w *capableWriter) Flush() { w.flushed = true; w.WriteHeader(200) }
func (w *capableWriter) ReadFrom(r io.Reader) (int64, error) {
	w.copied = true
	if w.err != nil {
		return 0, w.err
	}
	return io.Copy(w.ResponseWriter, r)
}
func (w *capableWriter) CloseNotify() <-chan bool             { return w.closed }
func (w *capableWriter) Push(string, *http.PushOptions) error { w.pushed = true; return w.err }
func (w *capableWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.hijacked = true
	return nil, nil, w.err
}

func TestPrivateWriterCapabilities(t *testing.T) {
	plain := basicWriter{httptest.NewRecorder()}
	servePrivate(plain, httptest.NewRequest("GET", "/", nil), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, ok := w.(http.Flusher); ok {
			t.Error("invented Flusher")
		}
		if _, ok := w.(http.Hijacker); ok {
			t.Error("invented Hijacker")
		}
		if _, ok := w.(http.Pusher); ok {
			t.Error("invented Pusher")
		}
		if _, ok := w.(io.ReaderFrom); ok {
			t.Error("invented ReaderFrom")
		}
		if _, ok := w.(http.CloseNotifier); ok {
			t.Error("invented CloseNotifier")
		}
		if !errors.Is(http.NewResponseController(w).Flush(), http.ErrNotSupported) {
			t.Error("unsupported flush changed")
		}
	}))
	for _, mode := range []string{"copy", "flush", "error"} {
		t.Run(mode, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			underlying := &capableWriter{ResponseWriter: recorder, closed: make(chan bool)}
			sentinel := errors.New("writer failure")
			if mode == "error" {
				underlying.err = sentinel
			}
			servePrivate(underlying, httptest.NewRequest("GET", "/", nil), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Cache-Control", "public")
				if mode == "flush" {
					w.(http.Flusher).Flush()
				} else {
					n, err := w.(io.ReaderFrom).ReadFrom(strings.NewReader("paid"))
					if !errors.Is(err, underlying.err) || mode == "copy" && n != 4 {
						t.Fatal(n, err)
					}
				}
				if w.(http.CloseNotifier).CloseNotify() != underlying.closed {
					t.Error("notification changed")
				}
				if err := w.(http.Pusher).Push("/style", nil); !errors.Is(err, underlying.err) {
					t.Error(err)
				}
				if _, _, err := w.(http.Hijacker).Hijack(); !errors.Is(err, underlying.err) {
					t.Error(err)
				}
				if w.(interface{ Unwrap() http.ResponseWriter }).Unwrap() != underlying {
					t.Error("wrong unwrap")
				}
			}))
			if !underlying.pushed || !underlying.hijacked || mode == "flush" && !underlying.flushed || mode != "flush" && !underlying.copied {
				t.Fatal("capability not forwarded")
			}
			if recorder.Result().Header.Get("Cache-Control") != "public, private" {
				t.Fatal("copy/flush bypassed cache enforcement")
			}
		})
	}
}
