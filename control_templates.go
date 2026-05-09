package main

import (
	"database/sql"
	"embed"
	"html/template"
)

//go:embed templates/*.html
var controlTemplateFS embed.FS

var controlTemplate = template.Must(template.New("").Funcs(template.FuncMap{
	"add":          func(a, b int) int { return a + b },
	"time":         formatControlTime,
	"nullableTime": formatControlNullTime,
}).ParseFS(controlTemplateFS, "templates/*.html"))

func formatControlNullTime(t sql.NullTime) string {
	if !t.Valid {
		return "Never"
	}
	return formatControlTime(t.Time)
}
