package main

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Config struct {
	AllowedRoots []string
	Username     string
	Password     string
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func readSecretOrEnv(fileVar, envVar, def string) string {
	if path := os.Getenv(fileVar); path != "" {
		if b, err := os.ReadFile(path); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return getenv(envVar, def)
}

func loadConfig() Config {
	rootsEnv := getenv("ALLOWED_ROOTS", "/data/sdd1,/data/hdd1")
	roots := []string{}
	for _, p := range strings.Split(rootsEnv, ",") {
		p = filepath.Clean(strings.TrimSpace(p))
		if p != "" {
			roots = append(roots, p)
		}
	}
	return Config{
		AllowedRoots: roots,
		Username:     readSecretOrEnv("USERNAME_FILE", "USERNAME", "admin"),
		Password:     readSecretOrEnv("PASSWORD_FILE", "PASSWORD", "changeme"),
	}
}

var cfg Config

type FileEntry struct {
	Name    string
	Path    string
	RelPath string
	IsDir   bool
	Size    int64
	ModTime time.Time
}

type PageData struct {
	Title       string
	CurrentRoot string
	CurrentPath string
	Breadcrumb  []Crumb
	Entries     []FileEntry
	Roots       []string
	Flash       string
}

type Crumb struct {
	Name string
	Link string
}

func must[T any](v T, err error) T {
	if err != nil {
		log.Fatal(err)
	}
	return v
}

func resolveSafePath(rootParam, rel string) (string, string, string, error) {
	rootParam = filepath.Clean(rootParam)
	var root string
	for _, r := range cfg.AllowedRoots {
		if r == rootParam {
			root = r
			break
		}
	}
	if root == "" {
		return "", "", "", fmt.Errorf("invalid root")
	}
	if rel == "" || rel == "/" {
		rel = "."
	}
	rel = filepath.Clean("/" + rel)
	rel = strings.TrimPrefix(rel, "/")
	abs := filepath.Join(root, rel)

	relCheck, err := filepath.Rel(root, abs)
	if err != nil || strings.HasPrefix(relCheck, "..") {
		return "", "", "", fmt.Errorf("path escapes root")
	}
	return root, abs, relCheck, nil
}

func buildBreadcrumb(root, rel string) []Crumb {
	crumbs := []Crumb{{Name: filepath.Base(root), Link: fmt.Sprintf("/?root=%s", urlq(root))}}
	if rel == "." || rel == "" {
		return crumbs
	}
	parts := strings.Split(rel, string(os.PathSeparator))
	curr := ""
	for _, p := range parts {
		if p == "" || p == "." {
			continue
		}
		if curr == "" {
			curr = p
		} else {
			curr = filepath.Join(curr, p)
		}
		crumbs = append(crumbs, Crumb{
			Name: p, Link: fmt.Sprintf("/?root=%s&path=%s", urlq(root), urlq(curr)),
		})
	}
	return crumbs
}

func listDir(abs, root string) ([]FileEntry, error) {
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	items := make([]FileEntry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		itemPath := filepath.Join(abs, e.Name())
		rel, _ := filepath.Rel(root, itemPath)
		items = append(items, FileEntry{
			Name:    e.Name(),
			Path:    itemPath,
			RelPath: rel,
			IsDir:   e.IsDir(),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].IsDir != items[j].IsDir {
			return items[i].IsDir
		}
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})
	return items, nil
}

func humanSize(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	suffix := []string{"KB", "MB", "GB", "TB"}
	f := float64(n)
	for i, s := range suffix {
		f /= 1024
		if f < 1024 || i == len(suffix)-1 {
			return fmt.Sprintf("%.1f %s", f, s)
		}
	}
	return fmt.Sprintf("%d B", n)
}

func urlq(s string) string {
	r := strings.ReplaceAll(s, " ", "%20")
	r = strings.ReplaceAll(r, "\n", "")
	return r
}

func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func basicAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != cfg.Username || p != cfg.Password {
			w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func render(w http.ResponseWriter, data PageData) {
	tmpl := template.Must(template.New("page").Funcs(template.FuncMap{
		"humanSize": humanSize,
		"fmtTime":   func(t time.Time) string { return t.Format("2006-01-02 15:04") },
	}).Parse(pageHTML))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, data); err != nil {
		log.Println("render error:", err)
	}
}

func handleBrowse(w http.ResponseWriter, r *http.Request) {
	root := r.URL.Query().Get("root")
	if root == "" && len(cfg.AllowedRoots) > 0 {
		root = cfg.AllowedRoots[0]
	}
	rel := r.URL.Query().Get("path")

	root, abs, relSafe, err := resolveSafePath(root, rel)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if st, err := os.Stat(abs); err == nil && !st.IsDir() {
		http.ServeFile(w, r, abs)
		return
	}
	items, err := listDir(abs, root)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := PageData{
		Title:       "Go File Manager",
		CurrentRoot: root,
		CurrentPath: relSafe,
		Breadcrumb:  buildBreadcrumb(root, relSafe),
		Entries:     items,
		Roots:       cfg.AllowedRoots,
	}
	render(w, data)
}

func addFileToZip(zw *zip.Writer, path, name string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, f)
	return err
}

func handleZip(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	root := r.Form.Get("root")
	rel := r.Form.Get("path")
	_, abs, relSafe, err := resolveSafePath(root, rel)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	st, err := os.Stat(abs)
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	if st.IsDir() {
		filepath.WalkDir(abs, func(p string, d fs.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if d.IsDir() {
				return nil
			}
			relp, _ := filepath.Rel(abs, p)
			return addFileToZip(zw, p, relp)
		})
	} else {
		addFileToZip(zw, abs, filepath.Base(abs))
	}
	zw.Close()
	name := strings.ReplaceAll(relSafe, string(os.PathSeparator), "_")
	if name == "" {
		name = filepath.Base(abs)
	}
	if name == "" {
		name = "download"
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s.zip\"", name))
	w.Write(buf.Bytes())
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(512 << 20); err != nil { // 512MB
		http.Error(w, err.Error(), 400)
		return
	}
	root := r.FormValue("root")
	rel := r.FormValue("path")
	_, abs, relSafe, err := resolveSafePath(root, rel)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	files := r.MultipartForm.File["files[]"]
	if len(files) == 0 {
		http.Error(w, "no files", 400)
		return
	}
	for _, fh := range files {
		if err := saveUploadedFile(abs, fh); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
	}
	http.Redirect(w, r, fmt.Sprintf("/?root=%s&path=%s", urlq(root), urlq(relSafe)), http.StatusSeeOther)
}

