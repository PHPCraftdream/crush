package tools

import (
	"bufio"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/filepathext"
	"github.com/charmbracelet/crush/internal/filetracker"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/skills"
	"github.com/charmbracelet/crush/internal/stringext"
)

//go:embed view.md.tpl
var viewDescriptionTmpl []byte

var viewDescriptionTpl = template.Must(
	template.New("viewDescription").
		Parse(string(viewDescriptionTmpl)),
)

type viewDescriptionData struct {
	DefaultReadLimit int
	MaxViewSizeKB    int
}

func viewDescription() string {
	return renderTemplate(viewDescriptionTpl, viewDescriptionData{
		DefaultReadLimit: DefaultReadLimit,
		MaxViewSizeKB:    MaxViewSize / 1024,
	})
}

type ViewParams struct {
	FilePath string `json:"file_path" description:"The path to the file to read"`
	Offset   int    `json:"offset,omitempty" description:"The line number to start reading from (0-based)"`
	Limit    int    `json:"limit,omitempty" description:"The number of lines to read (defaults to 500)"`
}

type ViewPermissionsParams struct {
	FilePath string `json:"file_path"`
	Offset   int    `json:"offset"`
	Limit    int    `json:"limit"`
}

type ViewResourceType string

const (
	ViewResourceUnset ViewResourceType = ""
	ViewResourceSkill ViewResourceType = "skill"
)

type ViewResponseMetadata struct {
	FilePath            string           `json:"file_path"`
	Content             string           `json:"content"`
	ResourceType        ViewResourceType `json:"resource_type,omitempty"`
	ResourceName        string           `json:"resource_name,omitempty"`
	ResourceDescription string           `json:"resource_description,omitempty"`
}

const (
	ViewToolName = "view"
	MaxViewSize  = 200 * 1024 // 200KB
	// Fork merge note (origin/main 1811bec2 "fix(prompts): tweak file reads
	// to encourage more targeted reads"): upstream cut the default from
	// 2000 to 200 to push the model toward offset/limit usage. We picked
	// 500 as a compromise — small enough to discourage "read everything",
	// large enough to cover most of our .go files in one pass so
	// cliprovider subprocess agents (claude/codex/gemini) don't have to
	// round-trip for every read.
	DefaultReadLimit = 500
	MaxLineLength    = 2000
)

type contentTooLargeError struct {
	Size int
	Max  int
}

func (e contentTooLargeError) Error() string {
	return fmt.Sprintf("content section is too large (%d bytes). Maximum size is %d bytes", e.Size, e.Max)
}

// osFailureIsFatal splits the residual OS errors of the stat/read sites in
// this package (view.go, edit.go, write.go, multiedit.go) by the
// retry-invariance criterion of the tool error contract in tools.go: fatal
// if and only if no resent path and no passage of time would make the call
// succeed. EIO, EROFS, ENOSPC and ENOMEM are failures of the storage or
// of the process itself — every path fails identically, so they stay at
// contract level 3 and end the run. Everything else the OS can say about
// a model-chosen path — EACCES/EPERM (no rights on that path), ENOTDIR
// (a component is a file), ELOOP, ENAMETOOLONG, EINVAL (e.g. a NUL byte
// in the path), ENOENT from a file that vanished between stat and read, a
// file locked by another process — is correctable by the model sending a
// different path, so those sites answer with a text-error response (level
// 1) instead of killing the run.
//
// Unknown errnos default to recoverable on purpose: the contract requires
// proof of retry-invariance before a failure may end the run, and an
// unclassified errno proves nothing.
//
// ON WINDOWS THIS FUNCTION ALWAYS RETURNS FALSE, which is worse than the
// earlier wording here admitted. EIO, EROFS, ENOSPC and ENOMEM are not
// Windows error codes at all; Go defines them there as synthetic
// APPLICATION_ERROR+n values (measured: 536870952, 536871024, 536870996,
// 536870991) that no native code can equal. ERROR_DISK_FULL(112),
// ERROR_HANDLE_DISK_FULL(39), ERROR_CRC(23), ERROR_IO_DEVICE(1117) and
// ERROR_WRITE_PROTECT(19) were all checked and all compare unequal. So
// this is a Unix-only split; on Windows it degenerates into demoting
// everything, including a full disk.
//
// Said plainly, because the contract in tools.go promises the opposite:
// on Windows an infrastructure failure that stays broken for the rest of
// the session reaches the operator only as repeated tool errors in the
// transcript, never through the run's fatal envelope. Closing this needs
// a windows-tagged classifier over the native codes above. Until then the
// demotion is still the lesser evil by the radius asymmetry — a demoted
// carrier failure costs turns that remain visible, a promoted path error
// costs the whole session — but it is a gap, not a rare edge case.
func osFailureIsFatal(err error) bool {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.EIO, syscall.EROFS, syscall.ENOSPC, syscall.ENOMEM:
			return true
		}
	}
	return false
}

