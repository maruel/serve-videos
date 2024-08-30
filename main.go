// Copyright 2024 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

// serve-videos serves a directory of videos over HTTP.
package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
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

//go:embed html/root.html
var rootHTML []byte

//go:embed html/list.html
var listHTML []byte

// Injected data to speed up page load, versus having to do an API call.
var dataTmpl = template.Must(template.New("").Parse("<script>'use strict';const data = {{.}};</script>"))

func getFiles(root string, exts []string) (*fsnotify.Watcher, []string, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create a watcher for %q: %w", root, err)
	}
	var files []string
	offset := len(root) + 1
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if d.IsDir() {
			if err2 := w.Add(path); err2 != nil {
				// Ignore, it's not a big deal.
				slog.Error("watcher", "path", path, "error", err2)
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
	return w, files, nil
}

type stringsFlag []string

func (s *stringsFlag) String() string {
	return strings.Join(*s, ",")
}

func (s *stringsFlag) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func mainImpl() error {
	logger := slog.New(tint.NewHandler(colorable.NewColorable(os.Stderr), &tint.Options{
		Level:      slog.LevelDebug,
		TimeFormat: time.TimeOnly,
		NoColor:    !isatty.IsTerminal(os.Stderr.Fd()),
	}))
	slog.SetDefault(logger)
	addr := flag.String("addr", ":8010", "address and port to listen to")
	var extsArg stringsFlag
	flag.Var(&extsArg, "e", "extensions")
	root := flag.String("root", ".", "root directory")
	flag.Parse()

	if flag.NArg() != 0 {
		return errors.New("unexpected argument")
	}
	if len(extsArg) == 0 {
		extsArg = []string{"m3u8", "mkv", "mp4", "ts"}
	}
	var err error
	if *root, err = filepath.Abs(filepath.Clean(*root)); err != nil {
		return err
	}
	if fi, err := os.Stat(*root); err != nil {
		return fmt.Errorf("-root %q is unusable: %w", *root, err)
	} else if !fi.IsDir() {
		return fmt.Errorf("-root %q is not a directory", *root)
	}
	slog.Info("looking for files", "root", *root, "ext", strings.Join(extsArg, ","))
	mu := sync.Mutex{}
	wat, files, err := getFiles(*root, extsArg)
	if err != nil {
		return err
	}

	go func() {
		for {
			e := <-wat.Events
			slog.Info("event", "op", e.Op, "name", e.Name)
			wat2, files2, _ := getFiles(*root, extsArg)
			_ = wat.Close()
			wat = wat2
			mu.Lock()
			files = files2
			mu.Unlock()
		}
	}()

	m := http.ServeMux{}
	// Videos
	m.HandleFunc("GET /raw/", func(w http.ResponseWriter, req *http.Request) {
		path, err2 := url.QueryUnescape(req.URL.Path)
		if err2 != nil {
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
		if _, err2 := w.Write(listHTML); err2 != nil {
			return
		}
		_ = dataTmpl.Execute(w, map[string]any{"files": tmp})
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
		if _, err2 := w.Write(rootHTML); err2 != nil {
			return
		}
		_ = dataTmpl.Execute(w, map[string]any{"files": tmp})
	})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	s := &http.Server{
		Handler:      &m,
		BaseContext:  func(net.Listener) context.Context { return ctx },
		ReadTimeout:  10. * time.Second,
		WriteTimeout: time.Hour,
	}
	l, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	slog.Info("serving", "addr", l.Addr())
	go s.Serve(l)
	<-ctx.Done()
	_ = s.Shutdown(context.Background())
	return nil
}

func main() {
	if err := mainImpl(); err != nil {
		fmt.Fprintf(os.Stderr, "serve-videos: %s\n", err)
		os.Exit(1)
	}
}
