package tools

import (
	"cmp"
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/filepathext"
	"github.com/charmbracelet/crush/internal/permission"
)

type DownloadParams struct {
	URL      string `json:"url" description:"The URL to download from"`
	FilePath string `json:"file_path" description:"The local file path where the downloaded content should be saved"`
	Timeout  int    `json:"timeout,omitempty" description:"Optional timeout in seconds (max 600)"`
}

type DownloadPermissionsParams struct {
	URL      string `json:"url"`
	FilePath string `json:"file_path"`
	Timeout  int    `json:"timeout,omitempty"`
}

const DownloadToolName = "download"

// MaxDownloadBytes bounds how much of the response body download will
// write to disk. Found missing by a full-project @crush --role reviewer
// audit: without a limit, a URL serving an unbounded stream (or a slow-drip
// server exploiting Timeout=0, which only bounds the HTTP client's overall
// deadline, not response size) could fill the disk. 500 MiB is generous for
// the tool's normal use (docs, small datasets, archives) while still being
// a hard ceiling.
const MaxDownloadBytes = 500 * 1024 * 1024

//go:embed download.md.tpl
var downloadDescriptionTmpl []byte

var downloadDescriptionTpl = template.Must(
	template.New("downloadDescription").
		Parse(string(downloadDescriptionTmpl)),
)

type downloadDescriptionData struct {
	MaxDownloadTimeout int
}

func downloadDescription() string {
	return renderTemplate(downloadDescriptionTpl, downloadDescriptionData{
		MaxDownloadTimeout: 600,
	})
}

func NewDownloadTool(permissions permission.Service, workingDir string, client *http.Client) fantasy.AgentTool {
	if client == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxIdleConns = 100
		transport.MaxIdleConnsPerHost = 10
		transport.IdleConnTimeout = 90 * time.Second

		client = &http.Client{
			Timeout:   5 * time.Minute, // Default 5 minute timeout for downloads
			Transport: transport,
		}
	}
	return fantasy.NewParallelAgentTool(
		DownloadToolName,
		downloadDescription(),
		func(ctx context.Context, params DownloadParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.URL == "" {
				return fantasy.NewTextErrorResponse("URL parameter is required"), nil
			}

			if params.FilePath == "" {
				return fantasy.NewTextErrorResponse("file_path parameter is required"), nil
			}

			if !strings.HasPrefix(params.URL, "http://") && !strings.HasPrefix(params.URL, "https://") {
				return fantasy.NewTextErrorResponse("URL must start with http:// or https://"), nil
			}

			filePath := filepathext.SmartJoin(workingDir, params.FilePath)
			relPath, _ := filepath.Rel(workingDir, filePath)
			relPath = filepath.ToSlash(cmp.Or(relPath, filePath))

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for downloading files")
			}

			p, err := permissions.Request(
				ctx,
				permission.CreatePermissionRequest{
					SessionID:   sessionID,
					Path:        filePath,
					ToolName:    DownloadToolName,
					Action:      "download",
					Description: fmt.Sprintf("Download file from URL: %s to %s", params.URL, filePath),
					Params:      DownloadPermissionsParams(params),
				},
			)
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if !p {
				return NewPermissionDeniedResponse(), nil
			}

			// Handle timeout with context
			requestCtx := ctx
			if params.Timeout > 0 {
				maxTimeout := 600 // 10 minutes
				if params.Timeout > maxTimeout {
					params.Timeout = maxTimeout
				}
				var cancel context.CancelFunc
				requestCtx, cancel = context.WithTimeout(ctx, time.Duration(params.Timeout)*time.Second)
				defer cancel()
			}

			req, err := http.NewRequestWithContext(requestCtx, "GET", params.URL, nil)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to create request: %w", err)
			}

			req.Header.Set("User-Agent", "crush/1.0")

			resp, err := client.Do(req)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to download from URL: %w", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Request failed with status code: %d", resp.StatusCode)), nil
			}

			// Create parent directories if they don't exist
			if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to create parent directories: %w", err)
			}

			// Write to a temp file in the same directory, then rename over
			// filePath — a network failure or ctx-cancel mid-copy must
			// never leave a truncated file at filePath for a later
			// view/edit to read as valid content. Mirrors
			// fsext.AtomicWriteFile's pattern, but streaming (via io.Copy
			// from a size-limited reader) since downloads can be large
			// enough that buffering the whole body first would be wasteful.
			tmpFile, err := os.CreateTemp(filepath.Dir(filePath), filepath.Base(filePath)+".*.tmp")
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to create output file: %w", err)
			}
			tmpPath := tmpFile.Name()
			cleanupTmp := func() { _ = os.Remove(tmpPath) }

			// Read one byte past the limit so an oversized body is detected
			// (bytesWritten > MaxDownloadBytes) rather than silently
			// truncated to exactly the limit and reported as a "complete"
			// download.
			bytesWritten, err := io.Copy(tmpFile, io.LimitReader(resp.Body, MaxDownloadBytes+1))
			if err != nil {
				tmpFile.Close()
				cleanupTmp()
				return fantasy.ToolResponse{}, fmt.Errorf("failed to write file: %w", err)
			}
			if bytesWritten > MaxDownloadBytes {
				tmpFile.Close()
				cleanupTmp()
				return fantasy.NewTextErrorResponse(fmt.Sprintf("download exceeds the %d byte limit", MaxDownloadBytes)), nil
			}
			if err := tmpFile.Sync(); err != nil {
				tmpFile.Close()
				cleanupTmp()
				return fantasy.ToolResponse{}, fmt.Errorf("failed to flush output file: %w", err)
			}
			if err := tmpFile.Close(); err != nil {
				cleanupTmp()
				return fantasy.ToolResponse{}, fmt.Errorf("failed to close output file: %w", err)
			}
			if err := os.Rename(tmpPath, filePath); err != nil {
				cleanupTmp()
				return fantasy.ToolResponse{}, fmt.Errorf("failed to finalize downloaded file: %w", err)
			}

			contentType := resp.Header.Get("Content-Type")
			responseMsg := fmt.Sprintf("Successfully downloaded %d bytes to %s", bytesWritten, relPath)
			if contentType != "" {
				responseMsg += fmt.Sprintf(" (Content-Type: %s)", contentType)
			}

			return fantasy.NewTextResponse(responseMsg), nil
		},
	)
}
