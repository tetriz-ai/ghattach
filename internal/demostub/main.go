// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Command demostub is a local stub of github.com's attachment upload. It runs
// the examples (scripts/example-setup.sh). It accepts the session "fake",
// answers every OWNER/REPO page, and returns made-up asset URLs. Run it with:
// go run ./internal/demostub -addr 127.0.0.1:8765
//
// With -addr-file, the address it actually listens on is written to that file
// once the port is bound, so -addr 127.0.0.1:0 lets the system pick a free port.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8765", "address to listen on; port 0 picks a free port")
	addrFile := flag.String("addr-file", "", "file to write the bound address to, once listening")
	flag.Parse()
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	bound := ln.Addr().String()
	base := "http://" + bound
	if *addrFile != "" {
		// Written whole and renamed, so a reader never sees half an address.
		tmp := *addrFile + ".tmp"
		if err := os.WriteFile(tmp, []byte(bound+"\n"), 0o600); err != nil {
			log.Fatal(err)
		}
		if err := os.Rename(tmp, *addrFile); err != nil {
			log.Fatal(err)
		}
	}

	var mu sync.Mutex
	next := 0
	names := map[int]string{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("user_session"); err != nil || c.Value != "fake" {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, `{"uploadToken":"demo-token"}`)
	})
	mux.HandleFunc("POST /upload/policies/assets", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		mr, err := r.MultipartReader()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		form, err := mr.ReadForm(1 << 20)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer func() { _ = form.RemoveAll() }()
		field := func(key string) string {
			if v := form.Value[key]; len(v) > 0 {
				return v[0]
			}
			return ""
		}
		// GitHub caps some types below ghattach's 100 MiB limit. The stub
		// refuses an image over 10 MiB, so the examples can show a failure
		// that only the server can report.
		if size, _ := strconv.ParseInt(field("size"), 10, 64); strings.HasPrefix(field("content_type"), "image/") && size > 10<<20 {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, "stub: an image must be at most 10 MiB")
			return
		}
		mu.Lock()
		next++
		id := next
		names[id] = field("name")
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"upload_url":                      base + "/storage",
			"asset":                           map[string]any{"id": id, "content_type": field("content_type")},
			"form":                            map[string]string{"key": fmt.Sprint(id)},
			"asset_upload_url":                fmt.Sprintf("/upload/assets/%d", id),
			"asset_upload_authenticity_token": "demo-finalize",
		})
	})
	mux.HandleFunc("POST /storage", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("PUT /upload/assets/{id}", func(w http.ResponseWriter, r *http.Request) {
		var id int
		_, _ = fmt.Sscan(r.PathValue("id"), &id)
		mu.Lock()
		name := names[id]
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{
			"href": fmt.Sprintf("https://github.com/user-attachments/assets/00000000-0000-4000-8000-%012d", id),
			"name": name,
		})
	})
	log.Fatal(http.Serve(ln, mux)) //nolint:gosec // local demo server, no timeouts needed
}
