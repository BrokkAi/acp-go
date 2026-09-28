package acp

import (
	"fmt"
	"sort"

	"github.com/BrokkAi/acp-go/internal/acpvalidate"
	"github.com/BrokkAi/acp-go/schema"
)

func boolPtr(value bool) *bool { return &value }

// WorkspaceCapabilities advertises the filesystem and terminal host methods
// used by unattended clients. Every argument defaults to disabled when false.
func WorkspaceCapabilities(readFiles, writeFiles, terminal bool) Capabilities {
	return Capabilities{
		Fs: &schema.FileSystemCapabilities{
			ReadTextFile:  boolPtr(readFiles),
			WriteTextFile: boolPtr(writeFiles),
		},
		Terminal: boolPtr(terminal),
	}
}

func NewTextContent(text string) Content {
	return Content{Text: &schema.TextContent{Text: text}}
}

func NewImageContent(mimeType, base64Data string) Content {
	return Content{Image: &schema.ImageContent{MimeType: mimeType, Data: base64Data}}
}

func NewAudioContent(mimeType, base64Data string) Content {
	return Content{Audio: &schema.AudioContent{MimeType: mimeType, Data: base64Data}}
}

func NewResourceLinkContent(name, uri string) Content {
	return Content{ResourceLink: &schema.ResourceLink{Name: name, URI: uri}}
}

func NewTextResourceContent(uri, text string) Content {
	return Content{Resource: &schema.EmbeddedResource{
		Resource: schema.EmbeddedResourceResource{
			TextResourceContents: &schema.TextResourceContents{URI: uri, Text: text},
		},
	}}
}

func NewBlobResourceContent(uri, base64Data string) Content {
	return Content{Resource: &schema.EmbeddedResource{
		Resource: schema.EmbeddedResourceResource{
			BlobResourceContents: &schema.BlobResourceContents{URI: uri, Blob: base64Data},
		},
	}}
}

func NewStdioMCPServer(name, command string, args []string, env map[string]string) schema.McpServer {
	variables := make([]schema.EnvVariable, 0, len(env))
	for key, value := range env {
		variables = append(variables, schema.EnvVariable{Name: key, Value: value})
	}
	sort.Slice(variables, func(i, j int) bool { return variables[i].Name < variables[j].Name })
	return schema.McpServer{Stdio: &schema.McpServerStdio{
		Name: name, Command: command, Args: args, Env: variables,
	}}
}

func NewHTTPMCPServer(name, url string, headers ...schema.HttpHeader) schema.McpServer {
	return schema.McpServer{HTTP: &schema.McpServerHttp{Name: name, URL: url, Headers: headers}}
}

func NewSSEMCPServer(name, url string, headers ...schema.HttpHeader) schema.McpServer {
	return schema.McpServer{SSE: &schema.McpServerSse{Name: name, URL: url, Headers: headers}}
}

func validatePromptCapabilities(init Initialization, prompt []Content) error {
	var capabilities *schema.PromptCapabilities
	if init.AgentCapabilities != nil {
		capabilities = init.AgentCapabilities.PromptCapabilities
	}
	image := false
	audio := false
	embedded := false
	if capabilities != nil {
		image = capabilities.Image != nil && *capabilities.Image
		audio = capabilities.Audio != nil && *capabilities.Audio
		embedded = capabilities.EmbeddedContext != nil && *capabilities.EmbeddedContext
	}
	for i, block := range prompt {
		variants := 0
		if block.Text != nil {
			variants++
		}
		if block.Image != nil {
			variants++
		}
		if block.Audio != nil {
			variants++
		}
		if block.ResourceLink != nil {
			variants++
		}
		if block.Resource != nil {
			variants++
		}
		if variants != 1 {
			return fmt.Errorf("prompt block %d has %d content variants, exactly one is required", i, variants)
		}
		switch {
		case block.Text != nil:
			// Text is a protocol v1 baseline capability.
		case block.ResourceLink != nil:
			if err := acpvalidate.URI("resource link URI", block.ResourceLink.URI); err != nil {
				return fmt.Errorf("prompt block %d: %w", i, err)
			}
		case block.Image != nil:
			if !image {
				return fmt.Errorf("agent did not advertise image prompt support (block %d)", i)
			}
			if err := validateMediaPayload(i, "image content", block.Image.MimeType, block.Image.URI); err != nil {
				return err
			}
		case block.Audio != nil:
			if !audio {
				return fmt.Errorf("agent did not advertise audio prompt support (block %d)", i)
			}
			if err := acpvalidate.MediaType("audio content media type", block.Audio.MimeType); err != nil {
				return fmt.Errorf("prompt block %d: %w", i, err)
			}
		case block.Resource != nil:
			if !embedded {
				return fmt.Errorf("agent did not advertise embedded resource prompt support (block %d)", i)
			}
			if err := validateEmbeddedResource(i, block.Resource); err != nil {
				return err
			}
		default:
			return fmt.Errorf("prompt block %d has no content variant", i)
		}
	}
	return nil
}

// validateMediaPayload checks the media type a block declares and, when one is
// present, that the optional URI is absolute.
func validateMediaPayload(index int, kind, mimeType string, uri *string) error {
	if err := acpvalidate.MediaType(kind+" media type", mimeType); err != nil {
		return fmt.Errorf("prompt block %d: %w", index, err)
	}
	if uri != nil {
		if err := acpvalidate.URI(kind+" URI", *uri); err != nil {
			return fmt.Errorf("prompt block %d: %w", index, err)
		}
	}
	return nil
}

// validateEmbeddedResource checks the URI, and any declared media type, of an
// embedded text or blob resource.
func validateEmbeddedResource(index int, resource *schema.EmbeddedResource) error {
	var uri string
	var mimeType *string
	switch contents := resource.Resource; {
	case contents.TextResourceContents != nil:
		uri, mimeType = contents.TextResourceContents.URI, contents.TextResourceContents.MimeType
	case contents.BlobResourceContents != nil:
		uri, mimeType = contents.BlobResourceContents.URI, contents.BlobResourceContents.MimeType
	default:
		return fmt.Errorf("prompt block %d: embedded resource requires a text or blob resource", index)
	}
	if err := acpvalidate.URI("embedded resource URI", uri); err != nil {
		return fmt.Errorf("prompt block %d: %w", index, err)
	}
	if mimeType != nil {
		if err := acpvalidate.MediaType("embedded resource media type", *mimeType); err != nil {
			return fmt.Errorf("prompt block %d: %w", index, err)
		}
	}
	return nil
}