func NewViewTool(
	permissions permission.Service,
	filetracker filetracker.Service,
	skillTracker *skills.Tracker,
	workingDir string,
	skillsPaths ...string,
) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		ViewToolName,
		viewDescription(),
		func(ctx context.Context, params ViewParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.FilePath == "" {
				return fantasy.NewTextErrorResponse("file_path is required"), nil
			}

			// Handle builtin skill files (crush: prefix).
			if strings.HasPrefix(params.FilePath, skills.BuiltinPrefix) {
				resp, err := readBuiltinFile(params, skillTracker)
				return resp, err
			}

			// Handle relative paths
			filePath := filepathext.SmartJoin(workingDir, params.FilePath)

			// Check if file is outside working directory and request permission if needed
			absWorkingDir, err := filepath.Abs(workingDir)
			if err != nil {
				// workingDir is tool wiring, not model input, so strictly
				// this failure is retry-invariant. It still answers as a
				// response, matching ls.go's identical site, because the
				// likeliest cause — the process cwd having been deleted or
				// unmounted — is something the operator can repair while
				// the session stays alive.
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"Error resolving the working directory %q: %v. The session's working directory cannot be resolved to an absolute path — it may have been deleted or unmounted. No file was read. This is an environment problem for the operator, not a path problem.",
					workingDir, err)), nil
			}

			absFilePath, err := filepath.Abs(filePath)
			if err != nil {
				// The model's own path string made the OS refuse to even
				// resolve it; that is correctable input, so level 1.
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"The OS rejected the file path %q: %v. The path string itself is invalid — common causes are an embedded NUL or other control character, a name too long for the filesystem, or a malformed path component. Nothing was read. Resend the path built from regular characters and valid separators.",
					params.FilePath, err)), nil
			}

			relPath, err := filepath.Rel(absWorkingDir, absFilePath)
			isOutsideWorkDir := err != nil || strings.HasPrefix(relPath, "..")
			isSkillFile := isInSkillsPath(absFilePath, skillsPaths)

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				// Deliberately unconditional, unlike ls.go where the check
				// lives inside the outside-workdir branch: a successful read
				// records the file in the session's file tracker at the
				// bottom of this function, so the session ID is needed on
				// the common path too, not only for the permission request.
				// A missing session ID is wiring, invariant to retry, so it
				// stays fatal (level 3).
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for recording file reads and for accessing files outside the working directory")
			}

			// Request permission for files outside working directory, unless it's a skill file.
			if isOutsideWorkDir && !isSkillFile {
				granted, permReqErr := permissions.Request(
					ctx,
					permission.CreatePermissionRequest{
						SessionID:   sessionID,
						Path:        absFilePath,
						ToolCallID:  call.ID,
						ToolName:    ViewToolName,
						Action:      "read",
						Description: fmt.Sprintf("Read file outside working directory: %s", absFilePath),
						Params:      ViewPermissionsParams(params),
					},
				)
				if permReqErr != nil {
					return fantasy.ToolResponse{}, permReqErr
				}
				if !granted {
					return NewPermissionDeniedResponse(), nil
				}
			}

			// Check if file exists
			fileInfo, err := os.Stat(filePath)
			if err != nil {
				if os.IsNotExist(err) {
					// Try to offer suggestions for similarly named files
					dir := filepath.Dir(filePath)
					base := filepath.Base(filePath)

					dirEntries, dirErr := os.ReadDir(dir)
					if dirErr == nil {
						var suggestions []string
						for _, entry := range dirEntries {
							if strings.Contains(strings.ToLower(entry.Name()), strings.ToLower(base)) ||
								strings.Contains(strings.ToLower(base), strings.ToLower(entry.Name())) {
								suggestions = append(suggestions, filepath.Join(dir, entry.Name()))
								if len(suggestions) >= 3 {
									break
								}
							}
						}

						if len(suggestions) > 0 {
							return fantasy.NewTextErrorResponse(fmt.Sprintf("File not found: %s\n\nDid you mean one of these?\n%s",
								filePath, strings.Join(suggestions, "\n"))), nil
						}
					}

					return fantasy.NewTextErrorResponse(fmt.Sprintf("File not found: %s", filePath)), nil
				}
				if osFailureIsFatal(err) {
					return fantasy.ToolResponse{}, fmt.Errorf("error accessing file: %w", err)
				}
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"Cannot access %s: %v. The OS rejected this path — common causes are no read permission somewhere along the path, a path component that is a file rather than a directory, a symlink loop, or a name too long for the filesystem. Nothing was read. Try a different path or a corrected form of this one.",
					filePath, err)), nil
			}

			// Check if it's a directory
			if fileInfo.IsDir() {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Path is a directory, not a file: %s", filePath)), nil
			}

			// Set default limit if not provided (no limit for SKILL.md files)
			if params.Limit <= 0 {
				if isSkillFile {
					params.Limit = 1000000 // Effectively no limit for skill files
				} else {
					params.Limit = DefaultReadLimit
				}
			}

			isSupportedImage, mimeType := getImageMimeType(filePath)
			if isSupportedImage {
				if fileInfo.Size() > MaxViewSize {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("Image file is too large (%d bytes). Maximum size is %d bytes",
						fileInfo.Size(), MaxViewSize)), nil
				}
				if !GetSupportsImagesFromContext(ctx) {
					modelName := GetModelNameFromContext(ctx)
					return fantasy.NewTextErrorResponse(fmt.Sprintf("This model (%s) does not support image data.", modelName)), nil
				}

				imageData, readErr := os.ReadFile(filePath)
				if readErr != nil {
					if osFailureIsFatal(readErr) {
						return fantasy.ToolResponse{}, fmt.Errorf("error reading image file: %w", readErr)
					}
					return fantasy.NewTextErrorResponse(fmt.Sprintf(
						"Cannot read image file %s: %v. The file was found but could not be read — it may have been deleted or locked by another process since it was found, or the path lacks read permission. No image content was returned. Try viewing it again or use a different path.",
						filePath, readErr)), nil
				}

				// Some tools save files with a mismatched extension
				// (e.g. pinchtab writes JPEG bytes to a .png file).
				// Providers like Anthropic strictly validate the
				// media type against the base64 magic bytes and 400
				// on mismatch, so prefer the sniffed type whenever
				// it identifies a supported image format.
				mimeType = sniffImageMimeType(imageData, mimeType)

				return fantasy.NewImageResponse(imageData, mimeType), nil
			}

			// Read the file content
			maxContentSize := MaxViewSize
			if isSkillFile {
				maxContentSize = 0
			}
			content, hasMore, err := readTextFile(filePath, params.Offset, params.Limit, maxContentSize)
			if err != nil {
				var tooLarge contentTooLargeError
				if errors.As(err, &tooLarge) {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("Content section is too large (%d bytes). Maximum size is %d bytes",
						tooLarge.Size, tooLarge.Max)), nil
				}
				if osFailureIsFatal(err) {
					return fantasy.ToolResponse{}, fmt.Errorf("error reading file: %w", err)
				}
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"Cannot read %s: %v. The file was found but could not be read — it may have been deleted or locked by another process since it was found, or the path lacks read permission. No file content was returned. Try viewing it again or use a different path.",
					filePath, err)), nil
			}
			if !utf8.ValidString(content) {
				return fantasy.NewTextErrorResponse("File content is not valid UTF-8"), nil
			}

			output := "<file>\n"
			output += addLineNumbers(content, params.Offset+1)

			if hasMore {
				output += fmt.Sprintf("\n\n(File has more lines. Use 'offset' parameter to read beyond line %d)",
					params.Offset+len(strings.Split(content, "\n")))
			}
			output += "\n</file>\n"
			filetracker.RecordRead(ctx, sessionID, filePath)

			meta := ViewResponseMetadata{
				FilePath: filePath,
				Content:  content,
			}
			if isSkillFile {
				if skill, err := skills.Parse(filePath); err == nil {
					meta.ResourceType = ViewResourceSkill
					meta.ResourceName = skill.Name
					meta.ResourceDescription = skill.Description
					skillTracker.MarkLoaded(skill.Name)
				}
			}

			return fantasy.WithResponseMetadata(
				fantasy.NewTextResponse(output),
				meta,
			), nil
		},
	)
}

