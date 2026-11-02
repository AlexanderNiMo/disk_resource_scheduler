package web

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
)

//go:embed views
var viewsFS embed.FS

// pageNames are the pages that have their own template set.
//
// A single set containing every page does not work: html/template resolves
// {{template "content"}} at execution time, and every page defines "content",
// so the last one parsed would win and every page would render the same body.
var pageNames = []string{"dashboard", "login"}

// templates holds one parsed set per page.
type templates struct {
	set    map[string]*template.Template
	static fs.FS
}

func newTemplates() (*templates, error) {
	funcs := template.FuncMap{
		// bytes renders a file size the way a person reads it.
		"bytes": humanBytes,
	}

	tmpl := &templates{set: make(map[string]*template.Template, len(pageNames))}
	for _, name := range pageNames {
		parsed, err := template.New(name).Funcs(funcs).ParseFS(viewsFS,
			"views/layout.html", "views/fragments.html", "views/"+name+".html")
		if err != nil {
			// At startup rather than at first render: a template that does
			// not parse is a broken deployment, and finding out from a blank
			// page in a browser is a worse way to learn it.
			return nil, fmt.Errorf("parse the %s template: %w", name, err)
		}
		tmpl.set[name] = parsed
	}

	static, err := fs.Sub(viewsFS, "views/static")
	if err != nil {
		return nil, fmt.Errorf("open the static files: %w", err)
	}
	tmpl.static = static
	return tmpl, nil
}

// render writes a page.
//
// The template is executed into a buffer first: a template that fails halfway
// through would otherwise have already written its headers and part of its
// body, so the response would be a 200 carrying half a page. Buffering turns
// that into a clean 500 and one log line.
func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, data any) {
	tmpl, ok := s.tmpl.set[name]
	if !ok {
		s.log.Error("no such template", "template", name)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "layout", data); err != nil {
		s.log.Error("rendering a page failed", "template", name, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

// renderFragment writes a piece of a page, with no layout.
func (s *Server) renderFragment(w http.ResponseWriter, r *http.Request, name string, data any) {
	tmpl, ok := s.tmpl.set["dashboard"]
	if !ok {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		s.log.Error("rendering a fragment failed", "fragment", name, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

// humanBytes renders a file size the way a person reads it.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d Б", n)
	}

	value := float64(n)
	for _, u := range []string{"КиБ", "МиБ", "ГиБ", "ТиБ"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, u)
		}
	}
	return fmt.Sprintf("%.1f ПиБ", value)
}
