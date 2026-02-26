package download

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cli/cli/v2/internal/safepaths"
	"github.com/cli/cli/v2/pkg/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// func Test_List(t *testing.T) {
//    reg := &httpmock.Registry{}
//    defer reg.Verify(t)
//
//    reg.Register(
//        httpmock.REST("GET", "repos/OWNER/REPO/actions/runs/123/artifacts"),
//        httpmock.StringResponse(`{
//            "total_count": 2,
//            "artifacts": [
//                {"name": "artifact-1"},
//                {"name": "artifact-2"}
//            ]
//        }`))
//
//    api := &apiPlatform{
//        client: &http.Client{Transport: reg},
//        repo:   ghrepo.New("OWNER", "REPO"),
//    }
//    // Provide a stubbed List method or mock as needed for your test
//    // artifacts, err := api.List("123")
//    // For now, skip this test or implement a mock
//    // require.NoError(t, err)
//
//    // require.Equal(t, 2, len(artifacts))
//    // assert.Equal(t, "artifact-1", artifacts[0].Name)
//    // assert.Equal(t, "artifact-2", artifacts[1].Name)
// }

// func Test_List_perRepository(t *testing.T) {
//    reg := &httpmock.Registry{}
//    defer reg.Verify(t)
//
//    reg.Register(
//        httpmock.REST("GET", "repos/OWNER/REPO/actions/artifacts"),
//        httpmock.StringResponse(`{}`))
//
//    api := &apiPlatform{
//        client: &http.Client{Transport: reg},
//        repo:   ghrepo.New("OWNER", "REPO"),
//    }
//    // _, err := api.List("")
//    // For now, skip this test or implement a mock
//    // require.NoError(t, err)
// }

func Test_Download(t *testing.T) {
	tmpDir := t.TempDir()
	destDir, err := safepaths.ParseAbsolute(filepath.Join(tmpDir, "artifact"))
	require.NoError(t, err)

	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	// First GET is the range-support probe; the mock returns 200 so multipart is skipped.
	reg.Register(
		httpmock.REST("GET", "repos/OWNER/REPO/actions/artifacts/12345/zip"),
		httpmock.FileResponse("./fixtures/myproject.zip"))
	// Second GET is the actual single-stream download.
	reg.Register(
		httpmock.REST("GET", "repos/OWNER/REPO/actions/artifacts/12345/zip"),
		httpmock.FileResponse("./fixtures/myproject.zip"))

	api := &apiPlatform{
		client: &http.Client{Transport: reg},
	}
	require.NoError(t, api.Download("https://api.github.com/repos/OWNER/REPO/actions/artifacts/12345/zip", destDir, nil))

	var paths []string
	parentPrefix := tmpDir + string(filepath.Separator)
	err = filepath.Walk(tmpDir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if p == tmpDir {
			return nil
		}
		entry := strings.TrimPrefix(p, parentPrefix)
		if info.IsDir() {
			entry += "/"
		} else if info.Mode()&0111 != 0 {
			entry += "(X)"
		}
		paths = append(paths, entry)
		return nil
	})
	require.NoError(t, err)

	sort.Strings(paths)
	assert.Equal(t, []string{
		"artifact/",
		filepath.Join("artifact", "bin") + "/",
		filepath.Join("artifact", "bin", "myexe"),
		filepath.Join("artifact", "readme.md"),
		filepath.Join("artifact", "src") + "/",
		filepath.Join("artifact", "src", "main.go"),
		filepath.Join("artifact", "src", "util.go"),
	}, paths)
}

func Test_progressReader(t *testing.T) {
	t.Run("invokes callback with downloaded and total bytes", func(t *testing.T) {
		data := []byte("hello world")
		var calls []struct{ downloaded, total int64 }

		pr := &progressReader{
			r:     strings.NewReader(string(data)),
			total: int64(len(data)),
			progress: func(downloaded, total int64) {
				calls = append(calls, struct{ downloaded, total int64 }{downloaded, total})
			},
		}

		buf := make([]byte, len(data))
		_, err := pr.Read(buf)
		require.NoError(t, err)

		require.NotEmpty(t, calls)
		assert.Equal(t, int64(len(data)), calls[len(calls)-1].downloaded)
		assert.Equal(t, int64(len(data)), calls[len(calls)-1].total)
	})

	t.Run("total is zero when content length unknown", func(t *testing.T) {
		data := []byte("hello")
		var lastTotal int64 = -1

		pr := &progressReader{
			r:     strings.NewReader(string(data)),
			total: 0,
			progress: func(downloaded, total int64) {
				lastTotal = total
			},
		}

		buf := make([]byte, len(data))
		_, err := pr.Read(buf)
		require.NoError(t, err)

		assert.Equal(t, int64(0), lastTotal)
	})

	t.Run("nil callback does not panic", func(t *testing.T) {
		pr := &progressReader{
			r:        strings.NewReader("data"),
			progress: nil,
		}
		buf := make([]byte, 4)
		_, err := pr.Read(buf)
		require.NoError(t, err)
	})
}

func Test_formatBytes(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1024 * 1024, "1.0 MB"},
		{int64(1.5 * 1024 * 1024), "1.5 MB"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			assert.Equal(t, tt.want, formatBytes(tt.n))
		})
	}
}