func addLineNumbers(content string, startLine int) string {
	if content == "" {
		return ""
	}

	lines := strings.Split(content, "\n")

	var result []string
	for i, line := range lines {
		line = strings.TrimSuffix(line, "\r")

		lineNum := i + startLine
		numStr := fmt.Sprintf("%d", lineNum)

		if len(numStr) >= 6 {
			result = append(result, fmt.Sprintf("%s|%s", numStr, line))
		} else {
			paddedNum := fmt.Sprintf("%6s", numStr)
			result = append(result, fmt.Sprintf("%s|%s", paddedNum, line))
		}
	}

	return strings.Join(result, "\n")
}

func readTextFile(filePath string, offset, limit, maxContentSize int) (string, bool, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", false, err
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	skipped := 0
	for skipped < offset {
		_, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				return "", false, nil
			}
			return "", false, err
		}
		skipped++
	}

	lines := make([]string, 0, min(limit, DefaultReadLimit))
	contentSize := 0

	for len(lines) < limit {
		lineText, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return "", false, err
		}
		lineText = strings.TrimSuffix(lineText, "\n")
		lineText = strings.TrimSuffix(lineText, "\r")
		if len(lineText) > MaxLineLength {
			lineText = stringext.Truncate(lineText, MaxLineLength) + "..."
		}
		projectedSize := contentSize + len(lineText)
		if len(lines) > 0 {
			projectedSize++
		}
		if maxContentSize > 0 && projectedSize > maxContentSize {
			return "", false, contentTooLargeError{Size: projectedSize, Max: maxContentSize}
		}
		contentSize = projectedSize
		lines = append(lines, lineText)
		if err == io.EOF {
			break
		}
	}

	// Peek one more line only when we filled the limit.
	hasMore := false
	if len(lines) == limit {
		lineText, peekErr := reader.ReadString('\n')
		hasMore = len(lineText) > 0 || peekErr == nil
	}

	return strings.Join(lines, "\n"), hasMore, nil
}

