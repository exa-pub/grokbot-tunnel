package app

import "github.com/exa-pub/grokbot-tunnel/internal/pkg/envvar"

func backendURL() string {
	return envvar.Get("SAND_BACKEND_URL", envvar.Get("CURSOR_API_BASE_URL", "https://api2.cursor.sh"))
}

func websiteURL() string {
	return envvar.Get("SAND_CURSOR_WEBSITE_URL", envvar.Get("CURSOR_WEBSITE_URL", "https://cursor.com"))
}
