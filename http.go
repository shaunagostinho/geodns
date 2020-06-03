package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/shaunagostinho/geodns/monitor"
	"github.com/shaunagostinho/geodns/zones"
)

type httpServer struct {
	mux        *http.ServeMux
	zones      *zones.MuxManager
	serverInfo *monitor.ServerInfo
}

type rate struct {
	Name    string
	Count   int64
	Metrics zones.ZoneMetrics
}
type rates []*rate

func (s rates) Len() int      { return len(s) }
func (s rates) Swap(i, j int) { s[i], s[j] = s[j], s[i] }

type ratesByCount struct{ rates }

func (s ratesByCount) Less(i, j int) bool {
	ic := s.rates[i].Count
	jc := s.rates[j].Count
	if ic == jc {
		return s.rates[i].Name < s.rates[j].Name
	}
	return ic > jc
}

func topParam(req *http.Request, def int) int {
	req.ParseForm()

	topOption := def
	topParam := req.Form["top"]

	if len(topParam) > 0 {
		var err error
		topOption, err = strconv.Atoi(topParam[0])
		if err != nil {
			topOption = def
		}
	}

	return topOption
}

func NewHTTPServer(mm *zones.MuxManager, serverInfo *monitor.ServerInfo) *httpServer {

	hs := &httpServer{
		zones:      mm,
		mux:        &http.ServeMux{},
		serverInfo: serverInfo,
	}
	hs.mux.HandleFunc("/", hs.mainServer)
	hs.mux.Handle("/metrics", promhttp.Handler())

	return hs
}

func (hs *httpServer) Mux() *http.ServeMux {
	return hs.mux
}

func (hs *httpServer) Run(listen string) {
	log.Println("Starting HTTP interface on", listen)
	log.Fatal(http.ListenAndServe(listen, &basicauth{h: hs.mux}))
}

func (hs *httpServer) mainServer(w http.ResponseWriter, req *http.Request) {
	if req.RequestURI != "/version" {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(200)
	io.WriteString(w, `GeoDNS `+hs.serverInfo.Version+`\n`)
}

type basicauth struct {
	h http.Handler
}

func (b *basicauth) ServeHTTP(w http.ResponseWriter, r *http.Request) {

	cfgMutex.RLock()
	user := Config.HTTP.User
	password := Config.HTTP.Password
	cfgMutex.RUnlock()

	if len(user) == 0 {
		b.h.ServeHTTP(w, r)
		return
	}

	ruser, rpass, ok := r.BasicAuth()
	if ok {
		if ruser == user && rpass == password {
			b.h.ServeHTTP(w, r)
			return
		}
	}

	w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Basic realm=%q`, "GeoDNS Status"))
	http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
	return
}
