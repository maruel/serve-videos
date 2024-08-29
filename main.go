// Copyright 2024 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

// serve-videos serves a directory of videos over HTTP.
package main

import (
	"flag"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lmittmann/tint"
	"github.com/mattn/go-colorable"
	"github.com/mattn/go-isatty"
	"gopkg.in/fsnotify.v1"
)

func getWd() string {
	wd, _ := os.Getwd()
	return wd
}

var rootTmpl = template.Must(template.New("").Parse(`<!DOCTYPE HTML>
<meta name="viewport" content="width=device-width, initial-scale=1" />
<style>
video {
	width: 100%;
}
</style>
<script src="https://cdnjs.cloudflare.com/ajax/libs/hls.js/1.5.15/hls.min.js"></script>
<div id=players></div>
<script>
const ESC = {'<': '&lt;', '>': '&gt;', '"': '&quot;', '&': '&amp;'}
function escapeChar(a) {
  return ESC[a] || a;
}
function escape(s) {
  return s.replace(/[<>"&]/g, escapeChar);
}

function add(i, file) {
	let parent = document.getElementById("players");
	let d = document.createElement("div");
	d.id = "d" + i;
	d.innerHTML = '' +
		'<a href="raw/' + escape(file) + '" target=_blank>' + file + '</a>' +
		'<video id="vid' + i + '" controls preload="none" ' +
		'onloadstart="this.playbackRate=2;" ' +
		'controlslist="nodownload noremoteplayback" ' +
		'disablepictureinpicture disableremoteplayback ' +
		'muted><source src="raw/' + escape(file) + '" /></video>';
	if (file.endsWith(".m3u8")) {
		if (Hls.isSupported()) {
			let video = d.getElementsByTagName('video')[0];
			let hls = new Hls();
			hls.loadSource("raw/" + file);
			hls.attachMedia(video);
		} else {
			console.log("welp for " + file);
			return null;
		}
	}
	parent.insertAdjacentElement("afterbegin", d);
	// In order: parent.appendChild(d);
	return document.getElementById("vid" + i);
}

function addall() {
	const observer = new IntersectionObserver((entries, observer) => {
		entries.forEach(entry => {
			let target = entry.target;
			if (entry.isIntersecting) {
				if (target.paused) {
					//console.log('Element ' + target.id + ' is now visible in the viewport: starting');
					// Only auto-start after being visible for 1s, to reduce
					// strain on the server when scrolling fast.
					target.playTimeout = setTimeout(() => {
						target.play();
						target.playTimeout = null;
					}, 1000);
				}
			} else {
				if (target.playTimeout) {
					clearTimeout(target.playTimeout);
					target.playTimeout = null;
				}
				if (!target.paused) {
					//console.log('Element ' + target.id + ' is not visible in the viewport anymore: pausing');
					// This may fire warnings in the dev console because pause()
					// is called before the play() promise is executed. We
					// don't care.
					target.pause();
				}
			}
		});
	});
	const files = {{.}};
	for (let i in files) {
		if (!files[i].endsWith(".ts")) {
			let child = add(i, files[i]);
			if (child) {
				observer.observe(child);
			}
		}
	}
}

addall();
</script>
`))

var listTmpl = template.Must(template.New("").Parse(`<!DOCTYPE HTML>
<meta name="viewport" content="width=device-width, initial-scale=1" />
<style>
</style>
<div>
<ul>
{{range $k, $v := .}}
	<li><a href="raw/{{$v}}" target="_blank" rel="noopener noreferrer">{{$v}}</a></li>
{{end}}
</ul>
</div>
`))

