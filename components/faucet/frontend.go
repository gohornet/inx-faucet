package faucet

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
)

//go:embed frontend/public
var distFiles embed.FS

func frontendFileSystem() http.FileSystem {
	f, err := fs.Sub(distFiles, "frontend/public")
	if err != nil {
		panic(err)
	}

	return http.FS(f)
}

func calculateMimeType(e echo.Context) string {
	url := e.Request().URL.String()

	switch {
	case strings.HasSuffix(url, ".html"):
		return echo.MIMETextHTMLCharsetUTF8
	case strings.HasSuffix(url, ".css"):
		return "text/css"
	case strings.HasSuffix(url, ".js"):
		return echo.MIMEApplicationJavaScript
	case strings.HasSuffix(url, ".json"):
		return echo.MIMEApplicationJSON
	case strings.HasSuffix(url, ".png"):
		return "image/png"
	case strings.HasSuffix(url, ".svg"):
		return "image/svg+xml"
	default:
		return echo.MIMETextHTMLCharsetUTF8
	}
}

func frontendMiddleware() echo.MiddlewareFunc {
	fs := frontendFileSystem()

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			contentType := calculateMimeType(c)

			path := strings.TrimPrefix(c.Request().RequestURI, "/")
			if len(path) == 0 {
				path = "index.html"
				contentType = echo.MIMETextHTMLCharsetUTF8
			}
			staticBlob, err := fs.Open(path)
			if err != nil {
				// If the asset cannot be found, fall back to the index.html for routing
				path = "index.html"
				contentType = echo.MIMETextHTMLCharsetUTF8
				staticBlob, err = fs.Open(path)
				if err != nil {
					return next(c)
				}
			}

			return c.Stream(http.StatusOK, contentType, staticBlob)
		}
	}
}
