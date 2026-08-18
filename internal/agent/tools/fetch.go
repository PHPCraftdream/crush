package tools

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/fantasy"
	md "github.com/JohannesKaufmann/html-to-markdown"
	"github.com/PuerkitoBio/goquery"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/stringext"
)

const (
	FetchToolName = "fetch"
	MaxFetchSize  = 100 * 1024 // 100KB
)

//go:embed fetch.md.tpl
var fetchDescriptionTmpl []byte

var fetchDescriptionTpl = template.Must(
	template.New("fetchDescription").
		Parse(string(fetchDescriptionTmpl)),
)

type fetchDescriptionData struct {
	GhAvailable    bool
	MaxFetchSizeKB int
}

func fetchDescription() string {
	return renderTemplate(fetchDescriptionTpl, fetchDescriptionData{
		GhAvailable:    ghAvailable,
		MaxFetchSizeKB: MaxFetchSize / 1024,
	})
}

func NewFetchTool(permissions permission.Service, workingDir string, client *http.Client) fantasy.AgentTool {
	if client == nil {
		// Default client is SSRF-guarded: it blocks dials to
		// loopback/private/link-local/metadata ranges so a
		// prompt-injected or malicious model can't exfiltrate cloud
		// metadata (169.254.169.254 et al.) via a fetch. A caller that
		// legitimately needs loopback passes its own client built with
		// NewSSRFGuardedClient(timeout, true).
		client = NewSSRFGuardedClient(30*time.Second, false)
	}

	return fantasy.NewParallelAgentTool(
		FetchToolName,
		fetchDescription(),
		func(ctx context.Context, params FetchParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.URL == "" {
				return fantasy.NewTextErrorResponse("URL parameter is required"), nil
			}

			format := strings.ToLower(params.Format)
			if format != "text" && format != "markdown" && format != "html" {
				return fantasy.NewTextErrorResponse("Format must be one of: text, markdown, html"), nil
			}

			if !strings.HasPrefix(params.URL, "http://") && !strings.HasPrefix(params.URL, "https://") {
				return fantasy.NewTextErrorResponse("URL must start with http:// or https://"), nil
			}

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for creating a new file")
			}

			p, err := permissions.Request(
				ctx,
				permission.CreatePermissionRequest{
					SessionID:   sessionID,
					Path:        workingDir,
					ToolCallID:  call.ID,
					ToolName:    FetchToolName,
					Action:      "fetch",
					Description: fmt.Sprintf("Fetch content from URL: %s", params.URL),
					Params:      FetchPermissionsParams(params),
				},
			)
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if !p {
				return NewPermissionDeniedResponse(), nil
			}

			// maxFetchTimeoutSeconds is the maximum allowed timeout for fetch requests (2 minutes)
			const maxFetchTimeoutSeconds = 120

			// Handle timeout with context
			requestCtx := ctx
			if params.Timeout > 0 {
				if params.Timeout > maxFetchTimeoutSeconds {
					params.Timeout = maxFetchTimeoutSeconds
				}
				var cancel context.CancelFunc
				requestCtx, cancel = context.WithTimeout(ctx, time.Duration(params.Timeout)*time.Second)
				defer cancel()
			}

			req, err := http.NewRequestWithContext(requestCtx, "GET", params.URL, nil)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"malformed URL %q: %v. The URL could not be parsed into a valid "+
						"HTTP request, so retrying it unchanged will fail the same "+
						"way — fix the url parameter (a well-formed absolute "+
						"http:// or https:// URL: no spaces, valid percent-escapes, "+
						"intact host) and call again.",
					params.URL, err,
				)), nil
			}

			req.Header.Set("User-Agent", "crush/1.0")

			resp, err := client.Do(req)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"fetch of %s failed at the network level: %v. The URL itself "+
						"is well-formed; the request just never completed — "+
						"unreachable host, DNS failure, connection refused or reset, "+
						"TLS error, timeout, or a blocked destination. That may be "+
						"transient: retry the same URL later, or try a different one.",
					params.URL, err,
				)), nil
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Request failed with status code: %d", resp.StatusCode)), nil
			}

			body, err := io.ReadAll(io.LimitReader(resp.Body, MaxFetchSize))
			if err != nil {
				return fantasy.NewTextErrorResponse("Failed to read response body: " + err.Error()), nil
			}

			content := string(body)

			validUTF8 := utf8.ValidString(content)
			if !validUTF8 {
				return fantasy.NewTextErrorResponse("Response content is not valid UTF-8"), nil
			}
			contentType := resp.Header.Get("Content-Type")

			switch format {
			case "text":
				if strings.Contains(contentType, "text/html") {
					text, err := extractTextFromHTML(content)
					if err != nil {
						return fantasy.NewTextErrorResponse("Failed to extract text from HTML: " + err.Error()), nil
					}
					content = text
				}

			case "markdown":
				if strings.Contains(contentType, "text/html") {
					markdown, err := convertHTMLToMarkdown(content)
					if err != nil {
						return fantasy.NewTextErrorResponse("Failed to convert HTML to Markdown: " + err.Error()), nil
					}
					content = markdown
				}

				content = "```\n" + content + "\n```"

			case "html":
				// return only the body of the HTML document
				if strings.Contains(contentType, "text/html") {
					doc, err := goquery.NewDocumentFromReader(strings.NewReader(content))
					if err != nil {
						return fantasy.NewTextErrorResponse("Failed to parse HTML: " + err.Error()), nil
					}
					body, err := doc.Find("body").Html()
					if err != nil {
						return fantasy.NewTextErrorResponse("Failed to extract body from HTML: " + err.Error()), nil
					}
					if body == "" {
						return fantasy.NewTextErrorResponse("No body content found in HTML"), nil
					}
					content = "<html>\n<body>\n" + body + "\n</body>\n</html>"
				}
			}
			// truncate content if it exceeds max read size
			if int64(len(content)) > MaxFetchSize {
				content = stringext.Truncate(content, int(MaxFetchSize))
				content += fmt.Sprintf("\n\n[Content truncated to %d bytes]", MaxFetchSize)
			}

			return fantasy.NewTextResponse(content), nil
		},
	)
}

func extractTextFromHTML(html string) (string, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return "", err
	}

	text := doc.Find("body").Text()
	text = strings.Join(strings.Fields(text), " ")

	return text, nil
}

func convertHTMLToMarkdown(html string) (string, error) {
	converter := md.NewConverter("", true, nil)

	markdown, err := converter.ConvertString(html)
	if err != nil {
		return "", err
	}

	return markdown, nil
}
