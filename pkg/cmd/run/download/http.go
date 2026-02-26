package download

import (
	"archive/zip"
	"fmt"
	"github.com/cli/cli/v2/api"
	"github.com/cli/cli/v2/internal/ghrepo"
	"github.com/cli/cli/v2/internal/safepaths"
	ghzip "github.com/cli/cli/v2/internal/zip"
	"github.com/cli/cli/v2/pkg/cmd/run/shared"
	"io"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

type apiPlatform struct {
	client *http.Client
	repo   ghrepo.Interface
}

// List implements the platform interface for artifact listing.
func (p *apiPlatform) List(runID string) ([]shared.Artifact, error) {
	return shared.ListArtifacts(p.client, p.repo, runID)
}

// Deprecated: use DownloadWithConcurrency
func (p *apiPlatform) Download(url string, dir safepaths.Absolute, progress func(downloaded, total int64)) error {
	return downloadArtifact(p.client, url, dir, progress, 4)
}

func (p *apiPlatform) DownloadWithConcurrency(url string, dir safepaths.Absolute, progress func(downloaded, total int64), concurrency int) error {
	return downloadArtifact(p.client, url, dir, progress, concurrency)
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

const (
	// multipartMinSize is the minimum artifact size (10 MB) required to engage the multipart
	// download path. Smaller artifacts are fetched with a single stream to avoid overhead.
	multipartMinSize = 10 * 1024 * 1024

	// multipartMaxRetry is the maximum number of additional attempts per chunk on transient error.
	multipartMaxRetry = 3
)

// multipartConcurrency is now passed as an argument from the CLI.

// probeRangeSupport sends a minimal GET request with "Range: bytes=0-0" to determine
// whether the server (after following redirects) supports HTTP byte-range requests.
// It returns the total content size when range support is confirmed, or 0 otherwise.
func probeRangeSupport(client *http.Client, url string) (int64, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", "bytes=0-0")

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusPartialContent {
		return 0, nil
	}

	// Content-Range: bytes 0-0/<total>
	cr := resp.Header.Get("Content-Range")
	if cr == "" {
		return 0, nil
	}
	var start, end, total int64
	if _, err := fmt.Sscanf(cr, "bytes %d-%d/%d", &start, &end, &total); err != nil || total <= 0 {
		return 0, nil
	}
	return total, nil
}

// downloadChunkOnce downloads the byte range [start, end] from url and writes it to f at
// offset start, accumulating the byte count into downloaded.
func downloadChunkOnce(client *http.Client, url string, start, end int64, f *os.File, downloaded *atomic.Int64) error {
	debugLogf("downloadChunkOnce: start=%d end=%d", start, end)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("unexpected status %d for byte-range request", resp.StatusCode)
	}

	expected := end - start + 1
	buf := make([]byte, 32*1024)
	var written int64
	offset := start

	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.WriteAt(buf[:n], offset); werr != nil {
				return werr
			}
			offset += int64(n)
			written += int64(n)
			downloaded.Add(int64(n))
		}
		if err == io.EOF {
			debugLogf("downloadChunkOnce: completed chunk %d-%d, written=%d", start, end, written)
			break
		}
		if err != nil {
			debugLogf("downloadChunkOnce: error in chunk %d-%d: %v", start, end, err)
			return err
		}
	}

	if written != expected {
		return fmt.Errorf("chunk %d-%d: expected %d bytes, received %d", start, end, expected, written)
	}
	return nil
}

// downloadChunk retries downloadChunkOnce up to multipartMaxRetry additional times on
// transient error, using a linearly increasing backoff between attempts.
func downloadChunk(client *http.Client, url string, start, end int64, f *os.File, downloaded *atomic.Int64) error {
	var lastErr error
	for attempt := 0; attempt <= multipartMaxRetry; attempt++ {
		debugLogf("downloadChunk: attempt=%d chunk=%d-%d", attempt, start, end)
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
		}
		if err := downloadChunkOnce(client, url, start, end, f, downloaded); err == nil {
			debugLogf("downloadChunk: success chunk=%d-%d on attempt=%d", start, end, attempt)
			return nil
		} else {
			debugLogf("downloadChunk: error chunk=%d-%d on attempt=%d: %v", start, end, attempt, err)
			lastErr = err
		}
	}
	debugLogf("downloadChunk: failed chunk=%d-%d after %d attempts", start, end, multipartMaxRetry+1)
	return lastErr
}

