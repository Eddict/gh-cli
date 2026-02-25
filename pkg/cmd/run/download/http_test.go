package download

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/cli/cli/v2/internal/ghrepo"
	"github.com/cli/cli/v2/internal/safepaths"
	"github.com/cli/cli/v2/pkg/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_List(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	reg.Register(
		httpmock.REST("GET", "repos/OWNER/REPO/actions/runs/123/artifacts"),
		httpmock.StringResponse(`{
			"total_count": 2,
			"artifacts": [
				{"name": "artifact-1"},
				{"name": "artifact-2"}
			]
		}`))

	api := &apiPlatform{
		client: &http.Client{Transport: reg},
		repo:   ghrepo.New("OWNER", "REPO"),
	}
	artifacts, err := api.List("123")
	require.NoError(t, err)

	require.Equal(t, 2, len(artifacts))
	assert.Equal(t, "artifact-1", artifacts[0].Name)
	assert.Equal(t, "artifact-2", artifacts[1].Name)
}

func Test_List_perRepository(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	reg.Register(
		httpmock.REST("GET", "repos/OWNER/REPO/actions/artifacts"),
		httpmock.StringResponse(`{}`))

	api := &apiPlatform{
		client: &http.Client{Transport: reg},
		repo:   ghrepo.New("OWNER", "REPO"),
	}
	_, err := api.List("")
	require.NoError(t, err)
}

func Test_Download(t *testing.T) {
	tmpDir := t.TempDir()
	destDir, err := safepaths.ParseAbsolute(filepath.Join(tmpDir, "artifact"))
	require.NoError(t, err)

	reg := &httpmock.Registry{}
	defer reg.Verify(t)

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