func getFiles(root string, exts []string) (*fsnotify.Watcher, []string) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		slog.Error("failed to create a watcher", "error", err)
		os.Exit(1)
	}
	var files []string
	offset := len(root) + 1
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if d.IsDir() {
			if err2 := w.Add(path); err2 != nil {
				// Ignore, it's not a big deal.
				//slog.Error("failed to add path", "path", path, "error", err2)
			}
		} else {
			for _, ext := range exts {
				if strings.HasSuffix(path, ext) {
					files = append(files, path[offset:])
					break
				}
			}
		}
		return nil
	})
	sort.Strings(files)
	slog.Info("done parsing", "num_files", len(files))
	return w, files
}

type stringsFlag []string

func (s *stringsFlag) String() string {
	return strings.Join(*s, ",")
}

func (s *stringsFlag) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func main() {
	logger := slog.New(tint.NewHandler(colorable.NewColorable(os.Stderr), &tint.Options{
		Level:      slog.LevelDebug,
		TimeFormat: time.TimeOnly,
		NoColor:    !isatty.IsTerminal(os.Stderr.Fd()),
	}))
	slog.SetDefault(logger)
	port := flag.Int("port", 8010, "port number")
	var extsArg stringsFlag
	flag.Var(&extsArg, "e", "extensions")
	root := flag.String("root", getWd(), "root directory")
	flag.Parse()

	if flag.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "Unexpected argument\n")
		return
	}

	if len(extsArg) == 0 {
		extsArg = []string{"m3u8", "mkv", "mp4", "ts"}
	}
	slog.Info("looking for files", "root", *root, "ext", strings.Join(extsArg, ","))
	mu := sync.Mutex{}
	wat, files := getFiles(*root, extsArg)

	go func() {
		e := <-wat.Events
		slog.Info("event", "op", e.Op, "name", e.Name)
		_ = wat.Close()
		mu.Lock()
		wat, files = getFiles(*root, extsArg)
		mu.Unlock()
	}()

	m := http.ServeMux{}
	// Videos
	m.HandleFunc("GET /raw/", func(w http.ResponseWriter, req *http.Request) {
		path, err := url.QueryUnescape(req.URL.Path)
		if err != nil {
			http.Error(w, "Invalid path", 404)
			return
		}
		f := path[len("/raw/"):]
		mu.Lock()
		// Only allow files in the list we have.
		i := sort.SearchStrings(files, f)
		found := i < len(files) && files[i] == f
		mu.Unlock()
		if !found {
			slog.Info("http", "f", f)
			http.Error(w, "Invalid path", 404)
			return
		}
		// Cache for a long time, the exception is m3u8 since it could be a live
		// playlist.
		if h := w.Header(); strings.HasSuffix(f, ".m3u8") {
			h.Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
			h.Set("Pragma", "no-cache")
			h.Set("Expires", "0")
			h.Set("Content-Type", "text/html; charset=utf-8")
		} else {
			h.Set("Cache-Control", "public, max-age=86400")
		}
		http.ServeFile(w, req, filepath.Join(*root, f))
	})

	// HTML
	m.HandleFunc("GET /list", func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		tmp := make([]string, len(files))
		copy(tmp, files)
		mu.Unlock()
		h := w.Header()
		h.Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		h.Set("Pragma", "no-cache")
		h.Set("Expires", "0")
		h.Set("Content-Type", "text/html; charset=utf-8")
		_ = listTmpl.Execute(w, tmp)
	})
	m.HandleFunc("GET /", func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		tmp := make([]string, len(files))
		copy(tmp, files)
		mu.Unlock()
		h := w.Header()
		h.Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		h.Set("Pragma", "no-cache")
		h.Set("Expires", "0")
		h.Set("Content-Type", "text/html; charset=utf-8")
		_ = rootTmpl.Execute(w, files)
	})
	s := &http.Server{
		Addr:           fmt.Sprintf(":%d", *port),
		Handler:        &m,
		ReadTimeout:    10. * time.Second,
		WriteTimeout:   time.Hour,
		MaxHeaderBytes: http.DefaultMaxHeaderBytes,
	}
	slog.Info("serving", "port", *port)
	_ = s.ListenAndServe()
}
