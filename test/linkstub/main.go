// linkstub is a console gateway stub for trying laavu link by hand and on the test VM.
//
//	linkstub --listen 0.0.0.0:7800 --key /tmp/linkstub.key [--auto-bind]
//
// Servers connect to ws://HOST:7800/v1 (laavu link allows ws:// only on loopback,
// so on the VM forward the port to 127.0.0.1). Control, from the same machine:
//
//	curl localhost:7800/servers
//	curl -X POST localhost:7800/confirm?code=ABCD-EFGH
//	curl -X POST localhost:7800/code                       # a code for laavu link --code
//	curl -X POST localhost:7800/job -d '{"op":"check"}'     # to the only connected server
//	curl -X POST localhost:7800/job -d '{"server":"KEY","op":"firewall.allow","params":{"port":8080}}'
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ilguzin/laavu/pkg/link"
	"github.com/ilguzin/laavu/pkg/link/linktest"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:7800", "address")
	keyFile := flag.String("key", "", "console key file, created if missing (default: a new key each run)")
	autoBind := flag.Bool("auto-bind", false, "confirm every binding without a code")
	flag.Parse()
	g := linktest.New()
	g.AutoBind = *autoBind
	g.Log = log.Printf
	if *keyFile != "" {
		g.Key = loadKey(*keyFile)
	}
	log.Printf("console key %s (fingerprint %s)", g.PublicKey(), link.Fingerprint(g.Key.Public().(ed25519.PublicKey)))

	mux := http.NewServeMux()
	mux.Handle("/v1", g)
	local := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			host, _, _ := net.SplitHostPort(r.RemoteAddr)
			if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
				http.Error(w, "control is local only", http.StatusForbidden)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("GET /servers", local(func(w http.ResponseWriter, r *http.Request) {
		var out []map[string]any
		for _, s := range g.Servers() {
			k, _ := link.ParseKey(s.Key)
			rep, rights := s.Last()
			out = append(out, map[string]any{"key": s.Key, "fingerprint": link.Fingerprint(k), "laavud": s.Auth.Laavud,
				"laavu": s.Auth.Laavu, "rights": rights, "last_report": rep})
		}
		writeJSON(w, out)
	}))
	mux.HandleFunc("POST /code", local(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, g.Code())
	}))
	mux.HandleFunc("POST /confirm", local(func(w http.ResponseWriter, r *http.Request) {
		if err := g.Confirm(r.URL.Query().Get("code")); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
		}
	}))
	mux.HandleFunc("POST /job", local(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Server string          `json:"server"`
			Op     string          `json:"op"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s, err := pick(g, req.Server)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		j := s.NewJob(req.Op, nil)
		j.Params = req.Params
		res, err := s.Run(j, 2*time.Hour)
		if err != nil {
			http.Error(w, err.Error(), http.StatusGatewayTimeout)
			return
		}
		writeJSON(w, res)
	}))
	log.Printf("listening on %s", *listen)
	log.Fatal(http.ListenAndServe(*listen, mux))
}

func pick(g *linktest.Gateway, key string) (*linktest.Server, error) {
	if key != "" {
		return g.Server(key, time.Second)
	}
	all := g.Servers()
	if len(all) != 1 {
		return nil, fmt.Errorf("%d servers connected, say which one", len(all))
	}
	return all[0], nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func loadKey(path string) ed25519.PrivateKey {
	if b, err := os.ReadFile(path); err == nil {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(raw) != ed25519.PrivateKeySize {
			log.Fatalf("%s is not a key", path)
		}
		return raw
	}
	_, k, _ := ed25519.GenerateKey(rand.Reader)
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(k)+"\n"), 0o600); err != nil {
		log.Fatal(err)
	}
	return k
}