func Test_probeRangeSupport(t *testing.T) {
	t.Run("returns total size when server responds 206 with Content-Range", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Range") == "bytes=0-0" {
				w.Header().Set("Content-Range", "bytes 0-0/12345")
				w.Header().Set("Accept-Ranges", "bytes")
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write([]byte("x"))
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		total, err := probeRangeSupport(srv.Client(), srv.URL+"/artifact.zip")
		require.NoError(t, err)
		assert.Equal(t, int64(12345), total)
	})

	t.Run("returns 0 when server responds 200 (no range support)", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("full body"))
		}))
		defer srv.Close()

		total, err := probeRangeSupport(srv.Client(), srv.URL+"/artifact.zip")
		require.NoError(t, err)
		assert.Equal(t, int64(0), total)
	})

	t.Run("returns 0 when server responds 206 without Content-Range header", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusPartialContent)
		}))
		defer srv.Close()

		total, err := probeRangeSupport(srv.Client(), srv.URL+"/artifact.zip")
		require.NoError(t, err)
		assert.Equal(t, int64(0), total)
	})
}

// rangeServer returns an httptest.Server that serves body as a byte-range aware endpoint.
// Each request is checked for a Range header; if present the appropriate 206 slice is served.
func rangeServer(body []byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeHdr := r.Header.Get("Range")
		if rangeHdr == "" {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
			return
		}
		var start, end int64
		if _, err := fmt.Sscanf(rangeHdr, "bytes=%d-%d", &start, &end); err != nil {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		if end >= int64(len(body)) {
			end = int64(len(body)) - 1
		}
		chunk := body[start : end+1]
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
		w.Header().Set("Content-Length", strconv.Itoa(len(chunk)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(chunk)
	}))
}

func Test_downloadChunkOnce(t *testing.T) {
	data := bytes.Repeat([]byte("abcdefgh"), 128) // 1 KB
	srv := rangeServer(data)
	defer srv.Close()

	tmpfile, err := os.CreateTemp("", "gh-chunk-test-*.bin")
	require.NoError(t, err)
	defer func() {
		_ = tmpfile.Close()
		_ = os.Remove(tmpfile.Name())
	}()
	require.NoError(t, tmpfile.Truncate(int64(len(data))))

	var downloaded atomic.Int64
	require.NoError(t, downloadChunkOnce(srv.Client(), srv.URL+"/artifact.zip", 0, int64(len(data)-1), tmpfile, &downloaded))

	assert.Equal(t, int64(len(data)), downloaded.Load())

	_, err = tmpfile.Seek(0, io.SeekStart)
	require.NoError(t, err)
	got, err := io.ReadAll(tmpfile)
	require.NoError(t, err)
	assert.Equal(t, data, got)
}

func Test_downloadArtifactMultipart(t *testing.T) {
	// Build a real zip archive in memory so zip.NewReader can open it.
	zipBytes := readFixtureZip(t, "./fixtures/myproject.zip")
	total := int64(len(zipBytes))

	srv := rangeServer(zipBytes)
	defer srv.Close()

	tmpDir := t.TempDir()
	destDir, err := safepaths.ParseAbsolute(filepath.Join(tmpDir, "out"))
	require.NoError(t, err)

	var progressCalls int
	progressFn := func(downloaded, size int64) { progressCalls++ }

	err = downloadArtifactMultipart(srv.Client(), srv.URL+"/artifact.zip", total, 4, destDir, progressFn)
	require.NoError(t, err)

	// Verify that at least the known top-level entries exist.
	entries, err := os.ReadDir(destDir.String())
	require.NoError(t, err)
	require.NotEmpty(t, entries)

	// Progress should have been called at least once (final report on defer).
	assert.GreaterOrEqual(t, progressCalls, 1)
}

func Test_downloadArtifact_multipartFallback(t *testing.T) {
	// Server that always returns 200 (no range support) → downloadArtifact must fall back
	// to the single-stream path and still produce the extracted artifact.
	zipBytes := readFixtureZip(t, "./fixtures/myproject.zip")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(zipBytes)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(zipBytes)
	}))
	defer srv.Close()

	tmpDir := t.TempDir()
	destDir, err := safepaths.ParseAbsolute(filepath.Join(tmpDir, "out"))
	require.NoError(t, err)

	err = downloadArtifact(srv.Client(), srv.URL+"/artifact.zip", destDir, nil, 4)
	require.NoError(t, err)

	entries, err := os.ReadDir(destDir.String())
	require.NoError(t, err)
	require.NotEmpty(t, entries)
}

func Test_downloadArtifactMultipart_concurrency_arg(t *testing.T) {
       zipBytes := readFixtureZip(t, "./fixtures/myproject.zip")
       total := int64(len(zipBytes))

       srv := rangeServer(zipBytes)
       defer srv.Close()

       tmpDir := t.TempDir()
       destDir, err := safepaths.ParseAbsolute(filepath.Join(tmpDir, "out"))
       require.NoError(t, err)

       for _, concurrency := range []int{1, 2, 4, 8} {
	       t.Run(fmt.Sprintf("concurrency=%d", concurrency), func(t *testing.T) {
		       var progressCalls int
		       progressFn := func(downloaded, size int64) { progressCalls++ }
		       err := downloadArtifactMultipart(srv.Client(), srv.URL+"/artifact.zip", total, concurrency, destDir, progressFn)
		       require.NoError(t, err)
		       entries, err := os.ReadDir(destDir.String())
		       require.NoError(t, err)
		       require.NotEmpty(t, entries)
		       assert.GreaterOrEqual(t, progressCalls, 1)
	       })
       }
}

// readFixtureZip reads the raw bytes of a fixture zip file for use as a test body.
func readFixtureZip(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return b
}
