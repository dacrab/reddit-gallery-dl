package main

import (
	"bytes"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"

	_ "golang.org/x/image/webp"
)

func convertImage(input io.Reader, format string) ([]byte, string, error) {
	if format == "" || format == "original" {
		data, err := io.ReadAll(input)
		return data, "", err
	}

	img, _, err := image.Decode(input)
	if err != nil {
		return nil, "", fmt.Errorf("decode error: %w", err)
	}

	var buf bytes.Buffer
	var ext string

	switch format {
	case "jpg", "jpeg":
		err = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90})
		ext = ".jpg"
	case "png":
		err = png.Encode(&buf, img)
		ext = ".png"
	case "gif":
		err = gif.Encode(&buf, img, nil)
		ext = ".gif"
	default:
		return nil, "", fmt.Errorf("unsupported format: %s", format)
	}

	if err != nil {
		return nil, "", fmt.Errorf("encode error: %w", err)
	}

	return buf.Bytes(), ext, nil
}