func getImageMimeType(filePath string) (bool, string) {
	ext := strings.ToLower(filepath.Ext(filePath))
	switch ext {
	case ".jpg", ".jpeg":
		return true, "image/jpeg"
	case ".png":
		return true, "image/png"
	case ".gif":
		return true, "image/gif"
	case ".webp":
		return true, "image/webp"
	default:
		return false, ""
	}
}

// sniffImageMimeType returns the content-sniffed MIME type when it identifies
// a supported image format. Otherwise it returns the provided fallback, which
// is usually the extension-derived type. Providers that validate the image
// media type against the base64 magic bytes (e.g. Anthropic) reject mismatched
// requests with a 400, so trusting the filename alone is unsafe.
func sniffImageMimeType(data []byte, fallback string) string {
	sniffed := http.DetectContentType(data)
	// http.DetectContentType may return the MIME with a ";" parameter
	// (e.g. "image/svg+xml; charset=utf-8") although current image sniffers
	// return bare types; strip defensively.
	if i := strings.IndexByte(sniffed, ';'); i >= 0 {
		sniffed = strings.TrimSpace(sniffed[:i])
	}
	switch sniffed {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return sniffed
	}
	return fallback
}

// isInSkillsPath checks if filePath is within any of the configured skills
// directories. Returns true for files that can be read without permission
// prompts and without size limits.
//
// Note that symlinks are resolved to prevent path traversal attacks via
// symbolic links.
func isInSkillsPath(filePath string, skillsPaths []string) bool {
	if len(skillsPaths) == 0 {
		return false
	}

	absFilePath, err := filepath.Abs(filePath)
	if err != nil {
		return false
	}

	evalFilePath, err := filepath.EvalSymlinks(absFilePath)
	if err != nil {
		return false
	}

	for _, skillsPath := range skillsPaths {
		absSkillsPath, err := filepath.Abs(skillsPath)
		if err != nil {
			continue
		}

		evalSkillsPath, err := filepath.EvalSymlinks(absSkillsPath)
		if err != nil {
			continue
		}

		relPath, err := filepath.Rel(evalSkillsPath, evalFilePath)
		if err == nil && !strings.HasPrefix(relPath, "..") {
			return true
		}
	}

	return false
}

// readBuiltinFile reads a file from the embedded builtin skills filesystem.
func readBuiltinFile(params ViewParams, skillTracker *skills.Tracker) (fantasy.ToolResponse, error) {
	embeddedPath := "builtin/" + strings.TrimPrefix(params.FilePath, skills.BuiltinPrefix)
	builtinFS := skills.BuiltinFS()

	data, err := fs.ReadFile(builtinFS, embeddedPath)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("Builtin file not found: %s", params.FilePath)), nil
	}

	content := string(data)
	if !utf8.ValidString(content) {
		return fantasy.NewTextErrorResponse("File content is not valid UTF-8"), nil
	}

	limit := params.Limit
	if limit <= 0 {
		limit = 1000000 // Effectively no limit for skill files.
	}

	lines := strings.Split(content, "\n")
	offset := min(params.Offset, len(lines))
	lines = lines[offset:]

	hasMore := len(lines) > limit
	if hasMore {
		lines = lines[:limit]
	}

	output := "<file>\n"
	output += addLineNumbers(strings.Join(lines, "\n"), offset+1)
	if hasMore {
		output += fmt.Sprintf("\n\n(File has more lines. Use 'offset' parameter to read beyond line %d)",
			offset+len(lines))
	}
	output += "\n</file>\n"

	meta := ViewResponseMetadata{
		FilePath: params.FilePath,
		Content:  strings.Join(lines, "\n"),
	}
	if skill, err := skills.ParseContent(data); err == nil {
		meta.ResourceType = ViewResourceSkill
		meta.ResourceName = skill.Name
		meta.ResourceDescription = skill.Description
		skillTracker.MarkLoaded(skill.Name)
	}

	return fantasy.WithResponseMetadata(
		fantasy.NewTextResponse(output),
		meta,
	), nil
}
