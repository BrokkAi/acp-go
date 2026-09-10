package v2

import schema "github.com/BrokkAi/acp-go/schema/v2"

func NewTextContent(text string) Content {
	return Content{Text: &schema.TextContent{Text: text}}
}

func NewImageContent(data, mimeType string) Content {
	return Content{Image: &schema.ImageContent{Data: data, MimeType: schema.MediaType(mimeType)}}
}

func NewAudioContent(data, mimeType string) Content {
	return Content{Audio: &schema.AudioContent{Data: data, MimeType: schema.MediaType(mimeType)}}
}
