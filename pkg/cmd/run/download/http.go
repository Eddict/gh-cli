package download

import (
	"archive/zip"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/cli/cli/v2/api"
	"github.com/cli/cli/v2/internal/ghrepo"
	"github.com/cli/cli/v2/internal/safepaths"
	ghzip "github.com/cli/cli/v2/internal/zip"
	"github.com/cli/cli/v2/pkg/cmd/run/shared"
)

type apiPlatform struct {
	client *http.Client
	repo   ghrepo.Interface
}

func (p *apiPlatform) List(runID string) ([]shared.Artifact, error) {
	return shared.ListArtifacts(p.client, p.repo, runID)
}

func (p *apiPlatform) Download(url string, dir safepaths.Absolute, progress func(downloaded, total int64)) error {
	return downloadArtifact(p.client, url, dir, progress)
}

// progressReader wraps an io.Reader and invokes a callback at most every progressInterval
// to report bytes downloaded and the total size (0 if unknown).
type progressReader struct {
	r          io.Reader
	total      int64
	downloaded int64
	progress   func(downloaded, total int64)
	lastReport time.Time
}

const progressInterval = 100 * time.Millisecond

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.r.Read(p)
	pr.downloaded += int64(n)
	now := time.Now()
	if pr.progress != nil && (now.Sub(pr.lastReport) >= progressInterval || err == io.EOF) {
		pr.progress(pr.downloaded, pr.total)
		pr.lastReport = now
	}
	return n, err
}

func downloadArtifact(httpClient *http.Client, url string, destDir safepaths.Absolute, progress func(downloaded, total int64)) error {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	// The server rejects this :(
	//req.Header.Set("Accept", "application/zip")

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode > 299 {
		return api.HandleHTTPError(resp)
	}

	tmpfile, err := os.CreateTemp("", "gh-artifact.*.zip")
	if err != nil {
		return fmt.Errorf("error initializing temporary file: %w", err)
	}
	defer func() {
		_ = tmpfile.Close()
		_ = os.Remove(tmpfile.Name())
	}()

	var contentLength int64
	if resp.ContentLength > 0 {
		contentLength = resp.ContentLength
	}
	pr := &progressReader{
		r:        resp.Body,
		total:    contentLength,
		progress: progress,
	}

	size, err := io.Copy(tmpfile, pr)
	if err != nil {
		return fmt.Errorf("error writing zip archive: %w", err)
	}

	zipfile, err := zip.NewReader(tmpfile, size)
	if err != nil {
		return fmt.Errorf("error extracting zip archive: %w", err)
	}
	if err := ghzip.ExtractZip(zipfile, destDir); err != nil {
		return fmt.Errorf("error extracting zip archive: %w", err)
	}

	return nil
}