func saveUploadedFile(dir string, fh *multipart.FileHeader) error {
	src, err := fh.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	dstPath := filepath.Join(dir, filepath.Base(fh.Filename))
	dst, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer dst.Close()
	_, err = io.Copy(dst, src)
	return err
}

func handleMkdir(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	root := r.Form.Get("root")
	rel := r.Form.Get("path")
	name := r.Form.Get("name")
	if name == "" {
		http.Error(w, "name required", 400)
		return
	}
	_, abs, relSafe, err := resolveSafePath(root, rel)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	target := filepath.Join(abs, filepath.Base(name))
	if err := os.MkdirAll(target, 0o755); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/?root=%s&path=%s", urlq(root), urlq(relSafe)), http.StatusSeeOther)
}

func handleDelete(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	root := r.Form.Get("root")
	rel := r.Form.Get("path")
	name := r.Form.Get("name")
	if name == "" {
		http.Error(w, "name required", 400)
		return
	}
	_, abs, relSafe, err := resolveSafePath(root, rel)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	target := filepath.Join(abs, filepath.Base(name))
	st, err := os.Stat(target)
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	if st.IsDir() {
		err = os.RemoveAll(target)
	} else {
		err = os.Remove(target)
	}
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/?root=%s&path=%s", urlq(root), urlq(relSafe)), http.StatusSeeOther)
}

func handleRename(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	root := r.Form.Get("root")
	rel := r.Form.Get("path")
	oldName := r.Form.Get("old")
	newName := r.Form.Get("new")
	if oldName == "" || newName == "" {
		http.Error(w, "names required", 400)
		return
	}
	_, abs, relSafe, err := resolveSafePath(root, rel)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	from := filepath.Join(abs, filepath.Base(oldName))
	to := filepath.Join(abs, filepath.Base(newName))
	if err := safeRename(from, to, abs); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/?root=%s&path=%s", urlq(root), urlq(relSafe)), http.StatusSeeOther)
}

func safeRename(from, to, dir string) error {
	for _, p := range []string{from, to} {
		rel, err := filepath.Rel(dir, p)
		if err != nil || strings.HasPrefix(rel, "..") {
			return errors.New("path escapes directory")
		}
	}
	return os.Rename(from, to)
}

