package main

import (
	"embed"
	"html/template"
)

//go:embed templates/*.html
var controlTemplateFS embed.FS

var controlTemplate = template.Must(template.ParseFS(controlTemplateFS, "templates/*.html"))
