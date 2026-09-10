package acp

import (
	"fmt"
	"sort"

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
		switch {
		case block.Text != nil || block.ResourceLink != nil:
			// Text and resource links are protocol v1 baseline capabilities.
		case block.Image != nil:
			if !image {
				return fmt.Errorf("agent did not advertise image prompt support (block %d)", i)
			}
		case block.Audio != nil:
			if !audio {
				return fmt.Errorf("agent did not advertise audio prompt support (block %d)", i)
			}
		case block.Resource != nil:
			if !embedded {
				return fmt.Errorf("agent did not advertise embedded resource prompt support (block %d)", i)
			}
		default:
			return fmt.Errorf("prompt block %d has no content variant", i)
		}
	}
	return nil
}