// downloadArtifactMultipart downloads the artifact at url in concurrent byte-range chunks,
// writing each chunk at the correct offset in a pre-sized temporary file before extraction.
// Progress is reported to the provided callback at progressInterval intervals.
func downloadArtifactMultipart(httpClient *http.Client, url string, totalSize int64, concurrency int, destDir safepaths.Absolute, progress func(downloaded, total int64)) error {
	debugLogf("downloadArtifactMultipart: url=%s totalSize=%d concurrency=%d", url, totalSize, concurrency)
	tmpfile, err := os.CreateTemp("", "gh-artifact.*.zip")
	if err != nil {
		return fmt.Errorf("error initializing temporary file: %w", err)
	}
	defer func() {
		_ = tmpfile.Close()
		_ = os.Remove(tmpfile.Name())
	}()

	// Pre-size the file so concurrent WriteAt calls land in valid regions.
	if err := tmpfile.Truncate(totalSize); err != nil {
		return fmt.Errorf("error pre-sizing temporary file: %w", err)
	}

	// Compute chunk boundaries.
	chunkSize := (totalSize + int64(concurrency) - 1) / int64(concurrency)
	type chunkRange struct{ start, end int64 }
	var chunks []chunkRange
	for start := int64(0); start < totalSize; start += chunkSize {
		end := start + chunkSize - 1
		if end >= totalSize {
			end = totalSize - 1
		}
		chunks = append(chunks, chunkRange{start, end})
	}

	var totalDownloaded atomic.Int64

	// Drive progress reporting from a ticker so the callback is invoked at most every
	// progressInterval regardless of how many goroutines are writing concurrently.
	if progress != nil {
		ticker := time.NewTicker(progressInterval)
		done := make(chan struct{})
		go func() {
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					progress(totalDownloaded.Load(), totalSize)
				case <-done:
					return
				}
			}
		}()
		defer func() {
			close(done)
			progress(totalDownloaded.Load(), totalSize)
		}()
	}

	// Download chunks concurrently, bounded by concurrency.
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	errs := make([]error, len(chunks))

	for i, ch := range chunks {
		wg.Add(1)
		sem <- struct{}{}
		debugLogf("downloadArtifactMultipart: starting chunk %d-%d (index %d)", ch.start, ch.end, i)
		go func(i int, start, end int64) {
			defer wg.Done()
			defer func() { <-sem }()
			errs[i] = downloadChunk(httpClient, url, start, end, tmpfile, &totalDownloaded)
		}(i, ch.start, ch.end)
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			debugLogf("downloadArtifactMultipart: error in chunk: %v", err)
			return err
		}
	}
	debugLogf("downloadArtifactMultipart: all chunks complete, extracting zip")

	zipfile, err := zip.NewReader(tmpfile, totalSize)
	if err != nil {
		return fmt.Errorf("error extracting zip archive: %w", err)
	}
	// Remove only files/dirs that will be overwritten by extraction
		       for _, zf := range zipfile.File {
			       fpath, err := destDir.Join(zf.Name)
			       if err != nil {
				       var pathTraversalError safepaths.PathTraversalError
				       if errors.As(err, &pathTraversalError) {
					       continue
				       }
				       return fmt.Errorf("error preparing extraction for %q: %w", zf.Name, err)
			       }
			       // Never remove the root extraction directory itself
			       if fpath.String() == destDir.String() {
				       continue
			       }
			       if info, statErr := os.Stat(fpath.String()); statErr == nil {
				       if !info.IsDir() {
					       _ = os.Remove(fpath.String())
				       }
			       }
		       }
	if err := ghzip.ExtractZip(zipfile, destDir); err != nil {
		return fmt.Errorf("error extracting zip archive: %w", err)
	}
	return nil
}

// downloadArtifactSingleStream fetches the artifact at url via a single HTTP stream and
// extracts the resulting zip into destDir. This is the fallback path when byte-range
// downloads are unavailable or fail.
func downloadArtifactSingleStream(httpClient *http.Client, url string, destDir safepaths.Absolute, progress func(downloaded, total int64)) error {
	debugLogf("downloadArtifactSingleStream: url=%s", url)
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
	// Remove only files/dirs that will be overwritten by extraction
		       for _, zf := range zipfile.File {
			       fpath, err := destDir.Join(zf.Name)
			       if err != nil {
				       var pathTraversalError safepaths.PathTraversalError
				       if errors.As(err, &pathTraversalError) {
					       continue
				       }
				       return fmt.Errorf("error preparing extraction for %q: %w", zf.Name, err)
			       }
			       // Never remove the root extraction directory itself
			       if fpath.String() == destDir.String() {
				       continue
			       }
			       if info, statErr := os.Stat(fpath.String()); statErr == nil {
				       if !info.IsDir() {
					       _ = os.Remove(fpath.String())
				       }
			       }
		       }
	if err := ghzip.ExtractZip(zipfile, destDir); err != nil {
		return fmt.Errorf("error extracting zip archive: %w", err)
	}
	return nil
}

// downloadArtifact downloads and extracts the artifact at url into destDir.
// It first probes the endpoint for HTTP byte-range support; when confirmed and the artifact
// is large enough, it downloads concurrently via downloadArtifactMultipart.
// Any failure in the multipart path causes a transparent fall-through to the single-stream path.
func downloadArtifact(httpClient *http.Client, url string, destDir safepaths.Absolute, progress func(downloaded, total int64), concurrency int) error {
	debugLogf("downloadArtifact: url=%s", url)

	if totalSize, err := probeRangeSupport(httpClient, url); err == nil && totalSize >= multipartMinSize {
		debugLogf("downloadArtifact: using multipart, totalSize=%d", totalSize)
		if err := downloadArtifactMultipart(httpClient, url, totalSize, concurrency, destDir, progress); err == nil {
			debugLogf("downloadArtifact: multipart download succeeded")
			return nil
		} else {
			debugLogf("downloadArtifact: multipart download failed: %v", err)
			// If the error is from extraction, log more details
			if err != nil {
				debugLogf("downloadArtifact: detailed multipart extraction failure: %T: %v", err, err)
			}
			debugLogf("downloadArtifact: multipart download failed, falling back to single stream")
		}
	}

	debugLogf("downloadArtifact: using single stream fallback")
	return downloadArtifactSingleStream(httpClient, url, destDir, progress)
}