var pageHTML = `<!doctype html>
<html lang="pt-br">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Go File Manager</title>
  <script src="https://unpkg.com/htmx.org@2.0.3"></script>
  <script src="https://cdn.tailwindcss.com"></script>
</head>
<body class="bg-slate-50 text-slate-900">
  <div class="max-w-6xl mx-auto p-6">
    <header class="flex items-center justify-between mb-6">
      <h1 class="text-2xl font-bold">Go File Manager</h1>
      <nav class="flex items-center gap-3">
        <form method="get" action="/" class="flex items-center gap-2">
          <label class="text-sm">Disco:</label>
          <select name="root" class="border rounded px-2 py-1">
            {{range .Roots}}
              <option value="{{.}}" {{if eq . $.CurrentRoot}}selected{{end}}>{{.}}</option>
            {{end}}
          </select>
          <input type="hidden" name="path" value="{{.CurrentPath}}" />
          <button class="bg-blue-600 text-white px-3 py-1 rounded">Abrir</button>
        </form>
      </nav>
    </header>

    <div class="text-sm breadcrumbs flex items-center gap-1 mb-4">
      {{range $i, $c := .Breadcrumb}}
        {{if $i}}<span>/</span>{{end}}
        <a class="text-blue-700 hover:underline" href="{{$c.Link}}">{{$c.Name}}</a>
      {{end}}
    </div>

    <section class="mb-4 p-4 bg-white rounded-2xl shadow">
      <form class="flex flex-wrap items-center gap-3" action="/upload" method="post" enctype="multipart/form-data">
        <input type="hidden" name="root" value="{{.CurrentRoot}}" />
        <input type="hidden" name="path" value="{{.CurrentPath}}" />
        <input class="border rounded px-3 py-2" type="file" name="files[]" multiple />
        <button class="bg-emerald-600 text-white px-4 py-2 rounded">Upload</button>
      </form>
      <form class="mt-3 flex items-center gap-2" action="/mkdir" method="post">
        <input type="hidden" name="root" value="{{.CurrentRoot}}" />
        <input type="hidden" name="path" value="{{.CurrentPath}}" />
        <input class="border rounded px-3 py-2" type="text" name="name" placeholder="Nova pasta" />
        <button class="bg-slate-700 text-white px-4 py-2 rounded">Criar pasta</button>
      </form>
    </section>

    <section class="bg-white rounded-2xl shadow">
      <table class="w-full text-left">
        <thead>
          <tr class="border-b">
            <th class="py-3 px-4">Nome</th>
            <th class="py-3 px-4">Tamanho</th>
            <th class="py-3 px-4">Modificado</th>
            <th class="py-3 px-4 text-right">Ações</th>
          </tr>
        </thead>
        <tbody>
          {{if not .Entries}}
            <tr><td class="py-6 px-4 text-slate-500" colspan="4">Vazio</td></tr>
          {{end}}
          {{range .Entries}}
            <tr class="border-b hover:bg-slate-50">
              <td class="py-2 px-4">
                {{if .IsDir}}
                  <a class="text-blue-700 hover:underline" href="/?root={{$.CurrentRoot}}&path={{.RelPath}}">📁 {{.Name}}</a>
                {{else}}
                  <a class="text-slate-800 hover:underline" href="/download?root={{$.CurrentRoot}}&path={{.RelPath}}">📄 {{.Name}}</a>
                {{end}}
              </td>
              <td class="py-2 px-4">{{if .IsDir}}—{{else}}{{humanSize .Size}}{{end}}</td>
              <td class="py-2 px-4">{{fmtTime .ModTime}}</td>
              <td class="py-2 px-4">
                <div class="flex items-center gap-2 justify-end">
                  {{if not .IsDir}}
                  <a class="px-2 py-1 rounded border" href="/download?root={{$.CurrentRoot}}&path={{.RelPath}}">Baixar</a>
                  {{end}}
                  <form action="/zip" method="post">
                    <input type="hidden" name="root" value="{{$.CurrentRoot}}" />
                    <input type="hidden" name="path" value="{{.RelPath}}" />
                    <button class="px-2 py-1 rounded border">Zip</button>
                  </form>
                  <form action="/rename" method="post" class="flex items-center gap-1">
                    <input type="hidden" name="root" value="{{$.CurrentRoot}}" />
                    <input type="hidden" name="path" value="{{$.CurrentPath}}" />
                    <input type="hidden" name="old" value="{{.Name}}" />
                    <input class="border rounded px-2 py-1 text-sm" type="text" name="new" placeholder="Novo nome" />
                    <button class="px-2 py-1 rounded border">Renomear</button>
                  </form>
                  <form action="/delete" method="post" onsubmit="return confirm('Excluir {{.Name}}? Esta ação é permanente.');">
                    <input type="hidden" name="root" value="{{$.CurrentRoot}}" />
                    <input type="hidden" name="path" value="{{$.CurrentPath}}" />
                    <input type="hidden" name="name" value="{{.Name}}" />
                    <button class="px-2 py-1 rounded border text-red-700">Excluir</button>
                  </form>
                </div>
              </td>
            </tr>
          {{end}}
        </tbody>
      </table>
    </section>
  </div>
</body>
</html>`

func main() {
	cfg = loadConfig()
	if len(cfg.AllowedRoots) == 0 {
		log.Fatal("No ALLOWED_ROOTS configured")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", basicAuth(handleBrowse))
	mux.HandleFunc("/download", basicAuth(func(w http.ResponseWriter, r *http.Request) {
		root := r.URL.Query().Get("root")
		rel := r.URL.Query().Get("path")
		_, abs, _, err := resolveSafePath(root, rel)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		http.ServeFile(w, r, abs)
	}))
	mux.HandleFunc("/zip", basicAuth(handleZip))
	mux.HandleFunc("/upload", basicAuth(handleUpload))
	mux.HandleFunc("/mkdir", basicAuth(handleMkdir))
	mux.HandleFunc("/delete", basicAuth(handleDelete))
	mux.HandleFunc("/rename", basicAuth(handleRename))

	addr := ":8080"
	log.Printf("Go File Manager listening on %s (roots: %v)\n", addr, cfg.AllowedRoots)
	log.Fatal(http.ListenAndServe(addr, withSecurityHeaders(mux)))
}
