package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"recipes/internal/cookbook"
)

// fileParam is a file the client hands to the tool by download URL, as
// ChatGPT does for files attached in the chat (declared via openai/fileParams).
type fileParam struct {
	DownloadURL string `json:"download_url" jsonschema:"URL to download the file from"`
	FileID      string `json:"file_id,omitempty"`
}

type uploadImageIn struct {
	URL  string     `json:"url,omitempty" jsonschema:"public https URL of the image, or a data:image/...;base64 URI"`
	File *fileParam `json:"file,omitempty" jsonschema:"an image file attached in the chat"`
}

type uploadImageOut struct {
	URL      string `json:"url" jsonschema:"image URL to reference from steps_markdown"`
	Markdown string `json:"markdown" jsonschema:"Markdown image to put into steps_markdown"`
}

func (t *tools) addImageTool(s *mcp.Server) {
	notDestructive, open := false, true
	mcp.AddTool(s, &mcp.Tool{
		Name:  "upload_image",
		Title: "Upload image",
		Description: "Upload a photo for a recipe (PNG, JPEG, GIF or WebP, up to 8 MiB) from a public https URL, a data: URI or a file attached in the chat. " +
			"Returns Markdown to insert into steps_markdown of create_recipe or update_recipe; the image becomes part of the recipe when that call saves it.",
		Annotations: &mcp.ToolAnnotations{Title: "Upload image", DestructiveHint: &notDestructive, OpenWorldHint: &open},
		Meta:        mcp.Meta{"openai/fileParams": []string{"file"}},
	}, t.uploadImage)
}

func (t *tools) uploadImage(ctx context.Context, _ *mcp.CallToolRequest, in uploadImageIn) (*mcp.CallToolResult, uploadImageOut, error) {
	if !t.canWrite(ctx) {
		return nil, uploadImageOut{}, errReadOnly
	}
	if t.d.FetchImage == nil {
		return nil, uploadImageOut{}, errors.New("image upload is not available")
	}
	src := strings.TrimSpace(in.URL)
	if in.File != nil {
		if src != "" {
			return nil, uploadImageOut{}, errors.New("pass either url or file, not both")
		}
		src = strings.TrimSpace(in.File.DownloadURL)
	}
	if src == "" {
		return nil, uploadImageOut{}, errors.New("url or file is required")
	}
	data, err := t.d.FetchImage(ctx, src)
	if err != nil {
		return nil, uploadImageOut{}, fmt.Errorf("could not get the image: %w", err)
	}
	name, err := t.d.Cookbook.SaveUpload(data)
	if errors.Is(err, cookbook.ErrUnsupportedImage) {
		return nil, uploadImageOut{}, errors.New("not a supported image: use PNG, JPEG, GIF or WebP up to 8 MiB")
	}
	if err != nil {
		return nil, uploadImageOut{}, err
	}
	u := t.d.BaseURL + "/uploads/" + name
	return nil, uploadImageOut{URL: u, Markdown: "![](" + u + ")"}, nil
}
